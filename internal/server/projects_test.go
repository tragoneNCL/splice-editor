package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"filmeditor/internal/synth"
)

// putMedia uploads one file through the chunked API and commits it to the
// signed-in account's library, returning the committed media list.
func putMedia(t *testing.T, a *acct, kindHdr, name string, data []byte) []mediaView {
	t.Helper()
	_, raw := a.do("POST", "/api/uploads", nil, nil)
	var sess struct {
		UploadID string `json:"upload_id"`
	}
	json.Unmarshal(raw, &sess)
	if sess.UploadID == "" {
		t.Fatalf("no upload session: %s", raw)
	}
	half := len(data) / 2
	for _, part := range [][2]int{{half, len(data)}, {0, half}} { // out of order on purpose
		resp, b := a.do("PUT", "/api/uploads/"+sess.UploadID+"/chunk", bytes.NewBuffer(data[part[0]:part[1]]), map[string]string{
			"X-Kind": kindHdr, "X-Index": "0", "X-Name": name,
			"X-Offset": fmt.Sprint(part[0]), "X-Total": fmt.Sprint(len(data)),
		})
		if resp.StatusCode != 200 {
			t.Fatalf("chunk: %d %s", resp.StatusCode, b)
		}
	}
	resp, b := a.do("POST", "/api/uploads/"+sess.UploadID+"/commit", nil, nil)
	var out struct {
		Media    []mediaView `json:"media"`
		Problems []string    `json:"problems"`
	}
	json.Unmarshal(b, &out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit: %d %s", resp.StatusCode, b)
	}
	return out.Media
}

func waitProject(t *testing.T, a *acct, id string, rev int) ProjectView {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		_, b := a.do("GET", "/api/projects/"+id, nil, nil)
		var v ProjectView
		json.Unmarshal(b, &v)
		if v.Status == "error" {
			t.Fatalf("project failed: %s", v.Error)
		}
		if v.Status == "done" && v.Revision >= rev {
			return v
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("project %s never reached revision %d", id, rev)
	return ProjectView{}
}

func TestLibraryKeepsFootageAndDedupes(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("lib@y.co", "longenough1")

	clip := clipBytes(t, synth.Landscape, 3)
	first := putMedia(t, a, "clip", "beach.mp4", clip)
	if len(first) != 1 || first[0].Name != "beach.mp4" || first[0].Duration < 2 || first[0].Existing {
		t.Fatalf("first commit: %+v", first)
	}
	again := putMedia(t, a, "clip", "beach-copy.mp4", clip)
	if len(again) != 1 || again[0].ID != first[0].ID || !again[0].Existing {
		t.Errorf("same footage should not be stored twice: %+v", again)
	}
	_, raw := a.do("GET", "/api/media?kind=clip", nil, nil)
	var list struct{ Media []mediaView }
	json.Unmarshal(raw, &list)
	if len(list.Media) != 1 {
		t.Errorf("library has %d items, want 1", len(list.Media))
	}

	resp, _ := a.do("GET", "/api/media/"+first[0].ID+"/thumb", nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("thumbnail: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	other := newAcct(t, ts.URL)
	other.signup("other@y.co", "longenough1")
	if r, _ := other.do("GET", "/api/media/"+first[0].ID+"/thumb", nil, nil); r.StatusCode != 404 {
		t.Errorf("another account's thumbnail = %d, want 404", r.StatusCode)
	}
	_, raw = other.do("GET", "/api/media", nil, nil)
	json.Unmarshal(raw, &list)
	if len(list.Media) != 0 {
		t.Errorf("library leaked across accounts: %+v", list.Media)
	}
	// The same bytes in a different account are that account's own copy.
	if m := putMedia(t, other, "clip", "beach.mp4", clip); len(m) != 1 || m[0].Existing || m[0].ID == first[0].ID {
		t.Errorf("per-account storage: %+v", m)
	}
}

func TestRejectsNonVideoAtCommit(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("bad@y.co", "longenough1")
	_, raw := a.do("POST", "/api/uploads", nil, nil)
	var sess struct {
		UploadID string `json:"upload_id"`
	}
	json.Unmarshal(raw, &sess)
	data := []byte("this is plainly not a video file at all")
	a.do("PUT", "/api/uploads/"+sess.UploadID+"/chunk", bytes.NewBuffer(data), map[string]string{
		"X-Kind": "clip", "X-Index": "0", "X-Name": "fake.mp4", "X-Offset": "0", "X-Total": fmt.Sprint(len(data))})
	resp, b := a.do("POST", "/api/uploads/"+sess.UploadID+"/commit", nil, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "not a readable video") {
		t.Errorf("commit: %d %s", resp.StatusCode, b)
	}
}

func TestProjectLifecycle(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("proj@y.co", "longenough1")
	m1 := putMedia(t, a, "clip", "a.mp4", clipBytes(t, synth.RedCircle, 3))[0]
	m2 := putMedia(t, a, "clip", "b.mp4", clipBytes(t, synth.Landscape, 3))[0]

	// create
	resp, raw := a.do("POST", "/api/projects", map[string]string{"name": "  Beach trip "}, nil)
	var p ProjectView
	json.Unmarshal(raw, &p)
	if resp.StatusCode != 201 || p.Name != "Beach trip" || p.Status != "draft" || p.Settings.Preset != "9:16" {
		t.Fatalf("create: %d %+v", resp.StatusCode, p)
	}

	// generating with no footage is a clear error, not a crash
	if r, b := a.do("POST", "/api/projects/"+p.ID+"/generate", nil, nil); r.StatusCode != 400 || !strings.Contains(string(b), "at least one clip") {
		t.Errorf("empty generate: %d %s", r.StatusCode, b)
	}

	// validation
	for name, body := range map[string]any{
		"unknown format": map[string]any{"settings": map[string]any{"preset": "7:3", "duration": 30}},
		"bad length":     map[string]any{"settings": map[string]any{"preset": "1:1", "duration": 9999}},
		"foreign media":  map[string]any{"media": []string{"someone-elses-id"}},
	} {
		if r, _ := a.do("PATCH", "/api/projects/"+p.ID, body, nil); r.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, r.StatusCode)
		}
	}

	// configure
	r, raw := a.do("PATCH", "/api/projects/"+p.ID, map[string]any{
		"name":     "Beach trip 2026",
		"media":    []string{m1.ID, m2.ID},
		"settings": map[string]any{"preset": "1:1", "duration": 8, "prompt": "calm", "title": "Summer"},
	}, nil)
	json.Unmarshal(raw, &p)
	if r.StatusCode != 200 || p.Name != "Beach trip 2026" || len(p.Media) != 2 || p.Settings.Preset != "1:1" {
		t.Fatalf("patch: %d %+v", r.StatusCode, p)
	}

	// generate, then regenerate: revisions advance and undo steps back
	if r, b := a.do("POST", "/api/projects/"+p.ID+"/generate", nil, nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("generate: %d %s", r.StatusCode, b)
	}
	v1 := waitProject(t, a, p.ID, 1)
	if v1.Revision != 1 || v1.EDL == nil || len(v1.EDL.Cuts) == 0 || v1.CanUndo {
		t.Fatalf("first generation: %+v", v1.JobView)
	}
	if r, _ := a.do("GET", "/api/projects/"+p.ID+"/video", nil, nil); r.StatusCode != 200 {
		t.Errorf("video: %d", r.StatusCode)
	}
	a.do("POST", "/api/projects/"+p.ID+"/generate", nil, nil)
	v2 := waitProject(t, a, p.ID, 2)
	if v2.Revision != 2 || !v2.CanUndo {
		t.Fatalf("regeneration: rev=%d undo=%v", v2.Revision, v2.CanUndo)
	}
	a.do("POST", "/api/projects/"+p.ID+"/undo", nil, nil)
	v3 := waitProject(t, a, p.ID, 3)
	if v3.CanUndo {
		t.Errorf("after undoing the regeneration there is nothing earlier: %+v", v3.JobView)
	}

	// list
	_, raw = a.do("GET", "/api/projects", nil, nil)
	var list struct{ Projects []projectSummary }
	json.Unmarshal(raw, &list)
	if len(list.Projects) != 1 || list.Projects[0].Name != "Beach trip 2026" || list.Projects[0].Media != 2 || !list.Projects[0].Rendered {
		t.Errorf("list: %+v", list.Projects)
	}

	// library footage in use can't be deleted by accident
	r, b := a.do("DELETE", "/api/media/"+m1.ID, nil, nil)
	if r.StatusCode != http.StatusConflict || !strings.Contains(string(b), "Beach trip 2026") {
		t.Errorf("delete in-use media: %d %s", r.StatusCode, b)
	}
	// deleting the project leaves the footage; then the footage can go
	if r, _ := a.do("DELETE", "/api/projects/"+p.ID, nil, nil); r.StatusCode != 200 {
		t.Fatalf("delete project: %d", r.StatusCode)
	}
	_, raw = a.do("GET", "/api/media", nil, nil)
	var lib struct{ Media []mediaView }
	json.Unmarshal(raw, &lib)
	if len(lib.Media) != 2 {
		t.Errorf("footage must survive project deletion: %d items", len(lib.Media))
	}
	if r, _ := a.do("DELETE", "/api/media/"+m1.ID, nil, nil); r.StatusCode != 200 {
		t.Errorf("delete unused media: %d", r.StatusCode)
	}
	if r, _ := a.do("GET", "/api/projects/"+p.ID, nil, nil); r.StatusCode != 404 {
		t.Errorf("deleted project still reachable: %d", r.StatusCode)
	}
}

func TestProjectsAreIsolatedBetweenAccounts(t *testing.T) {
	ts := authServer(t, nil)
	alice, bob := newAcct(t, ts.URL), newAcct(t, ts.URL)
	alice.signup("a@y.co", "longenough1")
	bob.signup("b@y.co", "longenough1")
	m := putMedia(t, alice, "clip", "a.mp4", clipBytes(t, synth.RedCircle, 2))[0]
	_, raw := alice.do("POST", "/api/projects", map[string]string{"name": "Private"}, nil)
	var p ProjectView
	json.Unmarshal(raw, &p)

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/projects/" + p.ID}, {"PATCH", "/api/projects/" + p.ID},
		{"DELETE", "/api/projects/" + p.ID}, {"POST", "/api/projects/" + p.ID + "/generate"},
		{"POST", "/api/projects/" + p.ID + "/undo"},
	} {
		if r, _ := bob.do(c.method, c.path, map[string]any{"name": "pwned"}, nil); r.StatusCode != 404 {
			t.Errorf("bob %s %s = %d, want 404", c.method, c.path, r.StatusCode)
		}
	}
	// Bob can't attach Alice's footage to his own project.
	_, raw = bob.do("POST", "/api/projects", map[string]string{"name": "Mine"}, nil)
	var bp ProjectView
	json.Unmarshal(raw, &bp)
	if r, _ := bob.do("PATCH", "/api/projects/"+bp.ID, map[string]any{"media": []string{m.ID}}, nil); r.StatusCode != 400 {
		t.Errorf("attaching another account's footage = %d, want 400", r.StatusCode)
	}
	_, raw = bob.do("GET", "/api/projects", nil, nil)
	var list struct{ Projects []projectSummary }
	json.Unmarshal(raw, &list)
	if len(list.Projects) != 1 || list.Projects[0].Name != "Mine" {
		t.Errorf("bob's list: %+v", list.Projects)
	}
}
