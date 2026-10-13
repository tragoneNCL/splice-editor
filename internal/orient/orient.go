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
// clockwise degrees (0/90/180/270) needed to make the frame upright.
type Detector interface {
	Detect(ctx context.Context, frames map[string]string) (map[string]int, error)
}

// Fix samples a frame from each clip (after container rotation), asks the
// detector, and returns clip path -> extra clockwise degrees. Clips needing no
// correction are omitted. workDir holds the sampled frames.
func Fix(ctx context.Context, d Detector, clips []string, workDir string) (map[string]int, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	frames := map[string]string{}
	idToClip := map[string]string{}
	for i, c := range clips {
		info, err := ingest.Probe(c)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("c%d", i)
		out := filepath.Join(workDir, id+".jpg")
		// 40% in: past fade-ins and slates, before the end.
		b, err := exec.Command("ffmpeg", "-y", "-v", "error", "-ss", fmt.Sprintf("%.2f", info.Duration*0.4),
			"-i", c, "-frames:v", "1", "-vf", "scale=384:-2", "-q:v", "3", out).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("sample frame %s: %w\n%s", c, err, b)
		}
		frames[id], idToClip[id] = out, c
	}
	if len(frames) == 0 {
		return nil, nil
	}
	res, err := d.Detect(ctx, frames)
	if err != nil {
		return nil, err
	}
	fixes := map[string]int{}
	for id, deg := range res {
		clip, ok := idToClip[id]
		if !ok {
			continue
		}
		if deg%90 != 0 || deg < 0 || deg > 270 {
			return nil, fmt.Errorf("detector returned invalid rotation %d for %s", deg, id)
		}
		if deg != 0 {
			fixes[clip] = deg
		}
	}
	return fixes, nil
}

// Vision implements Detector with any llm backend.
type Vision struct{ AI llm.Asker }

func (v Vision) Detect(ctx context.Context, frames map[string]string) (map[string]int, error) {
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString("You are shown images labelled ")
	b.WriteString(strings.Join(ids, ", "))
	b.WriteString(". Each is one frame from a video clip.\n")
	b.WriteString("For each, decide whether it is displayed upright, the way a viewer would expect: ")
	b.WriteString("sky above ground, objects the right way up, any text readable.\n")
	b.WriteString("If it is rotated, give the clockwise rotation in degrees (90, 180 or 270) that makes it upright.\n")
	b.WriteString("If it is upright, or has no orientation cues (abstract, symmetric, uncertain), answer 0. ")
	b.WriteString("Never guess: only rotate when you are confident.\n")
	b.WriteString("\nRespond with ONLY a JSON object mapping label to degrees, e.g. {\"c0\":0,\"c1\":180}\n")

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
	var res map[string]int
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, fmt.Errorf("orientation reply not valid: %w", err)
	}
	return res, nil
}
