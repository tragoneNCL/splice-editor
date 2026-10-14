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
	analysisWidth  = 480 // analysis proxy width; blank/detail thresholds are tuned for it

	// A frame whose edge map is nearly empty is a blank wall, a lens cap, or a
	// white-out, not something to show. Measured on real listing footage:
	// blank walls read 0 to 1, real rooms 2.5 to 12.
	blankDetail = 1.0
	// A usable run shorter than this is not worth a cut.
	minUsableSeconds = 0.8
	// Blank is judged against the clip itself: a frame is dead only if the same
	// clip also has frames with real detail. A clip that is dark, soft or plain
	// throughout is kept whole rather than thrown away.
	richDetail = 2.5
)

// Shot is a contiguous run of footage inside one clip.
type Shot struct {
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Score      float64 `json:"score"`           // 0..1, higher is a better-looking shot
	Blur       float64 `json:"blur"`            // mean blurdetect value, higher is blurrier
	Brightness float64 `json:"brightness"`      // mean luma 0..255
	Detail     float64 `json:"detail"`          // mean edge density; low = bare wall
	Motion     float64 `json:"motion"`          // mean frame-to-frame change; high = fast camera move
	Peak       float64 `json:"peak"`            // time of the best-looking frame in the shot
	Thumb      string  `json:"thumb,omitempty"` // path to a representative frame, if extracted
	Hash       uint64  `json:"hash,omitempty"`  // perceptual hash of the representative frame
	Group      int     `json:"group"`           // shots with the same non-zero group look alike
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

type frameStat struct{ t, blur, luma, detail, motion float64 }

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
// small proxy: analysis does not need full resolution, and decoding plus
// scaling is where nearly all the time goes on large footage.
func scan(path string) ([]float64, []frameStat, error) {
	m1, err := os.CreateTemp("", "scan-q-*.txt")
	if err != nil {
		return nil, nil, err
	}
	m1.Close()
	defer os.Remove(m1.Name())
	m2, err := os.CreateTemp("", "scan-e-*.txt")
	if err != nil {
		return nil, nil, err
	}
	m2.Close()
	defer os.Remove(m2.Name())

	graph := fmt.Sprintf(
		"[0:v:0]scale=%d:-2:flags=fast_bilinear,split=3[a][b][c];"+
			"[a]select='gt(scene,%g)+eq(n,0)',showinfo[s];"+
			"[b]fps=%d,blurdetect,signalstats,metadata=print:file=%s[q];"+
			"[c]fps=%d,edgedetect=low=0.08:high=0.25,signalstats,metadata=print:file=%s[e]",
		analysisWidth, sceneThreshold, statsFPS, m1.Name(), statsFPS, m2.Name())
	out, err := exec.Command("ffmpeg", "-v", "info", "-i", path, "-an",
		"-filter_complex", graph,
		"-map", "[s]", "-f", "null", "-",
		"-map", "[q]", "-f", "null", "-",
		"-map", "[e]", "-f", "null", "-").CombinedOutput()
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

	quality, err := readMeta(m1.Name())
	if err != nil {
		return nil, nil, err
	}
	edges, err := readMeta(m2.Name())
	if err != nil {
		return nil, nil, err
	}
	frames := make([]frameStat, 0, len(quality))
	for i, q := range quality {
		f := frameStat{t: q["t"], blur: q["lavfi.blur"], luma: q["lavfi.signalstats.YAVG"], motion: q["lavfi.signalstats.YDIF"]}
		if math.IsNaN(f.blur) || math.IsInf(f.blur, 0) {
			f.blur = 0 // blurdetect has no answer on a featureless frame
		}
		if i < len(edges) {
			f.detail = edges[i]["lavfi.signalstats.YAVG"]
		}
		frames = append(frames, f)
	}
	return cuts, frames, nil
}

// readMeta parses ffmpeg's metadata=print output into one map per frame.
func readMeta(path string) ([]map[string]float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []map[string]float64
	var cur map[string]float64
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "frame:"):
			cur = map[string]float64{}
			if m := ptsTime.FindStringSubmatch(line); m != nil {
				cur["t"], _ = strconv.ParseFloat(m[1], 64)
			}
			out = append(out, cur)
		case cur != nil && strings.Contains(line, "="):
			kv := strings.SplitN(line, "=", 2)
			if v, err := strconv.ParseFloat(kv[1], 64); err == nil {
				cur[kv[0]] = v
			}
		}
	}
	return out, nil
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

	// Nothing in the clip has real detail: do not treat anything as blank.
	blankBelow := blankDetail
	var richest float64
	for _, f := range frames {
		if f.detail > richest {
			richest = f.detail
		}
	}
	if richest < richDetail {
		blankBelow = 0
	}

	step := 1.0 / statsFPS
	var shots []Shot
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		var in []frameStat
		for _, f := range frames {
			if f.t >= lo && f.t < hi {
				in = append(in, f)
			}
		}
		if len(in) == 0 { // too short to sample: keep it, neutral numbers
			shots = append(shots, Shot{Start: lo, End: hi, Brightness: 128, Detail: 5, Peak: (lo + hi) / 2, Score: score(0, 128, 5)})
			continue
		}
		// Runs of non-blank frames become shots; blank stretches are dropped.
		runStart := -1
		flush := func(endIdx int) {
			if runStart < 0 {
				return
			}
			run := in[runStart:endIdx]
			start := run[0].t
			if start < lo+step {
				start = lo // a run that begins at the shot's first sample begins at the shot
			}
			end := run[len(run)-1].t + step
			if end > hi || endIdx == len(in) {
				end = hi
			}
			if end-start >= minUsableSeconds {
				shots = append(shots, summarize(start, end, run))
			}
			runStart = -1
		}
		for k, f := range in {
			if f.detail >= blankBelow {
				if runStart < 0 {
					runStart = k
				}
			} else {
				flush(k)
			}
		}
		flush(len(in))
	}
	return shots
}

// summarize rates one usable run of frames.
func summarize(start, end float64, run []frameStat) Shot {
	s := Shot{Start: start, End: end}
	best := -1.0
	for _, f := range run {
		s.Blur += f.blur
		s.Brightness += f.luma
		s.Detail += f.detail
		s.Motion += f.motion
		if fs := score(f.blur, f.luma, f.detail); fs > best {
			best, s.Peak = fs, f.t
		}
	}
	n := float64(len(run))
	s.Blur, s.Brightness, s.Detail, s.Motion = s.Blur/n, s.Brightness/n, s.Detail/n, s.Motion/n
	s.Score = score(s.Blur, s.Brightness, s.Detail)
	return s
}

// score is a heuristic: sharp, well-exposed shots score near 1.
func score(blur, luma, detail float64) float64 {
	sharp := 1 / (1 + blur/5)
	exposure := 1 - math.Min(1, math.Abs(luma-120)/120)
	richness := math.Min(1, detail/6) // bare walls have little to look at
	return math.Round((0.5*sharp+0.2*exposure+0.3*richness)*1000) / 1000
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
		sh := &c.Shots[i]
		// The best-looking moment, kept clear of the shot's edges.
		t := sh.Peak
		if lo, hi := sh.Start+0.1, sh.End-0.1; hi > lo {
			t = math.Min(math.Max(t, lo), hi)
		} else {
			t = (sh.Start + sh.End) / 2
		}
		out := filepath.Join(dir, fmt.Sprintf("%s_shot%d.jpg", base, i))
		if err := Frame(c.Path, t, c.Fix, out); err != nil {
			return err
		}
		sh.Thumb = out
		if h, err := HashFile(out); err == nil {
			sh.Hash = h
		}
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
