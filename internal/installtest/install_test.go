// Package installtest runs the real install.sh against a fake release server,
// in an isolated home directory.
package installtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func platform(t *testing.T) (string, string) {
	t.Helper()
	os_, arch := runtime.GOOS, runtime.GOARCH
	if os_ != "darwin" && os_ != "linux" {
		t.Skip("install.sh targets macOS and Linux")
	}
	if os_ == "linux" && arch != "amd64" {
		t.Skip("no linux/" + arch + " build")
	}
	return os_, arch
}

func release(t *testing.T, tag, os_, arch, payload string, sumOf string, compact bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(payload)
	tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("splice-%s-%s-%s/splice", tag, os_, arch), Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	name := fmt.Sprintf("splice-%s-%s-%s.tar.gz", tag, os_, arch)
	sum := sha256.Sum256([]byte(sumOf))
	if sumOf == "" {
		sum = sha256.Sum256(archive)
	}

	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{"tag_name": tag, "assets": []map[string]string{
			{"name": name, "browser_download_url": base + "/dl/" + name},
			{"name": "checksums.txt", "browser_download_url": base + "/dl/checksums.txt"},
		}}
		var b []byte
		if compact {
			b, _ = json.Marshal(doc)
		} else {
			b, _ = json.MarshalIndent(doc, "", "  ")
		}
		w.Write(b)
	})
	mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func runInstall(t *testing.T, api string) (string, string, string, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command("sh", "../../install.sh")
	cmd.Env = append(os.Environ(),
		"HOME="+home, "SPLICE_REPO=o/r", "SPLICE_API_BASE="+api,
		"SPLICE_DEST="+filepath.Join(home, "app"), "SPLICE_BIN="+filepath.Join(home, "bin"),
		"SPLICE_SKIP_FFMPEG=1")
	out, err := cmd.CombinedOutput()
	return string(out), filepath.Join(home, "app", "splice"), filepath.Join(home, "bin", "splice"), err
}

func TestInstallsAVerifiedRelease(t *testing.T) {
	os_, arch := platform(t)
	for _, compact := range []bool{false, true} { // GitHub pretty-prints; be robust to compact too
		srv := release(t, "v1.2.3", os_, arch, "#!/bin/sh\necho hello from splice\n", "", compact)
		out, app, link, err := runInstall(t, srv.URL)
		if err != nil {
			t.Fatalf("compact=%v install failed: %v\n%s", compact, err, out)
		}
		if !strings.Contains(out, "Checksum verified") || !strings.Contains(out, "v1.2.3") {
			t.Errorf("output: %s", out)
		}
		if st, err := os.Stat(app); err != nil || st.Mode()&0o111 == 0 {
			t.Fatalf("app not installed or not executable: %v", err)
		}
		got, err := exec.Command(link).Output() // the `splice` command works through the link
		if err != nil || !strings.Contains(string(got), "hello from splice") {
			t.Errorf("running the installed command: %q %v", got, err)
		}
	}
}

func TestRefusesATamperedDownload(t *testing.T) {
	os_, arch := platform(t)
	srv := release(t, "v1.0.0", os_, arch, "EVIL", "the checksum of something else", false)
	out, app, _, err := runInstall(t, srv.URL)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("a tampered download must fail: err=%v out=%s", err, out)
	}
	if _, statErr := os.Stat(app); statErr == nil {
		t.Error("nothing may be installed when the checksum does not match")
	}
}

func TestNoReleaseYetIsAFriendlyError(t *testing.T) {
	platform(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	out, _, _, err := runInstall(t, srv.URL)
	if err == nil || !strings.Contains(out, "published release") {
		t.Errorf("err=%v out=%s", err, out)
	}
}
