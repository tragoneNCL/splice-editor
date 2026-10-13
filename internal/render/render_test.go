package render

import (
	"filmeditor/internal/synth"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"filmeditor/internal/ingest"
	"filmeditor/internal/schema"
)

// makeClip writes a 320x240 clip whose stored pixels are red on top and blue
// on the bottom, tagged with a display rotation (counter-clockwise degrees).
func makeClip(t *testing.T, dir, name string, ccw int) string {
	t.Helper()
	raw := filepath.Join(dir, "raw_"+name)
	b, err := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=red:s=320x120:r=30:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=320x120:r=30:d=1",
		"-filter_complex", "[0][1]vstack[v]", "-map", "[v]",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", raw).CombinedOutput()
	if err != nil {
		t.Fatalf("make raw clip: %v\n%s", err, b)
	}
	// -display_rotation is an input option, so tag by remuxing.
	path := filepath.Join(dir, name)
	b, err = exec.Command("ffmpeg", "-y", "-v", "error",
		"-display_rotation", strconv.Itoa(ccw), "-i", raw, "-c", "copy", path).CombinedOutput()
	if err != nil {
		t.Fatalf("tag rotation: %v\n%s", err, b)
	}
	return path
}

// pixel returns the RGB value at (x,y) of the first frame of a video.
func pixel(t *testing.T, path string, x, y int) [3]int {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", fmt.Sprintf("format=rgb24,crop=1:1:%d:%d", x, y),
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil || len(b) < 3 {
		t.Fatalf("read pixel: %v", err)
	}
	return [3]int{int(b[0]), int(b[1]), int(b[2])}
}

func isRed(p [3]int) bool  { return p[0] > 180 && p[2] < 90 }
func isBlue(p [3]int) bool { return p[2] > 180 && p[0] < 90 }

func TestOrientationIsBakedIn(t *testing.T) {
	dir := t.TempDir()
	canvas := schema.Canvas{Width: 270, Height: 480, FPS: 30}
	r := schema.Recipe{Canvas: canvas}

	// 180 deg: stored red-top must display blue-top.
	clip := makeClip(t, dir, "up180.mp4", 180)
	info, err := ingest.Probe(clip)
	if err != nil {
		t.Fatal(err)
	}
	if info.Rotation != 180 {
		t.Fatalf("probe rotation = %d, want 180", info.Rotation)
	}
	out := filepath.Join(dir, "out180.mp4")
	edl := schema.EDL{Cuts: []schema.Cut{{Clip: clip, Start: 0, End: 0.5}}}
	if err := Render(edl, r, out); err != nil {
		t.Fatal(err)
	}
	// Letterboxed content is vertically centered: 270 wide -> 202 tall,
	// occupying y in [139,341]. Sample just inside top and bottom.
	if top := pixel(t, out, 135, 150); !isBlue(top) {
		t.Errorf("180: top pixel = %v, want blue (clip was upside down)", top)
	}
	if bot := pixel(t, out, 135, 330); !isRed(bot) {
		t.Errorf("180: bottom pixel = %v, want red", bot)
	}

	// 90 deg CCW: stored top (red) moves to the LEFT, so the 320x240 clip
	// displays as 240x320 with red on the left, blue on the right.
	clip90 := makeClip(t, dir, "ccw90.mp4", 90)
	info, err = ingest.Probe(clip90)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 240 || info.Height != 320 {
		t.Fatalf("probe display size = %dx%d, want 240x320", info.Width, info.Height)
	}
	out90 := filepath.Join(dir, "out90.mp4")
	edl = schema.EDL{Cuts: []schema.Cut{{Clip: clip90, Start: 0, End: 0.5}}}
	if err := Render(edl, r, out90); err != nil {
		t.Fatal(err)
	}
	// 240x320 scaled to fit 270x480 -> 270x360, centered: y in [60,420].
	if l := pixel(t, out90, 20, 240); !isRed(l) {
		t.Errorf("90: left pixel = %v, want red", l)
	}
	if rt := pixel(t, out90, 250, 240); !isBlue(rt) {
		t.Errorf("90: right pixel = %v, want blue", rt)
	}
}

func TestMixedClipsShareCanvas(t *testing.T) {
	dir := t.TempDir()
	r := schema.Recipe{Canvas: schema.Canvas{Width: 270, Height: 480, FPS: 30}}
	a := makeClip(t, dir, "a.mp4", 0)
	b := makeClip(t, dir, "b.mp4", 90)
	out := filepath.Join(dir, "out.mp4")
	edl := schema.EDL{Cuts: []schema.Cut{
		{Clip: a, Start: 0, End: 0.5},
		{Clip: b, Start: 0, End: 0.5},
	}}
	if err := Render(edl, r, out); err != nil {
		t.Fatal(err)
	}
	info, err := ingest.Probe(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 270 || info.Height != 480 {
		t.Errorf("output = %dx%d, want 270x480", info.Width, info.Height)
	}
	if info.Duration < 0.9 || info.Duration > 1.2 {
		t.Errorf("duration = %.2f, want ~1.0", info.Duration)
	}
}

// An untagged upside-down clip (pixels stored rotated, no metadata) is fixed
// by EDL.Fixes: sky must end up on top.
func TestFixesCorrectUpsideDownClip(t *testing.T) {
	dir := t.TempDir()
	clip := filepath.Join(dir, "flipped.mp4")
	if err := synth.WriteClip(clip, synth.Landscape, synth.Options{UpsideDown: true}); err != nil {
		t.Fatal(err)
	}
	r := schema.Recipe{Canvas: schema.Canvas{Width: 640, Height: 360, FPS: 30}}
	cuts := []schema.Cut{{Clip: clip, Start: 0, End: 1}}

	broken := filepath.Join(dir, "broken.mp4")
	if err := Render(schema.EDL{Cuts: cuts}, r, broken); err != nil {
		t.Fatal(err)
	}
	if top := pixel(t, broken, 320, 20); isBlue(top) {
		t.Fatalf("precondition: without a fix the top should not be sky, got %v", top)
	}

	fixed := filepath.Join(dir, "fixed.mp4")
	if err := Render(schema.EDL{Cuts: cuts, Fixes: map[string]int{clip: 180}}, r, fixed); err != nil {
		t.Fatal(err)
	}
	if top := pixel(t, fixed, 320, 20); top[2] < 180 || top[0] > 140 {
		t.Errorf("with fix: top pixel = %v, want sky blue", top)
	}
	if bot := pixel(t, fixed, 320, 340); bot[1] < 130 || bot[2] > 100 {
		t.Errorf("with fix: bottom pixel = %v, want grass green", bot)
	}
}
