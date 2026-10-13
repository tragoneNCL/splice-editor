package inspo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"filmeditor/internal/schema"
	"filmeditor/internal/synth"
)

// Build a reference with a known rhythm: three 3s shots, then six 1s shots.
func TestAnalyzeMeasuresKnownRhythm(t *testing.T) {
	dir := t.TempDir()
	var parts []string
	add := func(scene string, secs float64, name string) {
		p := filepath.Join(dir, name)
		if err := synth.WriteClip(p, scene, synth.Options{Seconds: secs, Width: 320, Height: 568}); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	scenes := []string{synth.RedCircle, synth.GreenSquare, synth.Landscape, synth.Night}
	for i := 0; i < 3; i++ {
		add(scenes[i%4], 3, "s"+string(rune('a'+i))+".mp4")
	}
	for i := 0; i < 6; i++ {
		add(scenes[(i+1)%4], 1, "f"+string(rune('a'+i))+".mp4")
	}
	ref := filepath.Join(dir, "ref.mp4")
	if err := synth.Concat(ref, parts...); err != nil {
		t.Fatal(err)
	}
	st, err := Analyze(ref, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Shots < 7 || st.Shots > 10 {
		t.Errorf("shots = %d, want about 9", st.Shots)
	}
	if st.AvgShot < 1.4 || st.AvgShot > 2.4 {
		t.Errorf("avg shot = %.2f, want about 1.7", st.AvgShot)
	}
	if st.LateAvg >= st.EarlyAvg {
		t.Errorf("early %.2f late %.2f: should speed up", st.EarlyAvg, st.LateAvg)
	}
	d := st.Describe()
	if !strings.Contains(d, "faster toward the end") {
		t.Errorf("description missing pacing trend: %q", d)
	}
	r := schema.DefaultRecipe()
	st.Apply(&r)
	if r.AvgShotLength != st.AvgShot {
		t.Errorf("recipe avg shot = %.2f, want %.2f", r.AvgShotLength, st.AvgShot)
	}
}

func TestValidateYouTubeURL(t *testing.T) {
	ok := []string{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://youtu.be/dQw4w9WgXcQ", "https://m.youtube.com/shorts/abc"}
	for _, u := range ok {
		if _, err := ValidateYouTubeURL(u); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	bad := []string{
		"http://www.youtube.com/watch?v=x",       // not https
		"https://evil.example.com/watch?v=x",     // other host
		"https://youtube.com.evil.example.com/x", // lookalike
		"https://user@www.youtube.com/watch?v=x", // userinfo
		"https://www.youtube.com:8443/watch?v=x", // odd port
		"file:///etc/passwd", "javascript:alert(1)", "",
		"https://169.254.169.254/latest/meta-data",
	}
	for _, u := range bad {
		if _, err := ValidateYouTubeURL(u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestFetchRefusesNonYouTubeBeforeAnyDownload(t *testing.T) {
	_, err := FetchYouTube(context.Background(), "https://example.com/video.mp4", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "YouTube") {
		t.Errorf("err = %v", err)
	}
}
