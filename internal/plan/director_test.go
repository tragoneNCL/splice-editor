package plan

import (
	"context"
	"math"
	"testing"

	"filmeditor/internal/llm"
	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

func shot(start, end, score, motion float64, group int) understand.Shot {
	return understand.Shot{Start: start, End: end, Score: score, Motion: motion, Detail: 5, Peak: (start + end) / 2, Group: group}
}

func clip(path string, dur float64, shots ...understand.Shot) understand.ClipSummary {
	return understand.ClipSummary{Path: path, Duration: dur, Width: 1080, Height: 1920, Shots: shots}
}

func inputOf(target float64, clips ...understand.ClipSummary) Input {
	r := schema.DefaultRecipe()
	r.TargetDuration = target
	return Input{Recipe: r, Clips: clips}
}

func build(t *testing.T, d Director, in Input) schema.EDL {
	t.Helper()
	edl, err := d.Plan(context.Background(), in, "")
	if err != nil {
		t.Fatal(err)
	}
	return Repair(edl, in.Clips)
}

func total(e schema.EDL) float64 {
	var t float64
	for i, c := range e.Cuts {
		t += c.Duration()
		if i > 0 && c.Transition != "" {
			t -= c.TransitionDur
		}
	}
	return t
}

// Ten usable shots over ~40s of footage, no AI.
func tour() Input {
	var cs []understand.ClipSummary
	for i := 0; i < 10; i++ {
		cs = append(cs, clip("c"+itoa(i)+".mp4", 5, shot(0, 5, 0.5+float64(i)*0.04, 10, 0)))
	}
	return inputOf(20, cs...)
}

func TestLengthHitsTheTargetWhenThereIsEnoughFootage(t *testing.T) {
	e := build(t, Director{}, tour())
	if got := total(e); math.Abs(got-20) > 1.5 {
		t.Errorf("length = %.1fs, want about 20s (%d cuts)", got, len(e.Cuts))
	}
}

func TestShortFootageGivesAShorterVideoNotPadding(t *testing.T) {
	in := inputOf(30, clip("a.mp4", 4, shot(0, 4, 0.7, 10, 0)), clip("b.mp4", 3, shot(0, 3, 0.6, 10, 0)))
	e := build(t, Director{}, in)
	if got := total(e); got > 8 {
		t.Errorf("only ~7s of footage exists but the edit is %.1fs: it repeated material", got)
	}
	// Nothing shown twice: windows from one clip must not overlap.
	for i := range e.Cuts {
		for j := i + 1; j < len(e.Cuts); j++ {
			a, b := e.Cuts[i], e.Cuts[j]
			if a.Clip == b.Clip && math.Min(a.End, b.End)-math.Max(a.Start, b.Start) > 0.05 {
				t.Errorf("cuts %d and %d show the same moment: %+v %+v", i, j, a, b)
			}
		}
	}
}

func TestEveryCutStaysInsideUsableFootage(t *testing.T) {
	// A clip whose first 3s were dead: its only shot starts at 3.
	in := inputOf(10, clip("a.mp4", 8, shot(3, 8, 0.7, 10, 0)), clip("b.mp4", 6, shot(1, 6, 0.6, 10, 0)))
	e := build(t, Director{}, in)
	for _, c := range e.Cuts {
		for _, cl := range in.Clips {
			if cl.Path != c.Clip {
				continue
			}
			ok := false
			for _, s := range cl.Shots {
				if c.Start >= s.Start-0.01 && c.End <= s.End+0.01 {
					ok = true
				}
			}
			if !ok {
				t.Errorf("cut %+v is outside the clip's usable shots %+v", c, cl.Shots)
			}
		}
	}
}

func TestLookAlikeShotsAreUsedOnce(t *testing.T) {
	in := inputOf(10,
		clip("a.mp4", 5, shot(0, 5, 0.9, 10, 1)), // two takes of the same view: group 1
		clip("b.mp4", 5, shot(0, 5, 0.5, 10, 1)),
		clip("c.mp4", 5, shot(0, 5, 0.6, 10, 0)),
		clip("d.mp4", 5, shot(0, 5, 0.6, 10, 0)),
	)
	e := build(t, Director{}, in)
	used := map[string]bool{}
	for _, c := range e.Cuts {
		used[c.Clip] = true
	}
	if used["a.mp4"] && used["b.mp4"] {
		t.Errorf("both takes of the same view were used: %v", used)
	}
	if !used["a.mp4"] {
		t.Errorf("the better take should have been kept: %v", used)
	}
}

func TestOpensOnTheBestShotAndHonoursTheRequestedHook(t *testing.T) {
	in := inputOf(12,
		clip("a.mp4", 5, shot(0, 5, 0.5, 10, 0)),
		clip("b.mp4", 5, shot(0, 5, 0.95, 10, 0)),
		clip("c.mp4", 5, shot(0, 5, 0.6, 10, 0)),
	)
	e := build(t, Director{}, in)
	if e.Cuts[0].Clip != "b.mp4" {
		t.Errorf("opened on %s, want the strongest shot b.mp4", e.Cuts[0].Clip)
	}
	labels := map[string]ShotLabel{
		ShotKey(0, 0): {Room: "kitchen", Wow: 3},
		ShotKey(1, 0): {Room: "bedroom", Wow: 3},
		ShotKey(2, 0): {Room: "outdoor", Wow: 3},
	}
	e = build(t, Director{Labels: labels, Style: Directive{HookRoom: "kitchen"}}, in)
	if e.Cuts[0].Clip != "a.mp4" {
		t.Errorf("asked to open on the kitchen, opened on %s", e.Cuts[0].Clip)
	}
}

func TestToursFollowRoomOrderAfterTheHook(t *testing.T) {
	in := inputOf(15,
		clip("bath.mp4", 5, shot(0, 5, 0.6, 10, 0)),
		clip("kitchen.mp4", 5, shot(0, 5, 0.6, 10, 0)),
		clip("entry.mp4", 5, shot(0, 5, 0.6, 10, 0)),
		clip("pool.mp4", 5, shot(0, 5, 0.9, 10, 0)),
	)
	labels := map[string]ShotLabel{
		ShotKey(0, 0): {Room: "bathroom", Wow: 2},
		ShotKey(1, 0): {Room: "kitchen", Wow: 3},
		ShotKey(2, 0): {Room: "entry", Wow: 2},
		ShotKey(3, 0): {Room: "outdoor", Wow: 5},
	}
	e := build(t, Director{Labels: labels}, in)
	var order []string
	for _, c := range e.Cuts {
		order = append(order, c.Clip)
	}
	want := []string{"pool.mp4", "entry.mp4", "kitchen.mp4", "bath.mp4"} // hook, then entry -> kitchen -> bathroom
	if len(order) != 4 {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestOneRoomDoesNotTakeOverTheTour(t *testing.T) {
	var cs []understand.ClipSummary
	labels := map[string]ShotLabel{}
	for i := 0; i < 5; i++ {
		cs = append(cs, clip("bath"+itoa(i)+".mp4", 5, shot(0, 5, 0.7, 10, 0)))
		labels[ShotKey(i, 0)] = ShotLabel{Room: "bathroom", Wow: 3}
	}
	cs = append(cs, clip("kitchen.mp4", 5, shot(0, 5, 0.6, 10, 0)), clip("pool.mp4", 5, shot(0, 5, 0.6, 10, 0)))
	labels[ShotKey(5, 0)] = ShotLabel{Room: "kitchen", Wow: 3}
	labels[ShotKey(6, 0)] = ShotLabel{Room: "outdoor", Wow: 3}
	e := build(t, Director{Labels: labels}, inputOf(12, cs...))
	baths := 0
	for _, c := range e.Cuts {
		if len(c.Clip) >= 4 && c.Clip[:4] == "bath" {
			baths++
		}
	}
	if baths > maxPerRoom {
		t.Errorf("%d bathroom shots in a tour with a kitchen and a pool available", baths)
	}
}

func TestPacingVariesAndTheHookIsHeld(t *testing.T) {
	e := build(t, Director{}, tour())
	var minD, maxD = math.MaxFloat64, 0.0
	for _, c := range e.Cuts {
		d := c.Duration()
		minD, maxD = math.Min(minD, d), math.Max(maxD, d)
		if d < 0.85 || d > 5.05 {
			t.Errorf("cut length %.1fs is outside the sane range", d)
		}
	}
	if maxD-minD < 0.3 {
		t.Errorf("every cut is about %.1fs: no pacing variation", minD)
	}
	if first := e.Cuts[0].Duration(); first < total(e)/float64(len(e.Cuts)) {
		t.Errorf("the opening shot (%.1fs) should be held at least as long as the average", first)
	}
}

func TestTransitionStyleIsObeyed(t *testing.T) {
	in := tour()
	hard := build(t, Director{Style: Directive{Transitions: "hard"}}, in)
	for _, c := range hard.Cuts {
		if c.Transition != "" {
			t.Errorf("hard-cut style produced %q", c.Transition)
		}
	}
	soft := build(t, Director{Style: Directive{Transitions: "dissolve"}}, in)
	dissolves := 0
	for i, c := range soft.Cuts {
		if c.Transition == "dissolve" {
			dissolves++
		}
		if i == 0 && c.Transition != "" {
			t.Errorf("the first cut must not have a transition")
		}
	}
	if dissolves < len(soft.Cuts)-2 {
		t.Errorf("dissolve style used only %d dissolves in %d cuts", dissolves, len(soft.Cuts))
	}
}

func TestEnergyChangesTheCutLength(t *testing.T) {
	calm := build(t, Director{Style: Directive{Energy: "calm"}}, tour())
	fast := build(t, Director{Style: Directive{Energy: "energetic"}}, tour())
	if len(fast.Cuts) <= len(calm.Cuts) {
		t.Errorf("energetic made %d cuts, calm made %d: energetic should cut more", len(fast.Cuts), len(calm.Cuts))
	}
}

func TestRepairPullsABlankCutIntoUsableFootageAndDropsEmptyClips(t *testing.T) {
	clips := []understand.ClipSummary{
		clip("a.mp4", 8, shot(3, 8, 0.7, 10, 0)),
		{Path: "empty.mp4", Duration: 5}, // analysed, nothing usable
	}
	bad := schema.EDL{Cuts: []schema.Cut{
		{Clip: "a.mp4", Start: 0, End: 2.5, Caption: "keep me", Transition: "dissolve", TransitionDur: 0.4},
		{Clip: "empty.mp4", Start: 0, End: 2},
	}}
	got := Repair(bad, clips)
	if len(got.Cuts) != 1 {
		t.Fatalf("cuts = %+v", got.Cuts)
	}
	c := got.Cuts[0]
	if c.Start < 3 || c.End > 8 || math.Abs((c.End-c.Start)-2.5) > 0.05 {
		t.Errorf("cut was not moved into the usable part intact: %+v", c)
	}
	if c.Caption != "keep me" {
		t.Errorf("repair dropped the caption: %+v", c)
	}
	if c.Transition != "" {
		t.Errorf("a transition into the very first cut is meaningless and must be cleared: %+v", c)
	}
}

func TestLabelShotsRejectsInventedIdsAndClampsValues(t *testing.T) {
	cs := []understand.ClipSummary{clip("a.mp4", 5, func() understand.Shot { s := shot(0, 5, 0.6, 10, 0); s.Thumb = "/tmp/x.jpg"; return s }())}
	ai := stubAI(`{"shots":{"c0s0":{"room":"Kitchen","wow":9},"c9s9":{"room":"pool","wow":5}},"style":{"avg_shot":99,"transitions":"wobble","energy":"calm","hook_room":"POOL"}}`)
	labels, style, err := LabelShots(context.Background(), ai, "calm", cs)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels["c0s0"].Room != "kitchen" || labels["c0s0"].Wow != 5 {
		t.Errorf("labels = %+v", labels)
	}
	if style.AvgShot != 0 || style.Transitions != "" || style.Energy != "calm" || style.HookRoom != "outdoor" && style.HookRoom != "" {
		t.Errorf("style = %+v", style)
	}
}

type stubAI string

func (s stubAI) Ask(_ context.Context, _ string, _ []llm.Image) (string, error) {
	return string(s), nil
}
