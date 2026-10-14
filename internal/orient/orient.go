// Package orient is the safety net for clips whose pixels are stored the
// wrong way up with no rotation metadata (so ffprobe cannot know). Container
// rotation is handled deterministically in ingest; this only catches what
// metadata cannot, using a vision model on one sampled frame per clip.
package orient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"filmeditor/internal/ingest"
	"filmeditor/internal/llm"
)

// Detector looks at frames (id -> image path) and returns, per id, the
// clockwise degrees (0/90/180/270) needed to make the frame upright, or nil when
// the frame has no orientation cues (a blank wall, a close-up of a texture).
type Detector interface {
	Detect(ctx context.Context, frames map[string]string) (map[string]*int, error)
}

// framesPerClip frames are judged per clip, at these fractions of its length.
// One frame can land on a blank wall; several rarely all do.
var sampleAt = []float64{0.2, 0.5, 0.8}

// Fix returns clip path -> extra clockwise degrees to apply (omitted = none).
//
//   - Each clip is judged from several frames; frames that cannot tell are
//     ignored, and the rest vote.
//   - A clip with no usable evidence takes the answer of clips that were shot
//     the same way (same size and stored rotation) if those all agree, because
//     footage from one camera or gimbal is stored one way. Clips with their own
//     clear evidence are never overridden by the group.
//
// workDir holds the sampled frames.
func Fix(ctx context.Context, d Detector, clips []string, workDir string) (map[string]int, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	frames := map[string]string{}
	owner := map[string]int{} // frame id -> clip index
	sig := make([]string, len(clips))
	for i, c := range clips {
		info, err := ingest.Probe(c)
		if err != nil {
			return nil, err
		}
		sig[i] = fmt.Sprintf("%dx%d/%d", info.Width, info.Height, info.Rotation)
		for k, frac := range sampleAt {
			id := fmt.Sprintf("c%df%d", i, k)
			out := filepath.Join(workDir, id+".jpg")
			b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-ss", fmt.Sprintf("%.2f", info.Duration*frac),
				"-i", c, "-frames:v", "1", "-vf", "scale=384:-2", "-q:v", "3", out).CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("sample frame %s: %w\n%s", c, err, b)
			}
			// Judging "which of these four is upright" is far more reliable for
			// a vision model than naming a rotation angle, so each sampled frame
			// is shown in all four orientations side by side.
			panel := filepath.Join(workDir, id+"_panels.jpg")
			if err := makePanels(out, panel); err != nil {
				return nil, err
			}
			frames[id], owner[id] = panel, i
		}
	}
	if len(frames) == 0 {
		return nil, nil
	}
	res, err := d.Detect(ctx, frames)
	if err != nil {
		return nil, err
	}

	votes := make([]map[int]int, len(clips)) // clip -> degrees -> count
	for i := range votes {
		votes[i] = map[int]int{}
	}
	for id, deg := range res {
		i, ok := owner[id]
		if !ok || deg == nil {
			continue
		}
		if *deg%90 != 0 || *deg < 0 || *deg > 270 {
			return nil, fmt.Errorf("detector returned invalid rotation %d for %s", *deg, id)
		}
		votes[i][*deg]++
	}

	decided := make([]int, len(clips))
	has := make([]bool, len(clips))
	for i, v := range votes {
		best, bestN, tie := 0, 0, false
		for deg, n := range v {
			switch {
			case n > bestN:
				best, bestN, tie = deg, n, false
			case n == bestN:
				tie = true
			}
		}
		if bestN > 0 && !tie {
			decided[i], has[i] = best, true
		}
	}
	// Clips with no evidence of their own follow the clear majority of the
	// clips shot the same way (needs at least two decided clips and a 60%
	// agreement). One clip needing a different rotation does not cancel the
	// group. Clips with their own evidence are never overridden.
	type tally struct {
		n     int
		byDeg map[int]int
	}
	groups := map[string]*tally{}
	for i := range clips {
		if !has[i] {
			continue
		}
		t := groups[sig[i]]
		if t == nil {
			t = &tally{byDeg: map[int]int{}}
			groups[sig[i]] = t
		}
		t.n++
		t.byDeg[decided[i]]++
	}
	groupDeg := map[string]int{}
	for k, t := range groups {
		if t.n < 2 {
			continue
		}
		for deg, n := range t.byDeg {
			if float64(n) >= 0.6*float64(t.n) {
				groupDeg[k] = deg
			}
		}
	}
	fixes := map[string]int{}
	for i, c := range clips {
		deg := decided[i]
		if !has[i] {
			deg = groupDeg[sig[i]] // zero value (no rotation) when the group has no clear answer
		}
		if deg != 0 {
			fixes[c] = deg
		}
	}
	return fixes, nil
}

// Vision implements Detector with any llm backend. It expects each image to be
// the four-panel strip made by makePanels.
type Vision struct{ AI llm.Asker }

func (v Vision) Detect(ctx context.Context, frames map[string]string) (map[string]*int, error) {
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString("You are checking the orientation of video frames from real estate footage (interiors and exteriors of buildings).\n")
	b.WriteString("You are shown images labelled ")
	b.WriteString(strings.Join(ids, ", "))
	b.WriteString(". Each image is ONE frame shown four ways, as four square panels from left to right labelled A, B, C, D ")
	b.WriteString("(grey bars are just padding). The panels are the same picture turned differently.\n\n")
	b.WriteString("For each image, say which single panel is the right way up, the way a person standing in the room would see it: ")
	b.WriteString("floors at the bottom, ceilings and light fixtures at the top, walls and door frames and window edges vertical, ")
	b.WriteString("the sky above the ground. Exactly one panel should look natural; the others look tipped or upside down.\n")
	b.WriteString("If no panel is clearly right (a blank wall, a plain surface, a blur), answer null for that image. Do not guess.\n")
	b.WriteString("\nRespond with ONLY a JSON object mapping label to one of \"A\",\"B\",\"C\",\"D\" or null, e.g. {\"c0f0\":\"B\",\"c0f1\":null}\n")

	images := make([]llm.Image, 0, len(ids))
	for _, id := range ids {
		images = append(images, llm.Image{Label: id, Path: frames[id]})
	}
	reply, err := v.AI.Ask(ctx, b.String(), images)
	if err != nil {
		return nil, err
	}
	raw, err := llm.JSONObject(reply)
	if err != nil {
		return nil, err
	}
	var letters map[string]*string
	if err := json.Unmarshal([]byte(raw), &letters); err != nil {
		return nil, fmt.Errorf("orientation reply not valid: %w", err)
	}
	res := make(map[string]*int, len(letters))
	for id, l := range letters {
		if l == nil {
			res[id] = nil
			continue
		}
		deg, ok := panelDegrees[strings.ToUpper(strings.TrimSpace(*l))]
		if !ok {
			res[id] = nil
			continue
		}
		res[id] = &deg
	}
	return res, nil
}

// panelDegrees maps the panel letters in a montage to the clockwise rotation
// that panel shows: A as stored, B turned 90 clockwise, C upside down, D turned
// 90 counter-clockwise (270 clockwise).
var panelDegrees = map[string]int{"A": 0, "B": 90, "C": 180, "D": 270}

// makePanels writes src as a 4-panel strip: A 0, B 90, C 180, D 270.
func makePanels(src, dst string) error {
	const cell = 256
	fit := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=0x808080", cell, cell, cell, cell)
	graph := "[0:v]split=4[a][b][c][d];" +
		"[a]" + fit + "[pa];" +
		"[b]transpose=1," + fit + "[pb];" +
		"[c]hflip,vflip," + fit + "[pc];" +
		"[d]transpose=2," + fit + "[pd];" +
		"[pa][pb][pc][pd]hstack=inputs=4"
	b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-i", src, "-filter_complex", graph,
		"-frames:v", "1", "-q:v", "3", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("make panels %s: %w\n%s", src, err, b)
	}
	return nil
}
