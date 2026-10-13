// Package render turns an EDL + Recipe into an mp4 via ffmpeg.
//
// ffmpeg autorotates inputs by default (applies the display matrix), so each
// cut is already upright before it is fitted onto the canvas.
//
// Graph shape: every cut becomes a [v_i]/[a_i] pair (trim, speed, fix, fit,
// grade); pairs are chained with concat (hard cut) or xfade/acrossfade
// (transitions); then fades, text overlays, music and loudness are applied.
package render

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"filmeditor/internal/ingest"
	"filmeditor/internal/schema"
)

const defaultTransition = 0.5

// BuildArgs returns the ffmpeg arguments for the edit. workDir receives
// generated text overlays and must outlive the ffmpeg run.
func BuildArgs(edl schema.EDL, r schema.Recipe, out, workDir string) ([]string, error) {
	if err := edl.Validate(); err != nil {
		return nil, err
	}
	c := r.Canvas
	if c.Width <= 0 || c.Height <= 0 || c.FPS <= 0 {
		return nil, fmt.Errorf("invalid canvas %+v", c)
	}
	args := []string{"-y", "-v", "error"}
	for _, cut := range edl.Cuts {
		args = append(args, "-i", cut.Clip)
	}
	n := len(edl.Cuts)

	var parts []string
	durs := make([]float64, n)
	for i, cut := range edl.Cuts {
		info, err := ingest.Probe(cut.Clip)
		if err != nil {
			return nil, err
		}
		end := math.Min(cut.End, info.Duration)
		if cut.Start >= end {
			return nil, fmt.Errorf("cut %d: start %.2f is past end of clip (%.2fs)", i, cut.Start, info.Duration)
		}
		speed := cut.SpeedOrDefault()
		durs[i] = (end - cut.Start) / speed

		v := fmt.Sprintf("[%d:v]trim=start=%.3f:end=%.3f,setpts=(PTS-STARTPTS)/%.4f", i, cut.Start, end, speed)
		if f := ingest.RotateFilter(edl.Fixes[cut.Clip]); f != "" {
			v += "," + f
		}
		v += "," + fitFilter(c, r.Fit, cut)
		if g := gradeFilter(cut.Color); g != "" {
			v += "," + g
		}
		v += fmt.Sprintf(",setsar=1,fps=%d,format=yuv420p[v%d]", c.FPS, i)
		parts = append(parts, v)

		if info.HasAudio {
			a := fmt.Sprintf("[%d:a]atrim=start=%.3f:end=%.3f,asetpts=PTS-STARTPTS", i, cut.Start, end)
			for _, t := range atempoChain(speed) {
				a += fmt.Sprintf(",atempo=%.4f", t)
			}
			vol := 1.0
			if cut.Volume != nil {
				vol = *cut.Volume
			}
			if vol != 1 {
				a += fmt.Sprintf(",volume=%.3f", vol)
			}
			a += fmt.Sprintf(",aresample=48000,aformat=channel_layouts=stereo[a%d]", i)
			parts = append(parts, a)
		} else {
			parts = append(parts, fmt.Sprintf(
				"anullsrc=r=48000:cl=stereo,atrim=duration=%.3f,asetpts=PTS-STARTPTS[a%d]", durs[i], i))
		}
	}

	// Chain the cuts, tracking where each starts on the timeline.
	curV, curA := "v0", "a0"
	length := durs[0]
	starts := make([]float64, n)
	for i := 1; i < n; i++ {
		cut := edl.Cuts[i]
		nv, na := fmt.Sprintf("cv%d", i), fmt.Sprintf("ca%d", i)
		if cut.Transition == "" {
			parts = append(parts,
				fmt.Sprintf("[%s][v%d]concat=n=2:v=1:a=0[%s]", curV, i, nv),
				fmt.Sprintf("[%s][a%d]concat=n=2:v=0:a=1[%s]", curA, i, na))
			starts[i] = length
			length += durs[i]
		} else {
			t := cut.TransitionDur
			if t <= 0 {
				t = defaultTransition
			}
			t = math.Min(t, math.Min(length, durs[i])*0.9)
			// A cross-dissolve in an editor is xfade's "fade". xfade's own
			// "dissolve" is a random-pixel dither, which is not what people mean.
			kind := "fade"
			if cut.Transition == "fade" {
				kind = "fadeblack"
			}
			parts = append(parts,
				fmt.Sprintf("[%s][v%d]xfade=transition=%s:duration=%.3f:offset=%.3f[%s]", curV, i, kind, t, length-t, nv),
				fmt.Sprintf("[%s][a%d]acrossfade=d=%.3f[%s]", curA, i, t, na))
			starts[i] = length - t
			length += durs[i] - t
		}
		curV, curA = nv, na
	}

	// Fade in/out of the whole programme. A transition on the first cut means
	// "from black".
	fadeIn := edl.FadeIn
	if edl.Cuts[0].Transition != "" {
		fadeIn = math.Max(fadeIn, orDefault(edl.Cuts[0].TransitionDur, defaultTransition))
	}
	if fadeIn > 0 {
		parts = append(parts,
			fmt.Sprintf("[%s]fade=t=in:st=0:d=%.3f[fi]", curV, fadeIn),
			fmt.Sprintf("[%s]afade=t=in:st=0:d=%.3f[fia]", curA, fadeIn))
		curV, curA = "fi", "fia"
	}
	if edl.FadeOut > 0 {
		st := math.Max(0, length-edl.FadeOut)
		parts = append(parts,
			fmt.Sprintf("[%s]fade=t=out:st=%.3f:d=%.3f[fo]", curV, st, edl.FadeOut),
			fmt.Sprintf("[%s]afade=t=out:st=%.3f:d=%.3f[foa]", curA, st, edl.FadeOut))
		curV, curA = "fo", "foa"
	}

	// Text overlays: explicit texts plus per-cut captions.
	texts := append([]schema.Text(nil), edl.Texts...)
	for i, cut := range edl.Cuts {
		if cut.Caption != "" {
			texts = append(texts, schema.Text{Text: cut.Caption, Start: starts[i], End: starts[i] + durs[i]})
		}
	}
	input := n
	for k, t := range texts {
		png := filepath.Join(workDir, fmt.Sprintf("text_%d.png", k))
		if err := writeTextPNG(png, c.Width, c.Height, t); err != nil {
			return nil, fmt.Errorf("text %q: %w", t.Text, err)
		}
		args = append(args, "-i", png)
		nv := fmt.Sprintf("tv%d", k)
		parts = append(parts, fmt.Sprintf("[%s][%d:v]overlay=0:0:enable='between(t,%.3f,%.3f)'[%s]",
			curV, input, t.Start, t.End, nv))
		curV = nv
		input++
	}

	// Music, optionally ducked under the programme audio.
	if m := edl.Music; m != nil {
		args = append(args, "-stream_loop", "-1", "-i", m.Path)
		vol := m.Volume
		if vol <= 0 {
			vol = 0.3
		}
		mus := fmt.Sprintf("[%d:a]aresample=48000,aformat=channel_layouts=stereo,atrim=duration=%.3f,asetpts=PTS-STARTPTS,volume=%.3f",
			input, length, vol)
		if m.FadeOut > 0 {
			mus += fmt.Sprintf(",afade=t=out:st=%.3f:d=%.3f", math.Max(0, length-m.FadeOut), m.FadeOut)
		}
		parts = append(parts, mus+"[mus]")
		if m.Duck {
			parts = append(parts,
				fmt.Sprintf("[%s]asplit=2[pa][pk]", curA),
				"[mus][pk]sidechaincompress=threshold=0.02:ratio=12:attack=20:release=300[md]",
				"[pa][md]amix=inputs=2:duration=first:normalize=0[mix]")
		} else {
			parts = append(parts, fmt.Sprintf("[%s][mus]amix=inputs=2:duration=first:normalize=0[mix]", curA))
		}
		curA = "mix"
	}
	if r.Loudnorm {
		parts = append(parts, fmt.Sprintf("[%s]loudnorm=I=-16:TP=-1.5:LRA=11[ln]", curA))
		curA = "ln"
	}

	crf := r.CRF
	if crf <= 0 {
		crf = 20
	}
	args = append(args,
		"-filter_complex", strings.Join(parts, ";"),
		"-map", "["+curV+"]", "-map", "["+curA+"]",
		"-t", fmt.Sprintf("%.3f", length),
		"-c:v", "libx264", "-crf", fmt.Sprint(crf), "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", out)
	return args, nil
}

// Render runs ffmpeg and writes the finished video to out.
func Render(edl schema.EDL, r schema.Recipe, out string) error {
	work, err := os.MkdirTemp("", "render-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	args, err := BuildArgs(edl, r, out, work)
	if err != nil {
		return err
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, b)
	}
	return nil
}

// fitFilter places a cut on the canvas: contain (letterbox) or cover (fill
// and crop), with optional punch-in zoom around a focus point.
func fitFilter(c schema.Canvas, fit string, cut schema.Cut) string {
	zoom := cut.Zoom
	if zoom < 1 {
		zoom = 1
	}
	if fit != "cover" && zoom == 1 {
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
			c.Width, c.Height, c.Width, c.Height)
	}
	fx, fy := orDefault(cut.FocusX, 0.5), orDefault(cut.FocusY, 0.5)
	sw, sh := even(float64(c.Width)*zoom), even(float64(c.Height)*zoom)
	return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d:(iw-%d)*%.3f:(ih-%d)*%.3f",
		sw, sh, c.Width, c.Height, c.Width, fx, c.Height, fy)
}

func gradeFilter(col *schema.Color) string {
	if col == nil {
		return ""
	}
	var fs []string
	if col.Brightness != 0 || col.Contrast != 0 || col.Saturation != 0 {
		fs = append(fs, fmt.Sprintf("eq=brightness=%.3f:contrast=%.3f:saturation=%.3f",
			col.Brightness, orDefault(col.Contrast, 1), orDefault(col.Saturation, 1)))
	}
	if col.Grayscale {
		fs = append(fs, "hue=s=0")
	}
	return strings.Join(fs, ",")
}

// atempoChain splits a speed factor into steps within atempo's 0.5..2 range.
func atempoChain(speed float64) []float64 {
	if speed == 1 {
		return nil
	}
	var steps []float64
	for speed > 2 {
		steps = append(steps, 2)
		speed /= 2
	}
	for speed < 0.5 {
		steps = append(steps, 0.5)
		speed /= 0.5
	}
	return append(steps, speed)
}

func even(v float64) int {
	n := int(math.Ceil(v))
	if n%2 != 0 {
		n++
	}
	return n
}

func orDefault(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}
