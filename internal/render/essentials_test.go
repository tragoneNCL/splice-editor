package render

import (
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"filmeditor/internal/ingest"
	"filmeditor/internal/schema"
	"filmeditor/internal/synth"
)

var canvas640 = schema.Recipe{Canvas: schema.Canvas{Width: 640, Height: 360, FPS: 30}}

func clip(t *testing.T, dir, name, scene string, o synth.Options) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := synth.WriteClip(p, scene, o); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRender(t *testing.T, edl schema.EDL, r schema.Recipe, out string) {
	t.Helper()
	if err := Render(edl, r, out); err != nil {
		t.Fatal(err)
	}
}

func durationOf(t *testing.T, path string) float64 {
	t.Helper()
	info, err := ingest.Probe(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Duration
}

// pixelAt samples RGB at (x,y) at time ts seconds.
func pixelAt(t *testing.T, path string, ts float64, x, y int) [3]int {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-v", "error", "-ss", fmt.Sprint(ts), "-i", path, "-frames:v", "1",
		"-vf", fmt.Sprintf("format=rgb24,crop=1:1:%d:%d", x, y), "-f", "rawvideo", "-").Output()
	if err != nil || len(b) < 3 {
		t.Fatalf("pixel at %.2fs: %v", ts, err)
	}
	return [3]int{int(b[0]), int(b[1]), int(b[2])}
}

// brightCount counts near-white pixels in a horizontal band at time ts.
func brightCount(t *testing.T, path string, ts float64, w, y, h int) int {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-v", "error", "-ss", fmt.Sprint(ts), "-i", path, "-frames:v", "1",
		"-vf", fmt.Sprintf("format=gray,crop=%d:%d:0:%d", w, h, y), "-f", "rawvideo", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range b {
		if v > 200 {
			n++
		}
	}
	return n
}

var meanVol = regexp.MustCompile(`mean_volume: (-?[0-9.]+|-inf) dB`)

func meanVolume(t *testing.T, path string) float64 {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-i", path, "-vn", "-af", "volumedetect", "-f", "null", "-").CombinedOutput()
	m := meanVol.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no volumedetect output (no audio?): %s", out)
	}
	if m[1] == "-inf" {
		return -200
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestSpeed(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.RedCircle, synth.Options{Seconds: 4, Sound: true})
	fast := filepath.Join(dir, "fast.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 2, Speed: 2}}}, canvas640, fast)
	if d := durationOf(t, fast); !near(d, 1.0, 0.15) {
		t.Errorf("2x of 2s = %.2fs, want ~1.0", d)
	}
	slow := filepath.Join(dir, "slow.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 1, Speed: 0.5}}}, canvas640, slow)
	if d := durationOf(t, slow); !near(d, 2.0, 0.15) {
		t.Errorf("0.5x of 1s = %.2fs, want ~2.0", d)
	}
}

func TestVolumeAndMute(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.RedCircle, synth.Options{Seconds: 2, Sound: true})
	loud := filepath.Join(dir, "loud.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 2}}}, canvas640, loud)
	muted := filepath.Join(dir, "muted.mp4")
	zero := 0.0
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 2, Volume: &zero}}}, canvas640, muted)
	l, m := meanVolume(t, loud), meanVolume(t, muted)
	if l < -40 {
		t.Errorf("unmuted mean volume %.1f dB, want audible", l)
	}
	if m > -70 {
		t.Errorf("muted mean volume %.1f dB, want silence", m)
	}
}

func TestDissolveBlendsAndShortensTimeline(t *testing.T) {
	dir := t.TempDir()
	a := clip(t, dir, "a.mp4", synth.RedCircle, synth.Options{Seconds: 2})   // white corners
	b := clip(t, dir, "b.mp4", synth.GreenSquare, synth.Options{Seconds: 2}) // black corners
	out := filepath.Join(dir, "out.mp4")
	edl := schema.EDL{Cuts: []schema.Cut{
		{Clip: a, Start: 0, End: 2},
		{Clip: b, Start: 0, End: 2, Transition: "dissolve", TransitionDur: 1},
	}}
	mustRender(t, edl, canvas640, out)
	if d := durationOf(t, out); !near(d, 3.0, 0.2) {
		t.Errorf("duration %.2f, want ~3.0 (2+2-1)", d)
	}
	if p := pixelAt(t, out, 0.3, 5, 5); p[0] < 230 {
		t.Errorf("before transition corner = %v, want white", p)
	}
	if p := pixelAt(t, out, 1.5, 5, 5); p[0] < 60 || p[0] > 200 {
		t.Errorf("mid transition corner = %v, want a blend", p)
	}
	if p := pixelAt(t, out, 2.8, 5, 5); p[0] > 30 {
		t.Errorf("after transition corner = %v, want black", p)
	}
}

func TestCoverFillsFrameContainLetterboxes(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "land.mp4", synth.Landscape, synth.Options{Seconds: 1}) // 640x360, sky on top
	vertical := schema.Canvas{Width: 360, Height: 640, FPS: 30}
	cuts := []schema.Cut{{Clip: c, Start: 0, End: 1}}

	contain := filepath.Join(dir, "contain.mp4")
	mustRender(t, schema.EDL{Cuts: cuts}, schema.Recipe{Canvas: vertical}, contain)
	if p := pixelAt(t, contain, 0.2, 5, 5); p[0] > 30 || p[2] > 30 {
		t.Errorf("contain top-left = %v, want black bar", p)
	}
	cover := filepath.Join(dir, "cover.mp4")
	mustRender(t, schema.EDL{Cuts: cuts}, schema.Recipe{Canvas: vertical, Fit: "cover"}, cover)
	if p := pixelAt(t, cover, 0.2, 5, 5); p[2] < 200 {
		t.Errorf("cover top-left = %v, want sky blue (frame filled)", p)
	}
	if info, _ := ingest.Probe(cover); info.Width != 360 || info.Height != 640 {
		t.Errorf("cover output %dx%d, want 360x640", info.Width, info.Height)
	}
}

func TestZoomPunchesIn(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.RedCircle, synth.Options{Seconds: 1}) // circle radius 120 at centre
	plain := filepath.Join(dir, "plain.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 1}}}, canvas640, plain)
	zoomed := filepath.Join(dir, "zoom.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 1, Zoom: 2}}}, canvas640, zoomed)
	// 170px right of centre: outside the circle at 1x, inside it at 2x.
	if p := pixelAt(t, plain, 0.2, 490, 180); p[1] < 200 {
		t.Errorf("1x: pixel = %v, want white background", p)
	}
	if p := pixelAt(t, zoomed, 0.2, 490, 180); p[0] < 180 || p[1] > 80 {
		t.Errorf("2x: pixel = %v, want red (circle grew)", p)
	}
}

func TestGrayscale(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.RedCircle, synth.Options{Seconds: 1})
	out := filepath.Join(dir, "gray.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 1, Color: &schema.Color{Grayscale: true}}}}, canvas640, out)
	p := pixelAt(t, out, 0.2, 320, 180)
	if math.Abs(float64(p[0]-p[1])) > 12 || math.Abs(float64(p[1]-p[2])) > 12 {
		t.Errorf("centre = %v, want neutral gray", p)
	}
}

func TestTextAppearsOnlyInItsWindow(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.GreenSquare, synth.Options{Seconds: 3}) // black bottom band
	out := filepath.Join(dir, "title.mp4")
	edl := schema.EDL{
		Cuts:  []schema.Cut{{Clip: c, Start: 0, End: 3}},
		Texts: []schema.Text{{Text: "HELLO WORLD", Start: 0, End: 1}},
	}
	mustRender(t, edl, canvas640, out)
	if n := brightCount(t, out, 0.5, 640, 250, 110); n < 150 {
		t.Errorf("during window: %d bright pixels in text band, want text", n)
	}
	if n := brightCount(t, out, 2.0, 640, 250, 110); n > 5 {
		t.Errorf("after window: %d bright pixels, want none", n)
	}
}

func TestCaptionFollowsItsCut(t *testing.T) {
	dir := t.TempDir()
	a := clip(t, dir, "a.mp4", synth.GreenSquare, synth.Options{Seconds: 2})
	out := filepath.Join(dir, "cap.mp4")
	edl := schema.EDL{Cuts: []schema.Cut{
		{Clip: a, Start: 0, End: 1},
		{Clip: a, Start: 0, End: 1, Caption: "second shot"},
	}}
	mustRender(t, edl, canvas640, out)
	if n := brightCount(t, out, 0.4, 640, 250, 110); n > 5 {
		t.Errorf("first cut shows caption (%d bright px)", n)
	}
	if n := brightCount(t, out, 1.5, 640, 250, 110); n < 100 {
		t.Errorf("second cut missing caption (%d bright px)", n)
	}
}

func tone(t *testing.T, path string, hz, secs int) {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=f=%d:d=%d", hz, secs), "-c:a", "aac", path).CombinedOutput()
	if err != nil {
		t.Fatalf("tone: %v\n%s", err, b)
	}
}

func TestMusicIsMixedAndTrimmedToProgramme(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "silent.mp4", synth.RedCircle, synth.Options{Seconds: 2}) // no audio track
	music := filepath.Join(dir, "m.m4a")
	tone(t, music, 880, 10)
	out := filepath.Join(dir, "out.mp4")
	edl := schema.EDL{
		Cuts:  []schema.Cut{{Clip: c, Start: 0, End: 2}},
		Music: &schema.Music{Path: music, Volume: 0.5},
	}
	mustRender(t, edl, canvas640, out)
	if d := durationOf(t, out); !near(d, 2.0, 0.2) {
		t.Errorf("duration %.2f, want 2.0 (music must not extend the video)", d)
	}
	if v := meanVolume(t, out); v < -45 {
		t.Errorf("mean volume %.1f dB, want audible music", v)
	}
}

func TestDuckingLowersMusicUnderProgramme(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "talk.mp4", synth.RedCircle, synth.Options{Seconds: 3, Sound: true})
	music := filepath.Join(dir, "m.m4a")
	tone(t, music, 880, 5)
	cuts := []schema.Cut{{Clip: c, Start: 0, End: 3}}
	plain := filepath.Join(dir, "plain.mp4")
	mustRender(t, schema.EDL{Cuts: cuts, Music: &schema.Music{Path: music, Volume: 1}}, canvas640, plain)
	ducked := filepath.Join(dir, "ducked.mp4")
	mustRender(t, schema.EDL{Cuts: cuts, Music: &schema.Music{Path: music, Volume: 1, Duck: true}}, canvas640, ducked)
	p, d := meanVolume(t, plain), meanVolume(t, ducked)
	if d > p-0.5 {
		t.Errorf("ducked %.1f dB vs plain %.1f dB, want ducked quieter", d, p)
	}
}

func TestFadeInAndOut(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.RedCircle, synth.Options{Seconds: 2})
	out := filepath.Join(dir, "out.mp4")
	mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 2}}, FadeIn: 0.5, FadeOut: 0.5}, canvas640, out)
	if p := pixelAt(t, out, 0, 320, 180); p[0] > 40 {
		t.Errorf("first frame = %v, want black", p)
	}
	if p := pixelAt(t, out, 1.0, 320, 180); p[0] < 180 {
		t.Errorf("middle = %v, want full red", p)
	}
	if p := pixelAt(t, out, 1.95, 320, 180); p[0] > 90 {
		t.Errorf("last frame = %v, want near black", p)
	}
}

func TestExportPresets(t *testing.T) {
	dir := t.TempDir()
	c := clip(t, dir, "c.mp4", synth.Landscape, synth.Options{Seconds: 1})
	for name, want := range map[string][2]int{"1:1": {1080, 1080}, "16:9": {1920, 1080}} {
		out := filepath.Join(dir, "p.mp4")
		r := schema.Recipe{Canvas: schema.Presets[name], Fit: "cover"}
		mustRender(t, schema.EDL{Cuts: []schema.Cut{{Clip: c, Start: 0, End: 1}}}, r, out)
		if info, _ := ingest.Probe(out); info.Width != want[0] || info.Height != want[1] {
			t.Errorf("%s: %dx%d, want %dx%d", name, info.Width, info.Height, want[0], want[1])
		}
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	bad := map[string]schema.Cut{
		"speed":      {Clip: "a", Start: 0, End: 1, Speed: 9},
		"zoom":       {Clip: "a", Start: 0, End: 1, Zoom: 0.5},
		"transition": {Clip: "a", Start: 0, End: 1, Transition: "spin"},
	}
	for name, c := range bad {
		if err := (schema.EDL{Cuts: []schema.Cut{c}}).Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}
