package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filmeditor/internal/store"
)

// visitorOf returns the caller's anonymous visitor id (a random 32-hex token
// the browser generates and keeps), or "" if absent or malformed. It is a
// preference key, not a credential: it unlocks nothing but saved settings and
// the list of that browser's own recent edits.
func visitorOf(r *http.Request) string {
	v := r.Header.Get("X-Visitor")
	if len(v) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return strings.ToLower(v)
}

// persist writes a job's current state to SQLite (no-op without a store).
func (s *Server) persist(j *job) {
	if s.store == nil {
		return
	}
	j.mu.Lock()
	row := store.Job{
		ID: j.id, Visitor: j.visitor, Created: j.created, Status: j.status, Stage: j.stage,
		Progress: j.progress, Error: j.errMsg, Planner: j.planner, Revision: j.rev, Parent: j.parent,
		Title: j.title, Preset: j.preset, Prompt: j.prompt, Dir: j.dir, Name: j.name,
	}
	row.Media, _ = json.Marshal(j.mediaIDs)
	row.Settings, _ = json.Marshal(j.settings)
	if j.edl != nil {
		row.EDL, _ = json.Marshal(j.edl)
	}
	row.Recipe, _ = json.Marshal(j.recipe)
	row.Notes, _ = json.Marshal(j.notes)
	j.mu.Unlock()
	_ = s.store.UpsertJob(row)
}

// saveRevision records a version of the edit so it can be undone later.
func (s *Server) saveRevision(j *job, instruction string) {
	if s.store == nil {
		return
	}
	j.mu.Lock()
	n, parent := j.rev, j.parent
	var raw []byte
	if j.edl != nil {
		raw, _ = json.Marshal(j.edl)
	}
	j.mu.Unlock()
	_ = s.store.AddRevision(j.id, store.Revision{N: n, Parent: parent, EDL: raw, Instruction: instruction})
}

// recover reloads jobs after a restart. Anything that was mid-flight is marked
// failed (its work is gone); jobs whose files have expired are forgotten.
func (s *Server) recoverJobs() {
	if s.store == nil {
		return
	}
	rows, err := s.store.Jobs()
	if err != nil {
		return
	}
	for _, r := range rows {
		if _, err := os.Stat(r.Dir); err != nil {
			_ = s.store.DeleteJob(r.ID)
			continue
		}
		j := &job{id: r.ID, dir: r.Dir, visitor: r.Visitor, created: r.Created, status: r.Status, stage: r.Stage,
			progress: r.Progress, errMsg: r.Error, planner: r.Planner, rev: r.Revision, parent: r.Parent,
			title: r.Title, preset: r.Preset, prompt: r.Prompt, name: r.Name}
		_ = json.Unmarshal(r.Media, &j.mediaIDs)
		_ = json.Unmarshal(r.Settings, &j.settings)
		if j.settings.Preset == "" {
			j.settings = defaultSettings()
		}
		if j.name == "" {
			j.name = projectNameFor(j.title)
		}
		j.keepReference = len(j.mediaIDs) > 0
		if len(r.EDL) > 0 {
			var e = new(edlT)
			if json.Unmarshal(r.EDL, e) == nil {
				j.edl = e
			}
		}
		_ = json.Unmarshal(r.Recipe, &j.recipe)
		_ = json.Unmarshal(r.Notes, &j.notes)
		if j.status == "queued" || j.status == "running" {
			j.status, j.stage, j.errMsg = "error", "Failed", "the editor restarted while this was running; please start again"
		}
		s.jobs[j.id] = j
		if r.Status != j.status {
			s.persist(j)
		}
	}
}

// ── API ──

type recentJob struct {
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	Status   string    `json:"status"`
	Title    string    `json:"title,omitempty"`
	Preset   string    `json:"preset,omitempty"`
	Prompt   string    `json:"prompt,omitempty"`
	Playable bool      `json:"playable"` // files still on the server
}

// me returns the visitor's saved preferences and recent edits.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	v := s.actor(r)
	if v == "" || s.store == nil {
		writeJSON(w, 200, map[string]any{"prefs": map[string]any{}, "recent": []recentJob{}})
		return
	}
	prefs, err := s.store.Prefs(v)
	if err != nil {
		fail(w, 500, "storage error")
		return
	}
	rows, _ := s.store.VisitorJobs(v, 8)
	recent := make([]recentJob, 0, len(rows))
	for _, r := range rows {
		_, err := os.Stat(filepath.Join(r.Dir, "out.mp4"))
		recent = append(recent, recentJob{ID: r.ID, Created: r.Created, Status: r.Status, Title: r.Title,
			Preset: r.Preset, Prompt: r.Prompt, Playable: err == nil && r.Status == "done"})
	}
	writeJSON(w, 200, map[string]any{"prefs": json.RawMessage(prefs), "recent": recent})
}

func (s *Server) setPrefs(w http.ResponseWriter, r *http.Request) {
	v := s.actor(r)
	if v == "" || s.store == nil {
		fail(w, http.StatusBadRequest, "no visitor id")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	var probe map[string]any
	if err != nil || json.Unmarshal(raw, &probe) != nil {
		fail(w, http.StatusBadRequest, "preferences must be a JSON object")
		return
	}
	// Whitelist: this must never become a place to stash keys or arbitrary data.
	clean := map[string]any{}
	for _, k := range []string{"preset", "duration", "briefMode", "prompt", "title", "provider"} {
		if val, ok := probe[k]; ok {
			if str, isStr := val.(string); isStr && len(str) > 1000 {
				continue
			}
			clean[k] = val
		}
	}
	out, _ := json.Marshal(clean)
	if err := s.store.SetPrefs(v, out); err != nil {
		fail(w, 500, "storage error")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// undo steps the edit back to the revision it was derived from and re-renders.
func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	if s.store == nil {
		fail(w, http.StatusNotImplemented, "history is not enabled on this server")
		return
	}
	var cur, parent int
	var busy bool
	j.set(func(j *job) {
		if j.status != "done" {
			busy = true
			return
		}
		cur, parent = j.rev, j.parent
		if parent > 0 {
			j.status, j.stage, j.progress, j.errMsg = "running", "Undoing", 0.1, ""
		}
	})
	if busy {
		fail(w, http.StatusConflict, "wait for the current edit to finish")
		return
	}
	if parent == 0 {
		fail(w, http.StatusBadRequest, "nothing to undo")
		return
	}
	target, err := s.store.Revision(j.id, parent)
	if err != nil {
		j.set(func(j *job) { j.status, j.stage = "done", "Done" })
		if errors.Is(err, store.ErrNotFound) {
			fail(w, http.StatusNotFound, "that earlier version is no longer available")
			return
		}
		fail(w, 500, "storage error")
		return
	}
	var prev edlT
	if err := json.Unmarshal(target.EDL, &prev); err != nil {
		j.set(func(j *job) { j.status, j.stage = "done", "Done" })
		fail(w, 500, "stored version is unreadable")
		return
	}
	task := func() {
		tmp := filepath.Join(j.dir, "out.undo.mp4")
		if err := renderTo(j, prev, tmp); err != nil {
			j.set(func(j *job) {
				j.status, j.stage, j.errMsg = "done", "Done", "could not undo: "+err.Error()
			})
			return
		}
		_ = os.Rename(tmp, filepath.Join(j.dir, "out.mp4"))
		j.set(func(j *job) {
			e := prev.Clone()
			j.edl, j.rev, j.parent = &e, cur+1, target.Parent
			j.status, j.stage, j.progress, j.errMsg = "done", "Done", 1, ""
		})
		s.saveRevision(j, "undo")
		s.persist(j)
	}
	select {
	case s.queue <- task:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": j.id})
	default:
		j.set(func(j *job) { j.status, j.stage = "done", "Done" })
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
	}
}
