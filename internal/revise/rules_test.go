package revise

import (
	"strings"
	"testing"

	"filmeditor/internal/schema"
)

func four() schema.EDL {
	return schema.EDL{Cuts: []schema.Cut{
		{Clip: "a", Start: 0, End: 4},
		{Clip: "b", Start: 0, End: 4},
		{Clip: "c", Start: 0, End: 4},
		{Clip: "d", Start: 0, End: 4},
	}}
}

func TestRules(t *testing.T) {
	cases := []struct {
		say   string
		check func(t *testing.T, e schema.EDL)
	}{
		{"make the second shot slow motion", func(t *testing.T, e schema.EDL) {
			if e.Cuts[1].Speed != 0.5 || e.Cuts[0].Speed != 0 {
				t.Errorf("speeds: %+v", e.Cuts)
			}
		}},
		{"Slow-mo the last shot", func(t *testing.T, e schema.EDL) {
			if e.Cuts[3].Speed != 0.5 {
				t.Error("last not slowed")
			}
		}},
		{"speed up shot 3", func(t *testing.T, e schema.EDL) {
			if e.Cuts[2].Speed != 2 {
				t.Error("shot 3 not sped up")
			}
		}},
		{"make it shorter and punchier", func(t *testing.T, e schema.EDL) {
			if e.Cuts[0].End != 3 {
				t.Errorf("end = %v, want 3", e.Cuts[0].End)
			}
		}},
		{"mute the clips, keep the music", func(t *testing.T, e schema.EDL) {
			for i, c := range e.Cuts {
				if c.Volume == nil || *c.Volume != 0 {
					t.Errorf("cut %d not muted", i)
				}
			}
		}},
		{"remove the third shot", func(t *testing.T, e schema.EDL) {
			if len(e.Cuts) != 3 || e.Cuts[2].Clip != "d" {
				t.Errorf("cuts: %+v", e.Cuts)
			}
		}},
		{"add a title: Summer 2026, big and bold", func(t *testing.T, e schema.EDL) {
			if len(e.Texts) != 1 || !strings.HasPrefix(e.Texts[0].Text, "Summer 2026") {
				t.Errorf("texts: %+v", e.Texts)
			}
		}},
		{`add a caption "golden hour" to the last shot`, func(t *testing.T, e schema.EDL) {
			if e.Cuts[3].Caption != "golden hour" {
				t.Errorf("caption: %q", e.Cuts[3].Caption)
			}
		}},
		{"make the first shot black and white and fade out", func(t *testing.T, e schema.EDL) {
			if e.Cuts[0].Color == nil || !e.Cuts[0].Color.Grayscale || e.FadeOut == 0 {
				t.Errorf("got %+v fadeout=%v", e.Cuts[0].Color, e.FadeOut)
			}
		}},
		{"add dissolves", func(t *testing.T, e schema.EDL) {
			if e.Cuts[1].Transition != "dissolve" || e.Cuts[0].Transition != "" {
				t.Error("transitions wrong")
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.say, func(t *testing.T) {
			got, summary, err := Rules(four(), c.say)
			if err != nil {
				t.Fatal(err)
			}
			if summary == "" {
				t.Error("empty summary")
			}
			c.check(t, got)
		})
	}
}

func TestRulesAreAllOrNothingAndHelpful(t *testing.T) {
	orig := four()
	got, _, err := Rules(orig, "slow-mo the first shot and make it feel like a dream")
	if err == nil {
		t.Fatal("expected an error for the unsupported clause")
	}
	if !strings.Contains(err.Error(), "Connect an AI key") {
		t.Errorf("error should explain what to do: %v", err)
	}
	if got.Cuts[0].Speed != 0 {
		t.Error("partial changes must not leak out")
	}
	if orig.Cuts[0].Speed != 0 {
		t.Error("input was mutated")
	}
}

func TestRulesBounds(t *testing.T) {
	for _, s := range []string{"slow-mo shot 9", "remove everything", "remove the fifth shot"} {
		if _, _, err := Rules(four(), s); err == nil {
			t.Errorf("%q: expected an error", s)
		}
	}
}
