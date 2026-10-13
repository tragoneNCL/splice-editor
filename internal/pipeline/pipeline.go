// Package pipeline is the whole hands-off edit as one call: check
// orientation, understand the footage, plan, render. The CLI and the web
// server both use it.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"filmeditor/internal/orient"
	"filmeditor/internal/plan"
	"filmeditor/internal/render"
	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

type Options struct {
	Clips  []string
	Prompt string
	Recipe schema.Recipe
	Out    string // output mp4 path
	// WorkDir holds sampled frames; it must exist and is the caller's to clean.
	WorkDir string

	// Planners are tried in order; the rule-based planner is always the last
	// resort. Vision (optional) enables the orientation check and gives
	// thumbnails to the planner.
	Planners []plan.Planner
	Vision   orient.Detector

	Music string
	Duck  bool
	Title string
	Fade  float64

	// Progress, if set, is told what stage is running and overall 0..1.
	Progress func(stage string, fraction float64)
}

type Result struct {
	EDL     schema.EDL
	Planner string
}

func (o Options) report(stage string, f float64) {
	if o.Progress != nil {
		o.Progress(stage, f)
	}
}

// Run executes the pipeline and writes o.Out. The returned EDL is the
// project file: save it to make further edits or revisions.
func Run(ctx context.Context, o Options) (Result, error) {
	if len(o.Clips) == 0 {
		return Result{}, fmt.Errorf("no clips")
	}
	if o.WorkDir == "" {
		return Result{}, fmt.Errorf("work dir required")
	}

	var fixes map[string]int
	if o.Vision != nil {
		o.report("Checking orientation", 0.05)
		var err error
		fixes, err = orient.Fix(ctx, o.Vision, o.Clips, filepath.Join(o.WorkDir, "orient"))
		if err != nil {
			// A failed safety net must not fail the job: carry on without it.
			fixes = nil
		}
	}

	clips := make([]understand.ClipSummary, 0, len(o.Clips))
	for i, p := range o.Clips {
		o.report(fmt.Sprintf("Analyzing clip %d of %d", i+1, len(o.Clips)), 0.1+0.4*float64(i)/float64(len(o.Clips)))
		c, err := understand.Analyze(p)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		c.Fix = fixes[p]
		if o.Vision != nil {
			if err := c.Thumbnails(filepath.Join(o.WorkDir, "frames")); err != nil {
				return Result{}, err
			}
		}
		clips = append(clips, c)
	}

	o.report("Planning the edit", 0.55)
	edl, used, err := plan.Run(ctx, plan.Input{Prompt: o.Prompt, Recipe: o.Recipe, Clips: clips}, o.Planners...)
	if err != nil {
		return Result{}, err
	}

	edl.FadeIn, edl.FadeOut = o.Fade, o.Fade
	if o.Music != "" {
		edl.Music = &schema.Music{Path: o.Music, Duck: o.Duck, FadeOut: 1}
	}
	if o.Title != "" {
		edl.Texts = append(edl.Texts, schema.Text{Text: o.Title, Start: 0.3, End: 3, Position: "center"})
	}

	o.report("Rendering", 0.7)
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return Result{}, err
	}
	if err := render.Render(edl, o.Recipe, o.Out); err != nil {
		return Result{}, err
	}
	o.report("Done", 1)
	return Result{EDL: edl, Planner: used}, nil
}
