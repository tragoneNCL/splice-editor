// Package inspo turns a reference ("inspiration") video into numbers and a
// sentence the planner can use: how fast it cuts, whether it speeds up, its
// shape. Only these statistics are kept; the reference footage itself is
// never used in the edit and is deleted right after analysis.
package inspo

import (
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

// maxAnalyzeSeconds bounds the work for long references.
const maxAnalyzeSeconds = 180

type Style struct {
	DurationSec float64 `json:"duration"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Shots       int     `json:"shots"`
	AvgShot     float64 `json:"avg_shot"`
	MedianShot  float64 `json:"median_shot"`
	EarlyAvg    float64 `json:"early_avg"` // average shot length, first half
	LateAvg     float64 `json:"late_avg"`  // average shot length, second half
	HasAudio    bool    `json:"has_audio"`
}

// Analyze measures the reference at path. workDir is scratch space.
func Analyze(path, workDir string) (Style, error) {
	src := path
	// Long references: analyse only the first few minutes (stream copy, cheap).
	if probeDur, err := durationOf(path); err == nil && probeDur > maxAnalyzeSeconds {
		short := filepath.Join(workDir, "inspo_short.mp4")
		b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-i", path, "-t", fmt.Sprint(maxAnalyzeSeconds),
			"-c", "copy", short).CombinedOutput()
		if err != nil {
			return Style{}, fmt.Errorf("trim reference: %w\n%s", err, b)
		}
		src = short
	}
	c, err := understand.Analyze(src)
	if err != nil {
		return Style{}, fmt.Errorf("analyse reference: %w", err)
	}
	return fromClip(c), nil
}

func fromClip(c understand.ClipSummary) Style {
	st := Style{DurationSec: c.Duration, Width: c.Width, Height: c.Height, HasAudio: c.HasAudio, Shots: len(c.Shots)}
	if len(c.Shots) == 0 {
		return st
	}
	lens := make([]float64, 0, len(c.Shots))
	var early, late []float64
	for _, s := range c.Shots {
		l := s.Length()
		lens = append(lens, l)
		if (s.Start+s.End)/2 < c.Duration/2 {
			early = append(early, l)
		} else {
			late = append(late, l)
		}
	}
	st.AvgShot = mean(lens)
	sort.Float64s(lens)
	st.MedianShot = lens[len(lens)/2]
	st.EarlyAvg, st.LateAvg = mean(early), mean(late)
	return st
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

// Apply nudges the recipe toward the reference's pacing. Length and format
// stay under the user's control.
func (s Style) Apply(r *schema.Recipe) {
	if s.AvgShot > 0 {
		r.AvgShotLength = math.Max(0.6, math.Min(8, s.AvgShot))
	}
}

// Aspect names the closest standard format.
func (s Style) Aspect() string {
	if s.Width == 0 || s.Height == 0 {
		return ""
	}
	ratio := float64(s.Width) / float64(s.Height)
	best, bestD := "", math.MaxFloat64
	for name, c := range schema.Presets {
		if d := math.Abs(ratio - float64(c.Width)/float64(c.Height)); d < bestD {
			best, bestD = name, d
		}
	}
	return best
}

// Describe is a sentence for the planner's brief.
func (s Style) Describe() string {
	if s.Shots == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Match the editing style of a reference video: %.0f seconds, %s, %d shots averaging %.1fs",
		s.DurationSec, s.Aspect(), s.Shots, s.AvgShot)
	switch {
	case s.EarlyAvg > 0 && s.LateAvg > 0 && s.LateAvg < s.EarlyAvg*0.75:
		fmt.Fprintf(&b, ", cutting faster toward the end (%.1fs early, %.1fs late)", s.EarlyAvg, s.LateAvg)
	case s.EarlyAvg > 0 && s.LateAvg > s.EarlyAvg*1.35:
		fmt.Fprintf(&b, ", slowing down toward the end (%.1fs early, %.1fs late)", s.EarlyAvg, s.LateAvg)
	}
	switch {
	case s.AvgShot < 1.5:
		b.WriteString(". It is very fast and energetic")
	case s.AvgShot > 4:
		b.WriteString(". It is slow and unhurried")
	}
	b.WriteString(".")
	return b.String()
}

func durationOf(path string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0, err
	}
	var d float64
	_, err = fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &d)
	return d, err
}
