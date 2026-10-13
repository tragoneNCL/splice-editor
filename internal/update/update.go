// Package update lets the locally run app replace itself with a newer release
// from GitHub: find the latest release, download the build for this system,
// verify its SHA-256 against the release's checksums.txt, then swap the
// executable. Shipping an update is just pushing a version tag.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxDownload = 200 << 20

type Release struct {
	Tag         string `json:"tag"`
	Notes       string `json:"notes,omitempty"`
	AssetName   string `json:"-"`
	AssetURL    string `json:"-"`
	ChecksumURL string `json:"-"`
}

type Updater struct {
	Repo    string       // "owner/name"
	Current string       // e.g. "v1.2.0"; "dev" disables updating
	APIBase string       // default https://api.github.com
	Client  *http.Client // default: 60s timeout
	Exe     string       // executable to replace (default: this process)
	GOOS    string       // default runtime.GOOS (overridable for tests)
	GOARCH  string
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

// Enabled is false for development builds.
func (u *Updater) Enabled() bool {
	return u.Repo != "" && u.Current != "" && u.Current != "dev"
}

// Latest returns the newest release and whether it is newer than Current.
func (u *Updater) Latest(ctx context.Context) (Release, bool, error) {
	base := u.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases/latest", base, u.Repo), nil)
	if err != nil {
		return Release{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client().Do(req)
	if err != nil {
		return Release{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, false, nil // no releases yet
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, false, fmt.Errorf("release lookup: http %d", resp.StatusCode)
	}
	var gh struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gh); err != nil {
		return Release{}, false, err
	}
	rel := Release{Tag: gh.Tag, Notes: truncate(gh.Body, 600)}
	want := fmt.Sprintf("-%s-%s.", u.goos(), u.goarch())
	for _, a := range gh.Assets {
		switch {
		case strings.HasPrefix(a.Name, "splice-") && strings.Contains(a.Name, want):
			rel.AssetName, rel.AssetURL = a.Name, a.URL
		case a.Name == "checksums.txt":
			rel.ChecksumURL = a.URL
		}
	}
	return rel, u.Enabled() && Newer(u.Current, rel.Tag), nil
}

// Newer reports whether latest is a higher version than current.
func Newer(current, latest string) bool {
	c, l := parse(current), parse(latest)
	if c == nil || l == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 3)
	if len(parts) == 0 || len(parts) > 3 {
		return nil
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// Apply downloads rel, verifies it, and replaces the executable. The caller
// restarts the process afterwards.
func (u *Updater) Apply(ctx context.Context, rel Release) error {
	if !u.Enabled() {
		return errors.New("this is a development build; updates are disabled")
	}
	if rel.AssetURL == "" {
		return fmt.Errorf("release %s has no build for %s/%s", rel.Tag, u.goos(), u.goarch())
	}
	if rel.ChecksumURL == "" {
		return fmt.Errorf("release %s has no checksums.txt; refusing to install unverified code", rel.Tag)
	}
	archive, err := u.fetch(ctx, rel.AssetURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	sums, err := u.fetch(ctx, rel.ChecksumURL)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	if err := verify(archive, rel.AssetName, sums); err != nil {
		return err
	}
	bin, err := extract(archive, rel.AssetName, u.goos())
	if err != nil {
		return err
	}
	exe := u.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	return replace(exe, bin, u.goos())
}

func (u *Updater) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDownload {
		return nil, errors.New("download too large")
	}
	return b, nil
}

// verify checks data against the line for name in a sha256sum-style file.
func verify(data []byte, name string, sums []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if strings.EqualFold(f[0], got) {
				return nil
			}
			return errors.New("the download did not match its checksum; not installing")
		}
	}
	return fmt.Errorf("no checksum listed for %s; not installing", name)
}

// extract pulls the app binary out of a .tar.gz or .zip release archive.
func extract(archive []byte, name, goos string) ([]byte, error) {
	want := "splice"
	if goos == "windows" {
		want = "splice.exe"
	}
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, errors.New("the release archive has no app binary")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the release archive has no app binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// replace swaps exe for bin. A running executable can be renamed on every
// platform, so the old file is moved aside first; only after the new one is
// in place is the old one removed (on Windows it is removed at next start).
func replace(exe string, bin []byte, goos string) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".splice-update-*")
	if err != nil {
		return fmt.Errorf("can't write next to the app (%w); install it somewhere you can write to", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Rename(old, exe) // put the working version back
		os.Remove(tmpName)
		return err
	}
	if goos != "windows" {
		os.Remove(old)
	}
	return nil
}

// CleanOld removes a leftover ".old" binary from a previous update.
func CleanOld() {
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
