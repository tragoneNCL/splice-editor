package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestJobsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "j1", Visitor: "v1", Created: time.Now(), Status: "done", Planner: "rule-based",
		Revision: 2, Parent: 1, Title: "Hi", Preset: "9:16",
		EDL: json.RawMessage(`{"cuts":[]}`), Recipe: json.RawMessage(`{}`), Notes: json.RawMessage(`["n"]`), Dir: "/x"}
	if err := s.UpsertJob(j); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRevision("j1", Revision{N: 1, EDL: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	jobs, err := s2.Jobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	got := jobs[0]
	if got.Status != "done" || got.Revision != 2 || got.Parent != 1 || string(got.Notes) != `["n"]` || got.Dir != "/x" {
		t.Errorf("round trip lost data: %+v", got)
	}
	r, err := s2.Revision("j1", 1)
	if err != nil || string(r.EDL) != `{"a":1}` {
		t.Errorf("revision: %+v %v", r, err)
	}
	if _, err := s2.Revision("j1", 9); err != ErrNotFound {
		t.Errorf("missing revision err = %v", err)
	}
}

func TestVisitorPrefsAndRecentJobs(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.Prefs("v1")
	if err != nil || string(p) != "{}" {
		t.Fatalf("fresh prefs = %s %v", p, err)
	}
	if err := s.SetPrefs("v1", json.RawMessage(`{"preset":"1:1"}`)); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Prefs("v1")
	if string(p) != `{"preset":"1:1"}` {
		t.Errorf("prefs = %s", p)
	}
	other, _ := s.Prefs("v2")
	if string(other) != "{}" {
		t.Errorf("prefs leaked across visitors: %s", other)
	}
	base := time.Now()
	for i, id := range []string{"a", "b", "c"} {
		_ = s.UpsertJob(Job{ID: id, Visitor: "v1", Created: base.Add(time.Duration(i) * time.Second), Status: "done"})
	}
	_ = s.UpsertJob(Job{ID: "z", Visitor: "v2", Created: base, Status: "done"})
	jobs, _ := s.VisitorJobs("v1", 2)
	if len(jobs) != 2 || jobs[0].ID != "c" || jobs[1].ID != "b" {
		t.Errorf("recent = %+v", jobs)
	}
	_ = s.DeleteJob("c")
	jobs, _ = s.VisitorJobs("v1", 5)
	if len(jobs) != 2 {
		t.Errorf("after delete: %d jobs", len(jobs))
	}
}
