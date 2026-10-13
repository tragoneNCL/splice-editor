package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMediaLibraryIsPerOwner(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	a := Media{ID: "m1", Owner: "u1", Kind: "clip", Name: "beach.mp4", Path: "/x/m1.mp4", Hash: "h1", Size: 100, Duration: 12.5, Width: 1920, Height: 1080, HasAudio: true, Created: now}
	if err := s.AddMedia(a); err != nil {
		t.Fatal(err)
	}
	_ = s.AddMedia(Media{ID: "m2", Owner: "u1", Kind: "music", Name: "song.mp3", Path: "/x/m2.mp3", Hash: "h2", Size: 50, Created: now.Add(time.Second)})
	_ = s.AddMedia(Media{ID: "m3", Owner: "u2", Kind: "clip", Name: "other.mp4", Path: "/y/m3.mp4", Hash: "h1", Size: 70, Created: now})

	got, err := s.GetMedia("u1", "m1")
	if err != nil || got.Name != "beach.mp4" || !got.HasAudio || got.Duration != 12.5 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetMedia("u2", "m1"); err != ErrNotFound {
		t.Errorf("another owner must not read it, err = %v", err)
	}
	if clips, _ := s.ListMedia("u1", "clip"); len(clips) != 1 {
		t.Errorf("clips = %d", len(clips))
	}
	if all, _ := s.ListMedia("u1", ""); len(all) != 2 || all[0].ID != "m2" {
		t.Errorf("all = %+v (want newest first)", all)
	}
	if dup, err := s.MediaByHash("u1", "clip", "h1"); err != nil || dup.ID != "m1" {
		t.Errorf("dedupe within owner: %+v %v", dup, err)
	}
	if _, err := s.MediaByHash("u2", "clip", "h2"); err != ErrNotFound {
		t.Errorf("dedupe must not cross owners: %v", err)
	}
	if n, _ := s.MediaBytes("u1"); n != 150 {
		t.Errorf("bytes = %d", n)
	}
	_ = s.DeleteMedia("u2", "m1") // wrong owner: no effect
	if _, err := s.GetMedia("u1", "m1"); err != nil {
		t.Error("another owner deleted my media")
	}
	_ = s.DeleteMedia("u1", "m1")
	if _, err := s.GetMedia("u1", "m1"); err != ErrNotFound {
		t.Errorf("after delete err = %v", err)
	}
}

func TestProjectColumnsRoundTrip(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	j := Job{ID: "p1", Visitor: "u1", Created: time.Now(), Status: "draft", Name: "Beach trip",
		Media: json.RawMessage(`["m1","m2"]`), Settings: json.RawMessage(`{"prompt":"calm"}`), EDL: json.RawMessage(`{}`), Recipe: json.RawMessage(`{}`), Notes: json.RawMessage(`[]`)}
	if err := s.UpsertJob(j); err != nil {
		t.Fatal(err)
	}
	j.Name = "Beach trip 2"
	j.Title, j.Preset = "Hi", "1:1"
	if err := s.UpsertJob(j); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.VisitorJobs("u1", 5)
	if len(jobs) != 1 || jobs[0].Name != "Beach trip 2" || string(jobs[0].Media) != `["m1","m2"]` ||
		jobs[0].Title != "Hi" || jobs[0].Preset != "1:1" {
		t.Errorf("round trip: %+v", jobs)
	}
}
