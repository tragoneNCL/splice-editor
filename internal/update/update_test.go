package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v1.0.0", "v1.0.1", true}, {"v1.2.0", "v1.10.0", true}, {"v2.0.0", "v1.9.9", false},
		{"v1.0.0", "v1.0.0", false}, {"dev", "v1.0.0", false}, {"v1.0.0", "garbage", false},
		{"v1.0", "v1.0.1", true}, {"1.0.0", "v1.0.1-rc1", true},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "splice-v2.0.0-linux-amd64/README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "splice-v2.0.0-linux-amd64/" + name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("splice-v2.0.0-windows-amd64/" + name)
	w.Write(content)
	zw.Close()
	return buf.Bytes()
}

// fakeGitHub serves a release whose asset is archive, with checksums listing sum.
func fakeGitHub(t *testing.T, assetName string, archive []byte, sumFor []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(sumFor)
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v2.0.0","body":"shiny","assets":[
			{"name":%q,"browser_download_url":"%s/dl/asset"},
			{"name":"checksums.txt","browser_download_url":"%s/dl/sums"}]}`, assetName, base, base)
	})
	mux.HandleFunc("/dl/asset", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func TestApplyReplacesTheExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "splice")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	archive := tarGz(t, "splice", []byte("NEW BINARY"))
	srv := fakeGitHub(t, "splice-v2.0.0-linux-amd64.tar.gz", archive, archive)

	u := &Updater{Repo: "o/r", Current: "v1.0.0", APIBase: srv.URL, Exe: exe, GOOS: "linux", GOARCH: "amd64"}
	rel, newer, err := u.Latest(context.Background())
	if err != nil || !newer || rel.Tag != "v2.0.0" || rel.Notes != "shiny" {
		t.Fatalf("latest: %+v newer=%v err=%v", rel, newer, err)
	}
	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "NEW BINARY" {
		t.Errorf("exe = %q", got)
	}
	if st, _ := os.Stat(exe); st.Mode()&0o111 == 0 {
		t.Error("replaced binary must stay executable")
	}
	if _, err := os.Stat(exe + ".old"); err == nil {
		t.Error("old binary should be cleaned up on unix")
	}
}

func TestApplyRefusesAChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "splice")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	archive := tarGz(t, "splice", []byte("TAMPERED"))
	srv := fakeGitHub(t, "splice-v2.0.0-linux-amd64.tar.gz", archive, []byte("something else"))

	u := &Updater{Repo: "o/r", Current: "v1.0.0", APIBase: srv.URL, Exe: exe, GOOS: "linux", GOARCH: "amd64"}
	rel, _, _ := u.Latest(context.Background())
	err := u.Apply(context.Background(), rel)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("a failed update must leave the working app alone, got %q", got)
	}
}

func TestWindowsZipAndDevBuildsAndMissingAssets(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "splice.exe")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	archive := zipOf(t, "splice.exe", []byte("WIN NEW"))
	srv := fakeGitHub(t, "splice-v2.0.0-windows-amd64.zip", archive, archive)

	u := &Updater{Repo: "o/r", Current: "v1.0.0", APIBase: srv.URL, Exe: exe, GOOS: "windows", GOARCH: "amd64"}
	rel, newer, err := u.Latest(context.Background())
	if err != nil || !newer {
		t.Fatalf("latest: %v %v", newer, err)
	}
	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "WIN NEW" {
		t.Errorf("exe = %q", got)
	}

	dev := &Updater{Repo: "o/r", Current: "dev", APIBase: srv.URL, Exe: exe, GOOS: "windows", GOARCH: "amd64"}
	if _, newer, _ := dev.Latest(context.Background()); newer {
		t.Error("development builds must never self-update")
	}
	if err := dev.Apply(context.Background(), rel); err == nil {
		t.Error("development builds must refuse Apply")
	}

	other := &Updater{Repo: "o/r", Current: "v1.0.0", APIBase: srv.URL, Exe: exe, GOOS: "linux", GOARCH: "arm64"}
	r2, _, _ := other.Latest(context.Background())
	if err := other.Apply(context.Background(), r2); err == nil || !strings.Contains(err.Error(), "no build") {
		t.Errorf("missing platform asset: %v", err)
	}
}
