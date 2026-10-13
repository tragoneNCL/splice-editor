package revise

import (
	"context"
	"strings"
	"testing"

	"filmeditor/internal/llm"
	"filmeditor/internal/schema"
)

type scripted struct {
	replies []string
	prompts []string
}

func (s *scripted) Ask(_ context.Context, p string, _ []llm.Image) (string, error) {
	s.prompts = append(s.prompts, p)
	r := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	return r, nil
}

var base = schema.EDL{Cuts: []schema.Cut{{Clip: "a.mp4", Start: 0, End: 4}, {Clip: "b.mp4", Start: 0, End: 3}}}

func TestAppliesValidRevision(t *testing.T) {
	ai := &scripted{replies: []string{"Sure!\n```json\n" +
		`{"cuts":[{"clip":"a.mp4","start":0,"end":2},{"clip":"b.mp4","start":0,"end":3,"speed":0.5}],"texts":[{"text":"Summer","start":0,"end":2}]}` +
		"\n```"}}
	got, err := Apply(context.Background(), ai, base, "shorten the first shot, slow-mo the second, title Summer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cuts[0].End != 2 || got.Cuts[1].Speed != 0.5 || len(got.Texts) != 1 {
		t.Errorf("unexpected result %+v", got)
	}
	if !strings.Contains(ai.prompts[0], `"a.mp4"`) || !strings.Contains(ai.prompts[0], "Summer") {
		t.Error("prompt should include the current project and the instruction")
	}
}

func TestRejectsInventedClipThenRetriesWithFeedback(t *testing.T) {
	ai := &scripted{replies: []string{
		`{"cuts":[{"clip":"invented.mp4","start":0,"end":2}]}`,
		`{"cuts":[{"clip":"a.mp4","start":0,"end":2}]}`,
	}}
	got, err := Apply(context.Background(), ai, base, "just the first shot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cuts) != 1 || got.Cuts[0].Clip != "a.mp4" {
		t.Errorf("got %+v", got)
	}
	if !strings.Contains(ai.prompts[1], "unknown clip") {
		t.Error("retry prompt should carry the rejection reason")
	}
}

func TestGivesUpAfterRepeatedBadAnswers(t *testing.T) {
	ai := &scripted{replies: []string{`{"cuts":[{"clip":"a.mp4","start":5,"end":1}]}`}}
	if _, err := Apply(context.Background(), ai, base, "x"); err == nil {
		t.Error("expected error after invalid answers")
	}
}
