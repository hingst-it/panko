package devlab

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTools puts docker and k3d scripts on PATH. They log their calls and report the clusters
// in $FAKE_CLUSTERS and the owner label in $FAKE_OWNER.
func fakeTools(t *testing.T, clusters, owner string) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "calls")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s %s\\n' \""+name+"\" \"$*\" >> \"$FAKE_LOG\"\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("k3d", `printf 'KUBECONFIG=%s\n' "$KUBECONFIG" >> "$FAKE_LOG"
case "$1 $2" in
  "version ") echo "k3d version v5.9.0" ;;
  "cluster list") printf '%s' "$FAKE_CLUSTERS" ;;
esac
`)
	write("docker", `case "$1" in
  info) echo 29.0.0 ;;
  inspect) echo "$FAKE_OWNER" ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_CLUSTERS", clusters)
	t.Setenv("FAKE_OWNER", owner)
	return Options{Name: "lab", Home: filepath.Join(dir, "home"), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, log
}

func calls(t *testing.T, log string) string {
	data, _ := os.ReadFile(log)
	return string(data)
}

const oneCluster = `[{"name":"lab","serversRunning":1,"serversCount":1}]`

func TestUpLeavesForeignClustersUnchanged(t *testing.T) {
	o, log := fakeTools(t, oneCluster, "")
	err := Up(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "not created by panko dev") {
		t.Fatalf("a foreign cluster was accepted: %v", err)
	}
	// A lab state that does not match the cluster's owner label is refused as well.
	o2, log2 := fakeTools(t, oneCluster, "someone-else")
	if err := o2.save(&Lab{Format: labFormat, Name: "lab", ID: "mine", Port: 18081}); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), o2); err == nil || !strings.Contains(err.Error(), "owner label") {
		t.Fatalf("a cluster with another owner was accepted: %v", err)
	}
	for _, c := range []string{calls(t, log), calls(t, log2)} {
		if strings.Contains(c, "cluster create") || strings.Contains(c, "cluster delete") || strings.Contains(c, "kubeconfig get") {
			t.Fatalf("a foreign cluster was changed: %s", c)
		}
	}
}

func TestDownNeedsConfirmationAndOwnership(t *testing.T) {
	o, log := fakeTools(t, oneCluster, "mine")
	if err := o.save(&Lab{Format: labFormat, Name: "lab", ID: "mine", Port: 18081}); err != nil {
		t.Fatal(err)
	}
	o.In = strings.NewReader("yes\n")
	if err := Down(context.Background(), o); err == nil {
		t.Fatal("down without typing the lab name deleted the lab")
	}
	if strings.Contains(calls(t, log), "cluster delete") {
		t.Fatal("an unconfirmed down deleted the cluster")
	}
	o.In = strings.NewReader("lab\n")
	if err := Down(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls(t, log), "k3d cluster delete lab") {
		t.Fatalf("the cluster was not deleted: %s", calls(t, log))
	}
	// k3d never sees the user's kubeconfig.
	for _, line := range strings.Split(calls(t, log), "\n") {
		if strings.HasPrefix(line, "KUBECONFIG=") && !strings.HasSuffix(line, filepath.Join("home", "k3d-kubeconfig")) {
			t.Fatalf("k3d ran with %s", line)
		}
	}
	if _, err := os.Stat(o.dir()); !os.IsNotExist(err) {
		t.Fatal("the lab state stayed")
	}
	// Another owner's cluster of the same name is never deleted, even with --yes.
	o2, log2 := fakeTools(t, oneCluster, "other")
	if err := o2.save(&Lab{Format: labFormat, Name: "lab", ID: "mine"}); err != nil {
		t.Fatal(err)
	}
	o2.Yes = true
	if err := Down(context.Background(), o2); err == nil || strings.Contains(calls(t, log2), "cluster delete") {
		t.Fatal("down deleted a cluster of another owner")
	}
}

func TestStatusPrintsNoSecrets(t *testing.T) {
	o, _ := fakeTools(t, `[{"name":"lab","serversRunning":0,"serversCount":1}]`, "mine")
	if err := o.save(&Lab{Format: labFormat, Name: "lab", ID: "mine", Port: 18081, Created: time.Now(), Completed: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.tokens(), []byte(`{"team-a":"very-secret-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.kubeconfig(), []byte("client-key-data: very-secret-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Status(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	out := o.Out.(*bytes.Buffer).String()
	if strings.Contains(out, "very-secret") || !strings.Contains(out, "0/1 servers running") || !strings.Contains(out, o.tokens()) {
		t.Fatalf("unexpected status: %s", out)
	}
}

func TestSupportedK3dVersions(t *testing.T) {
	for output, want := range map[string]bool{"k3d version v5.9.0\nk3s version v1.35": true, "k3d version v5.6.3": true,
		"k3d version v5.5.2": false, "k3d version v6.0.0": false, "something else": false} {
		if _, ok := supported(output); ok != want {
			t.Errorf("supported(%q) = %v", output, ok)
		}
	}
}

func TestDownloadVerifiesTheChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not k3d")) }))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "bin", "k3d")
	err := download(context.Background(), server.URL, strings.Repeat("0", 64), target)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a wrong binary was accepted: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 0 {
		t.Fatalf("a rejected download left files: %v", entries)
	}
	// sha256("not k3d")
	if err := download(context.Background(), server.URL, "ab8b2a4e1f5d97a5a3f8a1d8d4c1f3f0a9fb3f56c5fca1f3e7d3d6b2a7e0b4c1", target); err == nil {
		t.Fatal("an arbitrary checksum matched")
	}
}

func TestSplitImage(t *testing.T) {
	for ref, want := range map[string][2]string{"panko-server:dev": {"panko-server", "dev"}, "localhost:5000/panko/server:1.2": {"localhost:5000/panko/server", "1.2"}} {
		repository, tag, ok := splitImage(ref)
		if !ok || repository != want[0] || tag != want[1] {
			t.Errorf("splitImage(%q) = %q %q %v", ref, repository, tag, ok)
		}
	}
	for _, ref := range []string{"panko-server", "localhost:5000/panko", "panko@sha256:abc"} {
		if _, _, ok := splitImage(ref); ok {
			t.Errorf("splitImage(%q) accepted", ref)
		}
	}
}
