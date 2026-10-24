package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	mirror           *Mirror
	calls, downloads atomic.Int32
	corrupt, down    atomic.Bool
	hold             chan struct{}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	zip := []byte("verified provider zip")
	sum := sha256.Sum256(zip)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.down.Load() {
			http.Error(w, "outage", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/.well-known/terraform.json":
			io.WriteString(w, `{"providers.v1":"/v1/providers/"}`)
		case "/v1/providers/acme/test/versions":
			io.WriteString(w, `{"versions":[{"version":"1.2.3","platforms":[{"os":"linux","arch":"amd64"},{"os":"linux","arch":"arm64"},{"os":"darwin","arch":"arm64"}]}]}`)
		case "/v1/providers/acme/test/1.2.3/download/linux/amd64", "/v1/providers/acme/test/1.2.3/download/linux/arm64":
			fmt.Fprintf(w, `{"download_url":"https://releases.example/provider.zip","shasum":"%s"}`, hex.EncodeToString(sum[:]))
		case "/provider.zip":
			f.downloads.Add(1)
			if f.hold != nil {
				<-f.hold
			}
			if f.corrupt.Load() {
				io.WriteString(w, "unverified bytes")
			} else {
				w.Write(zip)
			}
		default:
			http.NotFound(w, r)
		}
	})
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	var err error
	f.mirror, err = New(Config{CacheDir: t.TempDir(), Client: client, IndexTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const provider = "/providers/registry.opentofu.org/acme/test/"

func request(m *Mirror, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}
func TestMissHitAndImmutableCache(t *testing.T) {
	f := setup(t)
	for _, file := range []string{"index.json", "1.2.3.json", "1.2.3_linux_amd64.zip"} {
		first := request(f.mirror, provider+file)
		if first.Code != 200 {
			t.Fatalf("%s: %d %s", file, first.Code, first.Body.String())
		}
		before := f.calls.Load()
		second := request(f.mirror, provider+file)
		if second.Code != 200 || second.Body.String() != first.Body.String() || f.calls.Load() != before {
			t.Fatalf("cache miss: %s", file)
		}
	}
	var v version
	if err := json.Unmarshal(request(f.mirror, provider+"1.2.3.json").Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	a := v.Archives["linux_amd64"]
	if a.URL != "1.2.3_linux_amd64.zip" || len(a.Hashes) != 1 || !strings.HasPrefix(a.Hashes[0], "zh:") || len(v.Archives) != 2 {
		t.Fatalf("invalid version: %+v", v)
	}
	// Metadata and ZIPs survive a process restart and need no upstream calls.
	next, err := New(f.mirror.config)
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	if request(next, provider+"1.2.3_linux_amd64.zip").Code != 200 || request(next, provider+"1.2.3.json").Code != 200 || f.calls.Load() != before {
		t.Fatal("persistent cache missed")
	}
}
func TestChecksumMismatchNotCached(t *testing.T) {
	f := setup(t)
	f.corrupt.Store(true)
	path := provider + "1.2.3_linux_amd64.zip"
	for i := 0; i < 2; i++ {
		w := request(f.mirror, path)
		if w.Code != 502 || strings.Contains(w.Body.String(), "unverified bytes") {
			t.Fatal("unverified bytes served")
		}
	}
	if f.downloads.Load() != 2 {
		t.Fatal("bad archive cached")
	}
	matches, err := filepath.Glob(filepath.Join(f.mirror.config.CacheDir, "registry.opentofu.org/acme/test/.*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("temporary archive retained", matches, err)
	}
	f.corrupt.Store(false)
	if request(f.mirror, path).Code != 200 {
		t.Fatal("retry failed")
	}
}
func TestValidation(t *testing.T) {
	f := setup(t)
	for _, path := range []string{
		"/providers/evil.example/acme/test/index.json",
		"/providers/registry.opentofu.org/../test/index.json",
		"/providers/registry.opentofu.org/acme/../index.json",
		"/providers/registry.opentofu.org/acme%2ftest/test/index.json",
		provider + "1.2.3/extra.json", provider + "../secret", provider + "1.2.3_darwin_arm64.zip", provider + strings.Repeat("1", 129) + ".json", provider + "latest.json", provider + "1.2.3_linux_amd64.zip/", provider + "%69ndex.json",
	} {
		if w := request(f.mirror, path); w.Code != 404 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
	if request(f.mirror, "/healthz").Code != 200 {
		t.Fatal("health probe failed")
	}
}
func TestConcurrentArchive(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			w := request(f.mirror, provider+"1.2.3_linux_arm64.zip")
			if w.Code != 200 || w.Body.String() != "verified provider zip" {
				t.Errorf("invalid concurrent response: %d", w.Code)
			}
		})
	}
	wg.Wait()
	if f.downloads.Load() != 1 {
		t.Fatalf("%d downloads", f.downloads.Load())
	}
}
func TestIndexExpires(t *testing.T) {
	f := setup(t)
	if request(f.mirror, provider+"index.json").Code != 200 {
		t.Fatal("miss")
	}
	path := filepath.Join(f.mirror.config.CacheDir, "registry.opentofu.org/acme/test/index.json")
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	if request(f.mirror, provider+"index.json").Code != 200 || f.calls.Load() == before {
		t.Fatal("expired index not refreshed")
	}
}
func TestStaleIndexDuringUpstreamOutage(t *testing.T) {
	f := setup(t)
	if request(f.mirror, provider+"index.json").Code != 200 || request(f.mirror, provider+"1.2.3.json").Code != 200 {
		t.Fatal("warm-up failed")
	}
	path := filepath.Join(f.mirror.config.CacheDir, "registry.opentofu.org/acme/test/index.json")
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	f.down.Store(true)
	if w := request(f.mirror, provider+"index.json"); w.Code != 200 || !strings.Contains(w.Body.String(), "1.2.3") {
		t.Fatal("expired index not served during an outage", w.Code)
	}
	if request(f.mirror, provider+"1.2.3.json").Code != 200 {
		t.Fatal("cached version metadata not served during an outage")
	}
}
func TestSlowDownloadDoesNotBlockOtherLookups(t *testing.T) {
	f := setup(t)
	f.hold = make(chan struct{})
	if request(f.mirror, provider+"1.2.3.json").Code != 200 {
		t.Fatal("warm-up failed")
	}
	done := make(chan int)
	go func() { done <- request(f.mirror, provider+"1.2.3_linux_amd64.zip").Code }()
	for f.downloads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	lookup := make(chan int)
	go func() { lookup <- request(f.mirror, provider+"index.json").Code }()
	select {
	case code := <-lookup:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("index lookup blocked by a running download")
	}
	close(f.hold)
	if code := <-done; code != 200 {
		t.Fatal(code)
	}
}
