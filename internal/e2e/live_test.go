// Live end-to-end checks against a real model. They cost usage and take a
// minute, so they only run with FILMEDITOR_LIVE=1:
//
//	FILMEDITOR_LIVE=1 go test ./internal/e2e -v
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filmeditor/internal/llm"
	"filmeditor/internal/orient"
	"filmeditor/internal/plan"
	"filmeditor/internal/schema"
	"filmeditor/internal/synth"
	"filmeditor/internal/understand"
)

func TestLiveOrientationAndBriefFollowing(t *testing.T) {
	if os.Getenv("FILMEDITOR_LIVE") != "1" {
		t.Skip("set FILMEDITOR_LIVE=1 to run against the real model")
	}
	dir := t.TempDir()
	set := []struct {
		name, scene string
		opt         synth.Options
	}{
		{"landscape.mp4", synth.Landscape, synth.Options{Seconds: 6}},
		{"redcircle.mp4", synth.RedCircle, synth.Options{Seconds: 4}},
		{"upside_down.mp4", synth.Landscape, synth.Options{Seconds: 5, UpsideDown: true}},
		{"greensquare_rotated.mp4", synth.GreenSquare, synth.Options{Seconds: 4, MetaRotateCCW: 90}},
		{"night.mp4", synth.Night, synth.Options{Seconds: 5}},
	}
	var paths []string
	for _, c := range set {
		p := filepath.Join(dir, c.name)
		if err := synth.WriteClip(p, c.scene, c.opt); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	ctx := context.Background()

	// 1. Orientation: exactly the untagged upside-down clip gets fixed.
	fixes, err := orient.Fix(ctx, orient.Vision{AI: llm.CLI{}}, paths, filepath.Join(dir, "orient"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		want := 0
		if strings.HasSuffix(p, "upside_down.mp4") {
			want = 180
		}
		if fixes[p] != want {
			t.Errorf("%s: fix = %d, want %d", filepath.Base(p), fixes[p], want)
		}
	}

	// 2. Brief following: the opening hook is the clip the prompt names.
	var clips []understand.ClipSummary
	for _, p := range paths {
		c, err := understand.Analyze(p)
		if err != nil {
			t.Fatal(err)
		}
		c.Fix = fixes[p]
		if err := c.Thumbnails(filepath.Join(dir, "frames")); err != nil {
			t.Fatal(err)
		}
		clips = append(clips, c)
	}
	r := schema.DefaultRecipe()
	r.TargetDuration, r.AvgShotLength = 10, 2
	in := plan.Input{Prompt: "Open on the clip with the red circle. Then show the others.", Recipe: r, Clips: clips}
	edl, used, err := plan.Run(ctx, in, plan.LLM{AI: llm.CLI{}, Label: "claude-cli"})
	if err != nil {
		t.Fatal(err)
	}
	if used != "claude-cli" {
		t.Errorf("planner = %s, want claude-cli (fell back to rules)", used)
	}
	if got := filepath.Base(edl.Cuts[0].Clip); got != "redcircle.mp4" {
		t.Errorf("opening clip = %s, want redcircle.mp4", got)
	}
}
