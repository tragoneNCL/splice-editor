package plan

import (
	"context"
	"errors"
	"testing"

	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

func input() Input {
	r := schema.DefaultRecipe()
	r.TargetDuration = 10
	r.AvgShotLength = 2
	return Input{
		Recipe: r,
		Clips: []understand.ClipSummary{
			{Path: "a.mp4", Duration: 20, Shots: []understand.Shot{
				{Start: 0, End: 5, Score: 0.4},
				{Start: 5, End: 12, Score: 0.9},
				{Start: 12, End: 20, Score: 0.6},
			}},
			{Path: "b.mp4", Duration: 10, Shots: []understand.Shot{
				{Start: 0, End: 10, Score: 0.8},
			}},
		},
	}
}

func TestRuleBasedMeetsTargetAndOpensOnBestShot(t *testing.T) {
	in := input()
	edl, err := RuleBased{}.Plan(context.Background(), in, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(edl, in); err != nil {
		t.Fatalf("rule-based edl failed check: %v", err)
	}
	first := edl.Cuts[0]
	if first.Clip != "a.mp4" || first.Start < 5 || first.End > 12 {
		t.Errorf("hook = %+v, want a cut inside the best shot a.mp4 5-12", first)
	}
}

func TestCheckRejectsBadEdits(t *testing.T) {
	in := input()
	cases := map[string]schema.EDL{
		"unknown clip": {Cuts: []schema.Cut{{Clip: "nope.mp4", Start: 0, End: 10}}},
		"past end":     {Cuts: []schema.Cut{{Clip: "b.mp4", Start: 5, End: 40}}},
		"too short":    {Cuts: []schema.Cut{{Clip: "a.mp4", Start: 0, End: 2}}},
		"empty":        {},
	}
	for name, edl := range cases {
		if err := Check(edl, in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

type fakePlanner struct {
	edl      schema.EDL
	err      error
	calls    int
	feedback []string
}

func (f *fakePlanner) Name() string { return "fake" }
func (f *fakePlanner) Plan(_ context.Context, _ Input, fb string) (schema.EDL, error) {
	f.calls++
	f.feedback = append(f.feedback, fb)
	return f.edl, f.err
}

func TestRunRetriesWithFeedbackThenFallsBack(t *testing.T) {
	in := input()
	bad := &fakePlanner{edl: schema.EDL{Cuts: []schema.Cut{{Clip: "nope.mp4", Start: 0, End: 10}}}}
	edl, used, err := Run(context.Background(), in, bad)
	if err != nil {
		t.Fatal(err)
	}
	if used != "rule-based" {
		t.Errorf("used = %q, want rule-based fallback", used)
	}
	if bad.calls != attemptsPerPlanner {
		t.Errorf("planner called %d times, want %d", bad.calls, attemptsPerPlanner)
	}
	if bad.feedback[0] != "" || bad.feedback[1] == "" {
		t.Errorf("feedback = %q, want empty first then the validation error", bad.feedback)
	}
	if err := Check(edl, in); err != nil {
		t.Errorf("fallback edl invalid: %v", err)
	}
}

func TestRunFallsBackWhenPlannerErrors(t *testing.T) {
	_, used, err := Run(context.Background(), input(), &fakePlanner{err: errors.New("boom")})
	if err != nil || used != "rule-based" {
		t.Fatalf("used=%q err=%v, want rule-based and no error", used, err)
	}
}

func TestParseEDLToleratesProse(t *testing.T) {
	edl, err := parseEDL("Here you go:\n```json\n{\"cuts\":[{\"clip\":\"a.mp4\",\"start\":1,\"end\":3}]}\n```")
	if err != nil || len(edl.Cuts) != 1 {
		t.Fatalf("edl=%+v err=%v", edl, err)
	}
	if _, err := parseEDL("sorry, no"); err == nil {
		t.Error("expected error for reply without JSON")
	}
}
