package inspo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoDownloader means yt-dlp is not installed on this server.
var ErrNoDownloader = errors.New("YouTube links need yt-dlp on the server; upload the reference video file instead")

var youtubeHosts = map[string]bool{
	"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true,
	"music.youtube.com": true, "youtu.be": true,
}

// ValidateYouTubeURL accepts only https links to YouTube. The allowlist is
// what stops this from being used to make the server fetch arbitrary URLs.
func ValidateYouTubeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !youtubeHosts[strings.ToLower(u.Hostname())] {
		return "", fmt.Errorf("that doesn't look like a YouTube link")
	}
	if u.User != nil || u.Port() != "" {
		return "", fmt.Errorf("that doesn't look like a YouTube link")
	}
	return u.String(), nil
}

// FetchYouTube downloads a low-resolution copy into dir and returns its path.
// Low resolution is deliberate: only the cut rhythm is measured, and a small
// file is quick and cheap. Playlists are refused; files over 150 MB are cut.
func FetchYouTube(ctx context.Context, rawURL, dir string) (string, error) {
	link, err := ValidateYouTubeURL(rawURL)
	if err != nil {
		return "", err
	}
	bin, err := exec.LookPath("yt-dlp")
	if err != nil {
		return "", ErrNoDownloader
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	tmpl := filepath.Join(dir, "inspo_dl.%(ext)s")
	out, err := exec.CommandContext(ctx, bin,
		"--no-playlist", "--no-warnings", "--no-progress",
		"-f", "bv*[height<=360]+ba/b[height<=360]/worst",
		"--merge-output-format", "mp4",
		"--max-filesize", "150M",
		"--socket-timeout", "20",
		"--match-filter", "duration < 1800",
		"-o", tmpl,
		"--", link).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("couldn't download that video (it may be private, age-restricted, too long, or blocked): %s",
			lastLine(string(out)))
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "inspo_dl.*"))
	if len(matches) == 0 {
		return "", fmt.Errorf("couldn't download that video")
	}
	return matches[0], nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := lines[len(lines)-1]
	if len(l) > 160 {
		l = l[:160]
	}
	return l
}
