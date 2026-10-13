package understand

import (
	"os/exec"
	"path/filepath"
	"testing"
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
