// Package plan decides what goes in the edit. Planners (rule-based, Claude CLI,
// later Gemini) all produce an EDL; Run validates it and retries with feedback,
// and always falls back to the rule-based planner so a job never dead-ends.
package plan

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"filmeditor/internal/llm"
	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

// Input is everything a planner is given.
type Input struct {
	Prompt string
	Recipe schema.Recipe
	Clips  []understand.ClipSummary
}

// Planner produces an EDL. feedback is empty on the first attempt, and on
// retries describes what was wrong with the previous answer.
type Planner interface {
	Name() string
	Plan(ctx context.Context, in Input, feedback string) (schema.EDL, error)
}

const (
	attemptsPerPlanner = 2
	durationTolerance  = 0.25 // result must be within ±25% of target
)

// Check validates an EDL against the real footage and the recipe.
func Check(edl schema.EDL, in Input) error {
	if err := edl.Validate(); err != nil {
		return err
	}
	durations := map[string]float64{}
	var available float64
	for _, c := range in.Clips {
		durations[c.Path] = c.Duration
		available += c.Duration
	}
	var total float64
	for i, c := range edl.Cuts {
		d, ok := durations[c.Clip]
		if !ok {
			return fmt.Errorf("cut %d: unknown clip %q (use only the provided paths)", i, c.Clip)
		}
		if c.End > d+0.05 {
			return fmt.Errorf("cut %d: end %.2f is past the end of %s (%.2fs)", i, c.End, c.Clip, d)
		}
		total += c.Duration()
	}
	// Short of footage is acceptable; being far off target otherwise is not.
	target := in.Recipe.TargetDuration
	if want := minF(target, available); total < want*(1-durationTolerance) || total > target*(1+durationTolerance) {
		return fmt.Errorf("total length %.1fs is outside the target %.1fs ±%d%%", total, target, int(durationTolerance*100))
	}
	return nil
}

// Run tries each planner in order, validating and retrying with feedback.
// The rule-based planner is appended as a guaranteed fallback.
func Run(ctx context.Context, in Input, planners ...Planner) (schema.EDL, string, error) {
	planners = append(planners, RuleBased{})
	for _, p := range planners {
		feedback := ""
		for attempt := 1; attempt <= attemptsPerPlanner; attempt++ {
			edl, err := p.Plan(ctx, in, feedback)
			if err == nil {
				err = Check(edl, in)
			}
			if err == nil {
				edl.Fixes = fixesOf(in.Clips)
				return edl, p.Name(), nil
			}
			log.Printf("plan: %s attempt %d failed: %v", p.Name(), attempt, err)
			feedback = err.Error()
		}
	}
	return schema.EDL{}, "", fmt.Errorf("no planner produced a valid edit (is there any footage?)")
}

// RuleBased needs no AI: open on the best shot, then keep the strongest
// shots in chronological order until the target length is reached.
type RuleBased struct{}

func (RuleBased) Name() string { return "rule-based" }

type candidate struct {
	clip  string
	shot  understand.Shot
	order int
}

func (RuleBased) Plan(_ context.Context, in Input, _ string) (schema.EDL, error) {
	piece := in.Recipe.AvgShotLength
	if piece <= 0 {
		piece = 2.5
	}

	// A long shot is split into several pieces so one good shot can fill
	// more than a single beat of the edit.
	var cands []candidate
	for ci, c := range in.Clips {
		for _, s := range c.Shots {
			n := int(s.Length() / piece)
			if n < 1 {
				n = 1
			}
			length := minF(s.Length(), piece)
			slack := s.Length() - float64(n)*length
			for k := 0; k < n; k++ {
				start := s.Start + slack/2 + float64(k)*length
				cands = append(cands, candidate{
					clip:  c.Path,
					shot:  understand.Shot{Start: start, End: start + length, Score: s.Score},
					order: ci*100000 + int(start*100),
				})
			}
		}
	}
	if len(cands) == 0 {
		return schema.EDL{}, fmt.Errorf("no shots to edit")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].shot.Score > cands[j].shot.Score })

	var chosen []candidate
	var total float64
	for _, c := range cands {
		if total >= in.Recipe.TargetDuration {
			break
		}
		chosen = append(chosen, c)
		total += c.shot.Length()
	}

	hook, rest := chosen[0], chosen[1:]
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].order < rest[j].order })
	ordered := append([]candidate{hook}, rest...)

	var edl schema.EDL
	var used float64
	for _, c := range ordered {
		length := c.shot.Length()
		if over := used + length - in.Recipe.TargetDuration; over > 0 {
			length -= over
		}
		if length < 0.3 {
			continue
		}
		edl.Cuts = append(edl.Cuts, schema.Cut{Clip: c.clip, Start: round2(c.shot.Start), End: round2(c.shot.Start + length)})
		used += length
	}
	return edl, nil
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

func fixesOf(clips []understand.ClipSummary) map[string]int {
	var fixes map[string]int
	for _, c := range clips {
		if c.Fix != 0 {
			if fixes == nil {
				fixes = map[string]int{}
			}
			fixes[c.Path] = c.Fix
		}
	}
	return fixes
}

// summarize renders clip summaries compactly for prompts, and returns the
// frames referenced by label (f0, f1, ...).
func summarize(clips []understand.ClipSummary) (string, []llm.Image) {
	var b strings.Builder
	var images []llm.Image
	keep := topShots(clips, maxPromptShots)
	for ci, c := range clips {
		if !keep[ci].any {
			continue
		}
		fmt.Fprintf(&b, "clip %q duration=%.1fs %dx%d audio=%v\n", c.Path, c.Duration, c.Width, c.Height, c.HasAudio)
		for si, s := range c.Shots {
			if !keep[ci].shots[si] {
				continue
			}
			fmt.Fprintf(&b, "  shot %.2f-%.2f score=%.2f", s.Start, s.End, s.Score)
			if s.Thumb != "" {
				label := fmt.Sprintf("f%d", len(images))
				images = append(images, llm.Image{Label: label, Path: s.Thumb})
				fmt.Fprintf(&b, " frame=%s", label)
			}
			b.WriteString("\n")
		}
	}
	return b.String(), images
}

// maxPromptShots bounds what the model is shown: with hours of footage there
// can be thousands of shots, which would blow up cost and latency for no gain.
// The strongest shots are kept; the rule-based planner still sees everything.
const maxPromptShots = 60

type kept struct {
	any   bool
	shots map[int]bool
}

func topShots(clips []understand.ClipSummary, limit int) []kept {
	type ref struct {
		ci, si int
		score  float64
	}
	var all []ref
	for ci, c := range clips {
		for si, s := range c.Shots {
			all = append(all, ref{ci, si, s.Score})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]kept, len(clips))
	for i := range out {
		out[i].shots = map[int]bool{}
	}
	for _, r := range all {
		out[r.ci].any = true
		out[r.ci].shots[r.si] = true
	}
	return out
}
