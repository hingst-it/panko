// Package mirror implements a verified pull-through provider network mirror.
package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	CacheDir         string
	Hosts, Platforms []string
	IndexTTL         time.Duration
	Client           *http.Client
}
type Mirror struct {
	config Config
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
}

// lock serializes work per cached file, so a large download does not block other providers or lookups.
func (m *Mirror) lock(key string) func() {
	m.mu.Lock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

type platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}
type release struct {
	Version   string     `json:"version"`
	Platforms []platform `json:"platforms"`
}
type download struct {
	URL string `json:"download_url"`
	Sum string `json:"shasum"`
}
type archive struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes"`
}
type version struct {
	Archives map[string]archive `json:"archives"`
}
type metadata struct {
	Public    version
	Downloads map[string]download
}

var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var hostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func New(c Config) (*Mirror, error) {
	if c.CacheDir == "" {
		return nil, fmt.Errorf("cache directory required")
	}
	if c.Hosts == nil {
		c.Hosts = []string{"registry.opentofu.org", "registry.terraform.io"}
	}
	if c.Platforms == nil {
		c.Platforms = []string{"linux_amd64", "linux_arm64"}
	}
	if c.IndexTTL <= 0 {
		c.IndexTTL = 5 * time.Minute
	}
	if c.IndexTTL > time.Hour {
		c.IndexTTL = time.Hour
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	client := *c.Client
	client.Timeout = 2 * time.Minute
	redirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return fmt.Errorf("invalid upstream redirect")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many upstream redirects")
		}
		if redirect != nil {
			return redirect(req, via)
		}
		return nil
	}
	c.Client = &client
	if err := os.MkdirAll(c.CacheDir, 0755); err != nil {
		return nil, err
	}
	return &Mirror{config: c, locks: map[string]*sync.Mutex{}}, nil
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func validVersion(v string) bool { return len(v) <= 128 && semver.MatchString(v) }
func (m *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/providers/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/providers/") || len(p) != 4 || len(p[0]) > 253 || !hostname.MatchString(p[0]) || !contains(m.config.Hosts, p[0]) || !segment.MatchString(p[1]) || !segment.MatchString(p[2]) || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	file := p[3]
	isIndex := file == "index.json"
	v := strings.TrimSuffix(file, ".json")
	isVersion := strings.HasSuffix(file, ".json") && validVersion(v)
	archiveVersion, plat, isArchive := parseArchive(file)
	if !isIndex && !isVersion && !isArchive {
		http.NotFound(w, r)
		return
	}
	if isArchive && !m.allowedPlatform(plat) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	dir := filepath.Join(m.config.CacheDir, p[0], p[1], p[2])
	// One writer per cached file coalesces concurrent misses; ZIP responses stream outside the lock.
	unlock := m.lock(filepath.Join(dir, file))
	err := os.MkdirAll(dir, 0755)
	var data []byte
	var archivePath string
	if err == nil {
		switch {
		case isIndex:
			data, err = m.index(ctx, p, dir)
		case isVersion:
			var meta metadata
			meta, err = m.version(ctx, p, dir, v)
			if err == nil {
				data, err = json.Marshal(meta.Public)
			}
		case isArchive:
			archivePath = filepath.Join(dir, file)
			if _, err = os.Stat(archivePath); os.IsNotExist(err) {
				var meta metadata
				meta, err = m.version(ctx, p, dir, archiveVersion)
				if err == nil {
					d, ok := meta.Downloads[plat]
					if !ok {
						err = fmt.Errorf("platform unavailable")
					} else {
						err = m.fetchArchive(ctx, archivePath, d)
					}
				}
			}
		}
	}
	unlock()
	if err != nil {
		http.Error(w, "provider unavailable", http.StatusBadGateway)
		return
	}
	if isArchive {
		http.ServeFile(w, r, archivePath)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
func parseArchive(file string) (string, string, bool) {
	parts := strings.Split(strings.TrimSuffix(file, ".zip"), "_")
	if !strings.HasSuffix(file, ".zip") || len(parts) != 3 || !validVersion(parts[0]) || !segment.MatchString(parts[1]) || !segment.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[0], parts[1] + "_" + parts[2], true
}
func (m *Mirror) allowedPlatform(p string) bool {
	return len(m.config.Platforms) == 0 || contains(m.config.Platforms, p)
}
func (m *Mirror) get(ctx context.Context, address string) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.config.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return resp, nil
}
func (m *Mirror) json(ctx context.Context, address string, out any) error {
	resp, err := m.get(ctx, address)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}
func (m *Mirror) base(ctx context.Context, p []string) (string, error) {
	var discovery map[string]string
	origin := "https://" + p[0] + "/.well-known/terraform.json"
	if err := m.json(ctx, origin, &discovery); err != nil {
		return "", err
	}
	u, err := url.Parse(discovery["providers.v1"])
	if err != nil || discovery["providers.v1"] == "" {
		return "", fmt.Errorf("missing provider service")
	}
	root, _ := url.Parse(origin)
	return strings.TrimRight(root.ResolveReference(u).String(), "/") + "/" + p[1] + "/" + p[2], nil
}
func (m *Mirror) index(ctx context.Context, p []string, dir string) ([]byte, error) {
	path := filepath.Join(dir, "index.json")
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < m.config.IndexTTL {
		return os.ReadFile(path)
	}
	// During an upstream outage a stale index still serves every cached version.
	stale := func(err error) ([]byte, error) {
		if data, readErr := os.ReadFile(path); readErr == nil {
			return data, nil
		}
		return nil, err
	}
	base, err := m.base(ctx, p)
	if err != nil {
		return stale(err)
	}
	var upstream struct {
		Versions []release `json:"versions"`
	}
	if err = m.json(ctx, base+"/versions", &upstream); err != nil {
		return stale(err)
	}
	versions := map[string]any{}
	for _, v := range upstream.Versions {
		if validVersion(v.Version) {
			versions[v.Version] = struct{}{}
		}
	}
	data, err := json.Marshal(map[string]any{"versions": versions})
	if err != nil {
		return nil, err
	}
	return data, atomicWrite(path, strings.NewReader(string(data)))
}
func (m *Mirror) version(ctx context.Context, p []string, dir, v string) (metadata, error) {
	var meta metadata
	path := filepath.Join(dir, v+".metadata")
	if data, err := os.ReadFile(path); err == nil {
		err = json.Unmarshal(data, &meta)
		return meta, err
	} else if !os.IsNotExist(err) {
		return meta, err
	}
	base, err := m.base(ctx, p)
	if err != nil {
		return meta, err
	}
	var upstream struct {
		Versions []release `json:"versions"`
	}
	if err = m.json(ctx, base+"/versions", &upstream); err != nil {
		return meta, err
	}
	meta = metadata{Public: version{Archives: map[string]archive{}}, Downloads: map[string]download{}}
	found := false
	for _, rel := range upstream.Versions {
		if rel.Version != v {
			continue
		}
		found = true
		for _, p := range rel.Platforms {
			key := p.OS + "_" + p.Arch
			if !segment.MatchString(p.OS) || !segment.MatchString(p.Arch) || !m.allowedPlatform(key) {
				continue
			}
			var d download
			if err = m.json(ctx, base+"/"+v+"/download/"+p.OS+"/"+p.Arch, &d); err != nil {
				return meta, err
			}
			sum, err := hex.DecodeString(d.Sum)
			if err != nil || len(sum) != sha256.Size {
				return meta, fmt.Errorf("invalid checksum")
			}
			d.Sum = strings.ToLower(d.Sum)
			meta.Downloads[key] = d
			meta.Public.Archives[key] = archive{URL: v + "_" + key + ".zip", Hashes: []string{"zh:" + d.Sum}}
		}
	}
	if !found {
		return meta, fmt.Errorf("version unavailable")
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return meta, err
	}
	return meta, atomicWrite(path, strings.NewReader(string(data)))
}
func atomicWrite(path string, reader io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, reader)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (m *Mirror) fetchArchive(ctx context.Context, path string, d download) error {
	resp, err := m.get(ctx, d.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), resp.Body)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != d.Sum {
		return fmt.Errorf("archive checksum mismatch")
	}
	return os.Rename(f.Name(), path)
}
