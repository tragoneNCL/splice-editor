package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filmeditor/internal/schema"
)

// A project is a named, persistent edit: the library footage it uses, the
// settings behind it, the current edit (EDL), and its version history. Its id
// is also its job id, so rendering, revising, undo and download all work on
// the same routes.

type projectSettings struct {
	Prompt     string  `json:"prompt"`
	Preset     string  `json:"preset"`
	Duration   float64 `json:"duration"`
	Title      string  `json:"title"`
	InspoURL   string  `json:"inspo_url,omitempty"`
	MusicMedia string  `json:"music_media,omitempty"`
	InspoMedia string  `json:"inspo_media,omitempty"`
}

func defaultSettings() projectSettings { return projectSettings{Preset: "9:16", Duration: 30} }

func (ps *projectSettings) validate() error {
	if _, ok := schema.Presets[ps.Preset]; !ok {
		return fmt.Errorf("unknown format %q", ps.Preset)
	}
	if ps.Duration < 5 || ps.Duration > 120 {
		return fmt.Errorf("length must be between 5 and 120 seconds")
	}
	if len(ps.Prompt) > 1000 {
		return fmt.Errorf("the brief is too long (max 1000 characters)")
	}
	if len(ps.Title) > 80 {
		return fmt.Errorf("the title is too long (max 80 characters)")
	}
	return nil
}

type projectSummary struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	Stage    string    `json:"stage,omitempty"`
	Progress float64   `json:"progress"`
	Created  time.Time `json:"created"`
	Preset   string    `json:"preset"`
	Cuts     int       `json:"cuts"`
	Length   float64   `json:"length"`
	Media    int       `json:"media"`
	Cover    string    `json:"cover,omitempty"` // media id whose thumbnail represents the project
	Revision int       `json:"revision"`
	Rendered bool      `json:"rendered"`
}

func summarize(j *job) projectSummary {
	j.mu.Lock()
	defer j.mu.Unlock()
	ps := projectSummary{ID: j.id, Name: j.name, Status: j.status, Stage: j.stage, Progress: j.progress,
		Created: j.created, Preset: j.settings.Preset, Media: len(j.mediaIDs), Revision: j.rev, Rendered: j.rev > 0}
	if len(j.mediaIDs) > 0 {
		ps.Cover = j.mediaIDs[0]
	}
	if j.edl != nil {
		ps.Cuts = len(j.edl.Cuts)
		for _, c := range j.edl.Cuts {
			ps.Length += c.Duration()
		}
	}
	return ps
}

func cleanName(n string) string {
	n = strings.TrimSpace(n)
	if len(n) > 80 {
		n = n[:80]
	}
	return n
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = readJSON(r, &body)
	name := cleanName(body.Name)
	if name == "" {
		name = "Untitled project"
	}
	id := newID()
	dir := filepath.Join(s.cfg.DataDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, "storage error")
		return
	}
	set := defaultSettings()
	rec := schema.DefaultRecipe()
	j := &job{id: id, dir: dir, created: time.Now(), status: "draft", stage: "Draft", recipe: rec,
		visitor: s.actor(r), name: name, settings: set, preset: set.Preset}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	s.persist(j)
	writeJSON(w, http.StatusCreated, s.projectView(j))
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	me := s.actor(r)
	var list []projectSummary
	s.mu.Lock()
	jobs := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		mine := j.visitor == me
		j.mu.Unlock()
		if mine {
			list = append(list, summarize(j))
		}
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Created.After(list[b].Created) })
	if list == nil {
		list = []projectSummary{}
	}
	writeJSON(w, 200, map[string]any{"projects": list})
}

// ProjectView is the full project document the workspace page loads.
type ProjectView struct {
	JobView
	Name     string          `json:"name"`
	Media    []mediaView     `json:"media"`
	Settings projectSettings `json:"settings"`
}

func (s *Server) projectView(j *job) ProjectView {
	v := j.view()
	j.mu.Lock()
	pv := ProjectView{JobView: v, Name: j.name, Settings: j.settings, Media: []mediaView{}}
	ids := append([]string(nil), j.mediaIDs...)
	owner := j.visitor
	j.mu.Unlock()
	if owner == "" {
		owner = "anon"
	}
	if s.store != nil {
		for _, id := range ids {
			if m, err := s.store.GetMedia(owner, id); err == nil {
				pv.Media = append(pv.Media, viewMedia(m))
			}
		}
	}
	return pv
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	if j := s.lookup(w, r); j != nil {
		writeJSON(w, 200, s.projectView(j))
	}
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	var body struct {
		Name     *string          `json:"name"`
		Media    *[]string        `json:"media"`
		Settings *projectSettings `json:"settings"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send the changes as JSON")
		return
	}
	if v := j.view(); v.Status == "running" || v.Status == "queued" {
		fail(w, http.StatusConflict, "wait for the current render to finish")
		return
	}
	owner := s.actor(r)
	if owner == "" {
		owner = "anon"
	}
	var newMedia []string
	if body.Media != nil {
		if len(*body.Media) > s.cfg.MaxClips {
			fail(w, http.StatusBadRequest, fmt.Sprintf("too many clips (max %d)", s.cfg.MaxClips))
			return
		}
		seen := map[string]bool{}
		for _, id := range *body.Media {
			if seen[id] {
				continue
			}
			seen[id] = true
			m, err := s.store.GetMedia(owner, id)
			if err != nil || m.Kind != "clip" {
				fail(w, http.StatusBadRequest, "one of those clips isn't in your library")
				return
			}
			newMedia = append(newMedia, id)
		}
	}
	if body.Settings != nil {
		if err := body.Settings.validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, ref := range []string{body.Settings.MusicMedia, body.Settings.InspoMedia} {
			if ref == "" {
				continue
			}
			if _, err := s.store.GetMedia(owner, ref); err != nil {
				fail(w, http.StatusBadRequest, "that music or reference file isn't in your library")
				return
			}
		}
		if body.Settings.InspoURL != "" {
			if _, err := validateYouTube(body.Settings.InspoURL); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	j.set(func(j *job) {
		if body.Name != nil {
			if n := cleanName(*body.Name); n != "" {
				j.name = n
			}
		}
		if body.Media != nil {
			j.mediaIDs = newMedia
		}
		if body.Settings != nil {
			j.settings = *body.Settings
			j.preset, j.title, j.prompt = j.settings.Preset, j.settings.Title, j.settings.Prompt
		}
	})
	s.persist(j)
	writeJSON(w, 200, s.projectView(j))
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	if v := j.view(); v.Status == "running" || v.Status == "queued" {
		fail(w, http.StatusConflict, "wait for the current render to finish")
		return
	}
	s.drop(j.id) // removes the project's own files and record; library footage stays
	writeJSON(w, 200, map[string]any{"ok": true})
}

// generate (re)builds the edit from the project's footage and settings.
func (s *Server) generateProject(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	ai, aiLabel, err := s.askerFor(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	owner := s.actor(r)
	if owner == "" {
		owner = "anon"
	}

	var p params
	var ids []string
	var set projectSettings
	var busy bool
	j.set(func(j *job) {
		if j.status == "running" || j.status == "queued" {
			busy = true
			return
		}
		ids = append(ids, j.mediaIDs...)
		set = j.settings
	})
	if busy {
		fail(w, http.StatusConflict, "this project is already rendering")
		return
	}
	if len(ids) == 0 {
		fail(w, http.StatusBadRequest, "add at least one clip to this project first")
		return
	}
	for _, id := range ids {
		m, err := s.store.GetMedia(owner, id)
		if err != nil {
			fail(w, http.StatusBadRequest, "some of this project's footage was removed from your library")
			return
		}
		if _, err := os.Stat(m.Path); err != nil {
			fail(w, http.StatusBadRequest, fmt.Sprintf("%q is missing from disk; add it again", m.Name))
			return
		}
		p.clips = append(p.clips, m.Path)
	}
	if set.MusicMedia != "" {
		if m, err := s.store.GetMedia(owner, set.MusicMedia); err == nil {
			p.music = m.Path
		}
	}
	if set.InspoMedia != "" {
		if m, err := s.store.GetMedia(owner, set.InspoMedia); err == nil {
			p.inspo = m.Path
		}
	}
	p.prompt, p.preset, p.duration, p.title, p.inspoURL = set.Prompt, set.Preset, set.Duration, set.Title, set.InspoURL

	rec := schema.DefaultRecipe()
	rec.Canvas, rec.TargetDuration = schema.Presets[set.Preset], set.Duration
	j.set(func(j *job) {
		j.recipe, j.clips = rec, p.clips
		j.status, j.stage, j.progress, j.errMsg = "queued", "Queued", 0, ""
		j.keepReference = true // library files are the user's: never delete them after a job
	})
	s.persist(j)

	select {
	case s.queue <- func() { s.runJob(j, p, ai, aiLabel) }:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": j.id})
	default:
		j.set(func(j *job) { j.status, j.stage = "draft", "Draft" })
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
	}
}

var _ = context.Background
var _ = json.Marshal
