package devlab

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// K3dVersion is installed privately when no supported k3d is found; supported are 5.6 to 5.x.
const K3dVersion = "5.9.0"

// k3dChecksums are the SHA-256 sums of the k3d v5.9.0 release binaries, from its checksums.txt.
var k3dChecksums = map[string]string{
	"darwin-amd64": "b4aabc37534f95b9c764e7823f2df923f50d57600837aa60a06266cce47db732",
	"darwin-arm64": "fe106541d5d0a3f18debcd4d432a16f8c0ce3e6ddc06f8fbb6f696a122313e00",
	"linux-amd64":  "06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0",
	"linux-arm64":  "03cde5cf23e6e8e67de5a039ecf26e5b85aca82fba3e5d13dadf904cd218a250",
}

var k3dVersionLine = regexp.MustCompile(`k3d version v(\d+)\.(\d+)\.(\d+)`)

// supported accepts k3d 5.6 and newer 5.x releases.
func supported(output string) (string, bool) {
	m := k3dVersionLine.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return strings.TrimPrefix(m[0], "k3d version "), major == 5 && minor >= 6
}

func (o Options) privateK3d() string { return filepath.Join(o.Home, "bin", "k3d-"+K3dVersion) }

// findK3d returns a supported k3d: the private copy first, then the one on PATH.
func (o Options) findK3d(ctx context.Context) (string, error) {
	candidates := []string{o.privateK3d()}
	if path, err := exec.LookPath("k3d"); err == nil {
		candidates = append(candidates, path)
	}
	found := []string{}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		out, err := o.run(ctx, candidate, "version")
		version, ok := supported(out)
		if err == nil && ok {
			return candidate, nil
		}
		if version != "" {
			found = append(found, candidate+" ("+version+")")
		}
	}
	if len(found) > 0 {
		return "", fmt.Errorf("no supported k3d (5.6 or newer 5.x); found %s", strings.Join(found, ", "))
	}
	return "", fmt.Errorf("k3d is not installed")
}

// k3d finds a supported k3d or, with consent, installs the pinned release privately.
func (o Options) k3d(ctx context.Context) (string, error) {
	path, err := o.findK3d(ctx)
	if err == nil {
		out, _ := o.run(ctx, path, "version")
		version, _ := supported(out)
		fmt.Fprintf(o.Out, "ok k3d %s (%s)\n", version, path)
		return path, nil
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	sum, known := k3dChecksums[platform]
	if !known {
		return "", fmt.Errorf("%v; install k3d 5.6 or newer for %s yourself", err, platform)
	}
	target := o.privateK3d()
	if !o.InstallK3d {
		fmt.Fprintf(o.Out, "%v. Install k3d v%s privately to %s (checksum verified, PATH unchanged)? [y/N] ", err, K3dVersion, target)
		answer, _ := bufio.NewReader(o.In).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return "", fmt.Errorf("k3d is required; install it or rerun with --install-k3d")
		}
	}
	url := fmt.Sprintf("https://github.com/k3d-io/k3d/releases/download/v%s/k3d-%s", K3dVersion, platform)
	if err := download(ctx, url, sum, target); err != nil {
		return "", err
	}
	fmt.Fprintf(o.Out, "ok k3d v%s installed to %s (SHA-256 %s)\n", K3dVersion, target, sum[:16]+"...")
	return target, nil
}

// download fetches url into target only if its SHA-256 matches; a mismatch leaves nothing behind.
func download(ctx context.Context, url, sum, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot download k3d: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("cannot download k3d: status %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".k3d-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, 256<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return fmt.Errorf("k3d checksum mismatch: expected %s, got %s; nothing was installed", sum, got)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
