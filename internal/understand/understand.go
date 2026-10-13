// Package understand turns raw clips into summaries the planner can reason
// about: shot boundaries and a per-shot quality score. All analysis is
// deterministic ffmpeg work; no model is involved.
package understand

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"filmeditor/internal/ingest"
)

const (
	sceneThreshold = 0.3 // ffmpeg scene score above which we call a cut
	minShotSeconds = 0.6 // shorter shots are merged into their neighbour
	statsFPS       = 2   // frames per second sampled for quality stats
)

// Shot is a contiguous run of footage inside one clip.
type Shot struct {
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Score      float64 `json:"score"`           // 0..1, higher is a better-looking shot
	Blur       float64 `json:"blur"`            // mean blurdetect value, higher is blurrier
	Brightness float64 `json:"brightness"`      // mean luma 0..255
	Thumb      string  `json:"thumb,omitempty"` // path to a representative frame, if extracted
}

func (s Shot) Length() float64 { return s.End - s.Start }

// ClipSummary is everything the planner knows about one clip.
type ClipSummary struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	HasAudio bool    `json:"has_audio"`
	// Fix is extra clockwise degrees correcting pixels stored the wrong way
	// up with no rotation metadata (set by the orientation check).
	Fix   int    `json:"fix,omitempty"`
	Shots []Shot `json:"shots"`
}

type frameStat struct{ t, blur, luma float64 }

// Analyze probes a clip and splits it into scored shots.
func Analyze(path string) (ClipSummary, error) {
	info, err := ingest.Probe(path)
	if err != nil {
		return ClipSummary{}, err
	}
	cuts, frames, err := scan(path)
	if err != nil {
		return ClipSummary{}, err
	}
	return ClipSummary{
		Path: path, Duration: info.Duration,
		Width: info.Width, Height: info.Height, HasAudio: info.HasAudio,
		Shots: buildShots(info.Duration, cuts, frames),
	}, nil
}

var ptsTime = regexp.MustCompile(`pts_time:([0-9.]+)`)

// scan finds scene cuts and samples per-frame quality in ONE decode, on a
// 320px-wide proxy: analysis does not need full resolution, and decoding plus
// scaling is where nearly all the time goes on large footage.
func scan(path string) ([]float64, []frameStat, error) {
	meta, err := os.CreateTemp("", "scan-*.txt")
	if err != nil {
		return nil, nil, err
	}
	meta.Close()
	defer os.Remove(meta.Name())

	graph := fmt.Sprintf(
		"[0:v:0]scale=320:-2:flags=fast_bilinear,split=2[a][b];"+
			"[a]select='gt(scene,%g)+eq(n,0)',showinfo[s];"+
			"[b]fps=%d,blurdetect,signalstats,metadata=print:file=%s[m]",
		sceneThreshold, statsFPS, meta.Name())
	out, err := exec.Command("ffmpeg", "-v", "info", "-i", path, "-an",
		"-filter_complex", graph,
		"-map", "[s]", "-f", "null", "-",
		"-map", "[m]", "-f", "null", "-").CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("analyse %s: %w\n%s", path, err, tail(out))
	}

	var cuts []float64
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "showinfo") {
			continue
		}
		if m := ptsTime.FindStringSubmatch(line); m != nil {
			if t, err := strconv.ParseFloat(m[1], 64); err == nil {
				cuts = append(cuts, t)
			}
		}
	}
	sort.Float64s(cuts)

	raw, err := os.ReadFile(meta.Name())
	if err != nil {
		return nil, nil, err
	}
	var frames []frameStat
	var cur frameStat
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "frame:"):
			if m := ptsTime.FindStringSubmatch(line); m != nil {
				cur = frameStat{}
				cur.t, _ = strconv.ParseFloat(m[1], 64)
			}
		case strings.HasPrefix(line, "lavfi.blur="):
			cur.blur, _ = strconv.ParseFloat(strings.TrimPrefix(line, "lavfi.blur="), 64)
		case strings.HasPrefix(line, "lavfi.signalstats.YAVG="):
			cur.luma, _ = strconv.ParseFloat(strings.TrimPrefix(line, "lavfi.signalstats.YAVG="), 64)
			frames = append(frames, cur)
		}
	}
	return cuts, frames, nil
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	return s
}

func buildShots(duration float64, cuts []float64, frames []frameStat) []Shot {
	bounds := []float64{0}
	for _, c := range cuts {
		if c-bounds[len(bounds)-1] >= minShotSeconds && duration-c >= minShotSeconds {
			bounds = append(bounds, c)
		}
	}
	bounds = append(bounds, duration)

	var shots []Shot
	for i := 0; i+1 < len(bounds); i++ {
		s := Shot{Start: bounds[i], End: bounds[i+1]}
		var n float64
		for _, f := range frames {
			if f.t >= s.Start && f.t < s.End {
				s.Blur += f.blur
				s.Brightness += f.luma
				n++
			}
		}
		if n > 0 {
			s.Blur /= n
			s.Brightness /= n
		} else {
			s.Brightness = 128 // too short to sample; assume neutral
		}
		s.Score = score(s.Blur, s.Brightness)
		shots = append(shots, s)
	}
	return shots
}

// score is a heuristic: sharp, well-exposed shots score near 1.
func score(blur, luma float64) float64 {
	sharp := 1 / (1 + blur/5)
	exposure := 1 - math.Min(1, math.Abs(luma-120)/120)
	return math.Round((0.7*sharp+0.3*exposure)*1000) / 1000
}

// Thumbnails writes one upright frame per shot (the middle of the shot) into
// dir and records the paths on c.Shots. Call after Fix is set so the planner
// sees the clip the way the viewer will.
func (c *ClipSummary) Thumbnails(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(c.Path), filepath.Ext(c.Path))
	for i := range c.Shots {
		mid := (c.Shots[i].Start + c.Shots[i].End) / 2
		out := filepath.Join(dir, fmt.Sprintf("%s_shot%d.jpg", base, i))
		vf := "scale=384:-2"
		if f := ingest.RotateFilter(c.Fix); f != "" {
			vf = f + "," + vf
		}
		b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-ss", fmt.Sprintf("%.2f", mid),
			"-i", c.Path, "-frames:v", "1", "-vf", vf, "-q:v", "3", out).CombinedOutput()
		if err != nil {
			return fmt.Errorf("thumbnail %s: %w\n%s", c.Path, err, b)
		}
		c.Shots[i].Thumb = out
	}
	return nil
}

// Frame writes one upright, downscaled JPEG of the clip at time t.
func Frame(path string, t float64, fix int, out string) error {
	vf := "scale=384:-2"
	if f := ingest.RotateFilter(fix); f != "" {
		vf = f + "," + vf
	}
	b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-ss", fmt.Sprintf("%.2f", t),
		"-i", path, "-frames:v", "1", "-vf", vf, "-q:v", "3", out).CombinedOutput()
	if err != nil {
		return fmt.Errorf("frame %s: %w\n%s", path, err, b)
	}
	return nil
}
