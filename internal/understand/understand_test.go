package understand

import (
	"os/exec"
	"path/filepath"
	"testing"

	"filmeditor/internal/synth"
)

func TestAnalyzeFindsSceneCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two.mp4")
	// 2s red then 2s blue: one hard cut at 2.0s.
	b, err := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=red:s=320x240:r=30:d=2",
		"-f", "lavfi", "-i", "color=c=blue:s=320x240:r=30:d=2",
		"-filter_complex", "[0][1]concat=n=2:v=1[v]", "-map", "[v]",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("make clip: %v\n%s", err, b)
	}
	c, err := Analyze(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Shots) != 2 {
		t.Fatalf("got %d shots, want 2: %+v", len(c.Shots), c.Shots)
	}
	if got := c.Shots[0].End; got < 1.9 || got > 2.1 {
		t.Errorf("cut at %.2f, want ~2.0", got)
	}
	for _, s := range c.Shots {
		if s.Score <= 0 || s.Score > 1 {
			t.Errorf("score %.3f out of range", s.Score)
		}
	}
}

// A clip that opens on (or ends with) bare wall must not offer that footage,
// as long as the clip has real detail somewhere else.
func TestBlankStretchesAreRemovedButPlainClipsAreKept(t *testing.T) {
	dir := t.TempDir()
	rich := filepath.Join(dir, "rich.mp4")
	if err := synth.WriteClip(rich, synth.Landscape, synth.Options{Seconds: 2, Texture: true}); err != nil {
		t.Fatal(err)
	}
	wall := filepath.Join(dir, "wall.mp4")
	if err := synth.WriteClip(wall, synth.Wall, synth.Options{Seconds: 3}); err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(dir, "mixed.mp4")
	if err := synth.Concat(mixed, wall, rich); err != nil { // 3s of bare wall, then 2s of detail
		t.Fatal(err)
	}

	c, err := Analyze(mixed)
	if err != nil {
		t.Fatal(err)
	}
	var usable, first float64 = 0, -1
	for _, s := range c.Shots {
		usable += s.Length()
		if first < 0 || s.Start < first {
			first = s.Start
		}
	}
	if usable < 1.0 || usable > 2.6 {
		t.Errorf("usable = %.1fs, want only the ~2s of detailed footage: %+v", usable, c.Shots)
	}
	if first < 2.5 {
		t.Errorf("first usable moment at %.1fs, but the first 3s are a bare wall", first)
	}

	// The same bare footage on its own is kept: nothing in it has detail, so
	// nothing is "dead" relative to the rest.
	plain, err := Analyze(wall)
	if err != nil {
		t.Fatal(err)
	}
	var kept float64
	for _, s := range plain.Shots {
		kept += s.Length()
	}
	if kept < 2.5 {
		t.Errorf("a uniformly plain clip must be kept whole, kept %.1fs of 3s", kept)
	}
}
