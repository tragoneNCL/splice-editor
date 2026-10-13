package project

import (
	"path/filepath"
	"testing"

	"filmeditor/internal/schema"
)

func three() *Project {
	return New(schema.EDL{Cuts: []schema.Cut{
		{Clip: "a", Start: 0, End: 4},
		{Clip: "b", Start: 0, End: 2},
		{Clip: "c", Start: 1, End: 3},
	}})
}

func clips(p *Project) string {
	s := ""
	for _, c := range p.EDL.Cuts {
		s += c.Clip
	}
	return s
}

func TestSplitKeepsTotalLength(t *testing.T) {
	p := three()
	before := p.Duration()
	if err := p.Split(0, 1.5); err != nil {
		t.Fatal(err)
	}
	if len(p.EDL.Cuts) != 4 || p.EDL.Cuts[0].End != 1.5 || p.EDL.Cuts[1].Start != 1.5 {
		t.Fatalf("bad split: %+v", p.EDL.Cuts)
	}
	if p.Duration() != before {
		t.Errorf("duration %.2f -> %.2f, split must not change length", before, p.Duration())
	}
	if err := p.Split(0, 9); err == nil {
		t.Error("split outside the cut should fail")
	}
}

func TestMoveDeleteReorder(t *testing.T) {
	p := three()
	if err := p.Move(2, 0); err != nil || clips(p) != "cab" {
		t.Fatalf("move: %v %s", err, clips(p))
	}
	if err := p.Delete(1); err != nil || clips(p) != "cb" {
		t.Fatalf("delete: %v %s", err, clips(p))
	}
	if err := p.Delete(0); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(0); err == nil {
		t.Error("deleting the last cut should fail")
	}
}

func TestUndoRedo(t *testing.T) {
	p := three()
	_ = p.Delete(0)
	_ = p.Trim(0, 0.5, 1.5)
	if clips(p) != "bc" || p.EDL.Cuts[0].Start != 0.5 {
		t.Fatalf("setup wrong: %s %+v", clips(p), p.EDL.Cuts[0])
	}
	p.Undo()
	if p.EDL.Cuts[0].Start != 0 {
		t.Error("undo should restore trim")
	}
	p.Undo()
	if clips(p) != "abc" {
		t.Errorf("undo should restore delete, got %s", clips(p))
	}
	p.Redo()
	if clips(p) != "bc" {
		t.Errorf("redo, got %s", clips(p))
	}
	// A new edit clears redo.
	_ = p.Move(0, 1)
	if p.Redo() {
		t.Error("redo should be cleared after a new edit")
	}
}

func TestFailedEditLeavesProjectUntouched(t *testing.T) {
	p := three()
	if err := p.Trim(0, 3, 1); err == nil {
		t.Fatal("invalid trim should fail")
	}
	if p.EDL.Cuts[0].End != 4 || p.Undo() {
		t.Error("failed edit must not change state or history")
	}
}

func TestSnapshotsDoNotAlias(t *testing.T) {
	p := three()
	v := 0.5
	_ = p.Update(0, func(c *schema.Cut) { c.Volume = &v })
	_ = p.Update(0, func(c *schema.Cut) { *c.Volume = 0 })
	p.Undo()
	if got := *p.EDL.Cuts[0].Volume; got != 0.5 {
		t.Errorf("undo restored volume %.1f, want 0.5 (history aliased live state)", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := three()
	_ = p.AddText(schema.Text{Text: "hi", Start: 0, End: 1})
	path := filepath.Join(t.TempDir(), "proj.json")
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	q, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if clips(q) != "abc" || len(q.EDL.Texts) != 1 {
		t.Errorf("round trip lost data: %+v", q.EDL)
	}
}
