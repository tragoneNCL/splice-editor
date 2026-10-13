package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"filmeditor/internal/synth"
)

func f(v float64) *float64 { return &v }

// readyProject makes a signed-in account with a generated two-clip project.
func readyProject(t *testing.T, a *acct) ProjectView {
	t.Helper()
	m1 := putMedia(t, a, "clip", "a.mp4", clipBytes(t, synth.RedCircle, 6))[0]
	m2 := putMedia(t, a, "clip", "b.mp4", clipBytes(t, synth.Landscape, 6))[0]
	_, raw := a.do("POST", "/api/projects", map[string]string{"name": "Edit me"}, nil)
	var p ProjectView
	json.Unmarshal(raw, &p)
	a.do("PATCH", "/api/projects/"+p.ID, map[string]any{
		"media":    []string{m1.ID, m2.ID},
		"settings": map[string]any{"preset": "1:1", "duration": 10},
	}, nil)
	if r, b := a.do("POST", "/api/projects/"+p.ID+"/generate", nil, nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("generate: %d %s", r.StatusCode, b)
	}
	return waitProject(t, a, p.ID, 1)
}

func edit(t *testing.T, a *acct, id string, ops ...editOp) (*http.Response, []byte) {
	t.Helper()
	return a.do("POST", "/api/projects/"+id+"/edit", map[string]any{"ops": ops}, nil)
}

func TestManualTimelineEdits(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("edit@y.co", "longenough1")
	p := readyProject(t, a)
	n := len(p.EDL.Cuts)
	if n < 2 {
		t.Fatalf("need at least two cuts to test with, got %d", n)
	}
	first := p.EDL.Cuts[0]

	// Trim + slow-mo + caption in one batch -> exactly one new revision.
	newEnd := first.Start + (first.End-first.Start)/2
	resp, raw := edit(t, a, p.ID,
		editOp{Op: "trim", I: 0, Start: f(first.Start), End: f(newEnd)},
		editOp{Op: "set", I: 0, Speed: f(0.5), Caption: ptr("hello")},
	)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("edit: %d %s", resp.StatusCode, raw)
	}
	v := waitProject(t, a, p.ID, 2)
	if v.Revision != 2 || !v.CanUndo {
		t.Fatalf("after edit: rev=%d undo=%v", v.Revision, v.CanUndo)
	}
	c0 := v.EDL.Cuts[0]
	if c0.End != newEnd || c0.Speed != 0.5 || c0.Caption != "hello" {
		t.Errorf("cut 0 = %+v", c0)
	}

	// Split + reorder.
	mid := c0.Start + (c0.End-c0.Start)/2
	edit(t, a, p.ID, editOp{Op: "split", I: 0, At: f(mid)}, editOp{Op: "move", From: 0, To: 1})
	v = waitProject(t, a, p.ID, 3)
	if len(v.EDL.Cuts) != n+1 {
		t.Errorf("split should add a cut: %d -> %d", n, len(v.EDL.Cuts))
	}

	// Delete + title + fades.
	edit(t, a, p.ID, editOp{Op: "delete", I: 0},
		editOp{Op: "add_text", Text: "Fin", Start: f(0.2), End: f(2), Position: "center"},
		editOp{Op: "fades", FadeIn: f(0.4), FadeOut: f(0.6)})
	v = waitProject(t, a, p.ID, 4)
	if len(v.EDL.Cuts) != n || len(v.EDL.Texts) != 1 || v.EDL.FadeIn != 0.4 || v.EDL.FadeOut != 0.6 {
		t.Errorf("after delete/title/fades: cuts=%d texts=%d fades=%v/%v", len(v.EDL.Cuts), len(v.EDL.Texts), v.EDL.FadeIn, v.EDL.FadeOut)
	}

	// Undo steps back one whole batch.
	a.do("POST", "/api/projects/"+p.ID+"/undo", nil, nil)
	v = waitProject(t, a, p.ID, 5)
	if len(v.EDL.Cuts) != n+1 || len(v.EDL.Texts) != 0 {
		t.Errorf("undo should restore the previous batch: cuts=%d texts=%d", len(v.EDL.Cuts), len(v.EDL.Texts))
	}
}

func ptr(s string) *string { return &s }

func TestBadEditsAreRejectedAndChangeNothing(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("bad@y.co", "longenough1")
	p := readyProject(t, a)
	cases := map[string][]editOp{
		"empty":            nil,
		"unknown op":       {{Op: "explode"}},
		"index out":        {{Op: "delete", I: 99}},
		"trim past source": {{Op: "trim", I: 0, Start: f(0), End: f(500)}},
		"bad speed":        {{Op: "set", I: 0, Speed: f(50)}},
		"split outside":    {{Op: "split", I: 0, At: f(9999)}},
		"trim no points":   {{Op: "trim", I: 0}},
		"empty title":      {{Op: "add_text", Text: "", Start: f(0), End: f(1)}},
		"one bad of many":  {{Op: "set", I: 0, Speed: f(0.5)}, {Op: "delete", I: 99}},
	}
	for name, ops := range cases {
		resp, raw := edit(t, a, p.ID, ops...)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d (%s), want 400", name, resp.StatusCode, raw)
		}
	}
	_, raw := a.do("GET", "/api/projects/"+p.ID, nil, nil)
	var v ProjectView
	json.Unmarshal(raw, &v)
	if v.Revision != 1 || v.Status != "done" || v.EDL.Cuts[0].Speed != 0 {
		t.Errorf("rejected edits must change nothing: rev=%d status=%s cut0=%+v", v.Revision, v.Status, v.EDL.Cuts[0])
	}
}

func TestEditsAreOwnerOnly(t *testing.T) {
	ts := authServer(t, nil)
	a, b := newAcct(t, ts.URL), newAcct(t, ts.URL)
	a.signup("own@y.co", "longenough1")
	b.signup("intruder@y.co", "longenough1")
	p := readyProject(t, a)
	resp, _ := edit(t, b, p.ID, editOp{Op: "delete", I: 0})
	if resp.StatusCode != 404 {
		t.Errorf("another account's edit = %d, want 404", resp.StatusCode)
	}
	if r, _ := a.do("POST", "/api/projects/"+p.ID+"/edit", "not json", nil); r.StatusCode != 400 {
		t.Errorf("malformed body = %d, want 400", r.StatusCode)
	}
	_ = strings.TrimSpace
}
