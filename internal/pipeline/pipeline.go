// Package pipeline is the whole hands-off edit as one call: check
// orientation, understand the footage, plan, render. The CLI and the web
// server both use it.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"filmeditor/internal/inspo"
	"filmeditor/internal/llm"
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

	// Inspiration: a reference video (file) and/or a YouTube link. Only its
	// pacing is used; the footage is deleted after it is measured.
	Inspo    string
	InspoURL string
	// StyleAI, if set, looks at a few reference frames to describe the style.
	StyleAI llm.Asker

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
	// Notes are non-fatal things the user should know ("couldn't fetch the
	// reference video, edited from your brief only").
	Notes []string
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

	var notes []string
	prompt := o.Prompt
	recipe := o.Recipe

	// Reference video -> pacing numbers + a sentence for the brief.
	refPath := o.Inspo
	if refPath == "" && o.InspoURL != "" {
		o.report("Fetching your reference video", 0.03)
		p, err := inspo.FetchYouTube(ctx, o.InspoURL, o.WorkDir)
		if err != nil {
			notes = append(notes, "Couldn't use the reference video ("+err.Error()+"). Edited from your brief instead.")
		} else {
			refPath = p
		}
	}
	if refPath != "" {
		o.report("Studying your reference", 0.06)
		st, err := inspo.Analyze(refPath, o.WorkDir)
		if err != nil {
			notes = append(notes, "Couldn't read the reference video. Edited from your brief instead.")
		} else {
			st.Apply(&recipe)
			if d := st.Describe(); d != "" {
				prompt = strings.TrimSpace(d + " " + prompt)
			}
			if o.StyleAI != nil {
				if extra := describeStyle(ctx, o.StyleAI, refPath, o.WorkDir); extra != "" {
					prompt = strings.TrimSpace(prompt + " Reference style: " + extra)
				}
			}
		}
		// The reference is only measured, never kept.
		if o.InspoURL != "" && o.Inspo == "" {
			os.Remove(refPath)
		}
	}

	var fixes map[string]int
	if o.Vision != nil {
		o.report("Checking orientation", 0.08)
		var err error
		fixes, err = orient.Fix(ctx, o.Vision, o.Clips, filepath.Join(o.WorkDir, "orient"))
		if err != nil {
			// A failed safety net must not fail the job: carry on without it.
			fixes = nil
		}
	}

	// Analyse clips in parallel: this is the slow part on large footage.
	clips := make([]understand.ClipSummary, len(o.Clips))
	errs := make([]error, len(o.Clips))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	sem := make(chan struct{}, analysisWorkers)
	for i, p := range o.Clips {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			c, err := understand.Analyze(p)
			if err == nil {
				c.Fix = fixes[p]
				if o.Vision != nil {
					err = c.Thumbnails(filepath.Join(o.WorkDir, "frames"))
				}
			}
			clips[i], errs[i] = c, err
			mu.Lock()
			done++
			o.report(fmt.Sprintf("Analyzing clips (%d of %d)", done, len(o.Clips)),
				0.1+0.45*float64(done)/float64(len(o.Clips)))
			mu.Unlock()
		}(i, p)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", filepath.Base(o.Clips[i]), err)
		}
	}

	o.report("Planning the edit", 0.55)
	edl, used, err := plan.Run(ctx, plan.Input{Prompt: prompt, Recipe: recipe, Clips: clips}, o.Planners...)
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
	if err := render.Render(edl, recipe, o.Out); err != nil {
		return Result{}, err
	}
	o.report("Done", 1)
	return Result{EDL: edl, Planner: used, Notes: notes}, nil
}

// analysisWorkers bounds concurrent ffmpeg analyses; each already uses
// several threads.
const analysisWorkers = 3

// describeStyle shows the model a few evenly spaced reference frames and asks
// for a short description of the look. Failure is silent: it only enriches.
func describeStyle(ctx context.Context, ai llm.Asker, ref, work string) string {
	info, err := understand.Analyze(ref)
	if err != nil || info.Duration <= 0 {
		return ""
	}
	var images []llm.Image
	for i := 0; i < 5; i++ {
		t := info.Duration * (float64(i) + 0.5) / 5
		out := filepath.Join(work, fmt.Sprintf("ref_%d.jpg", i))
		if err := understand.Frame(ref, t, 0, out); err != nil {
			continue
		}
		images = append(images, llm.Image{Label: fmt.Sprintf("r%d", i), Path: out})
	}
	if len(images) == 0 {
		return ""
	}
	reply, err := ai.Ask(ctx, "These are frames from a reference video, in order. In two short sentences, describe its visual "+
		"style for a video editor to imitate: colour grade, framing, subject matter, mood. No preamble, plain text.", images)
	if err != nil {
		return ""
	}
	reply = strings.TrimSpace(reply)
	if len(reply) > 400 {
		reply = reply[:400]
	}
	return reply
}
