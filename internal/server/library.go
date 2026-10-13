package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filmeditor/internal/ingest"
	"filmeditor/internal/store"
	"filmeditor/internal/understand"
)

// The media library keeps each person's footage after the upload that brought
// it in, so it can be reused across projects without being sent again.

type mediaView struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Duration float64   `json:"duration"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	HasAudio bool      `json:"has_audio"`
	Created  time.Time `json:"created"`
	Existing bool      `json:"existing,omitempty"` // already in the library; not stored twice
}

func viewMedia(m store.Media) mediaView {
	return mediaView{ID: m.ID, Kind: m.Kind, Name: m.Name, Size: m.Size, Duration: m.Duration,
		Width: m.Width, Height: m.Height, HasAudio: m.HasAudio, Created: m.Created}
}

// ownerDir is a filesystem-safe directory name for whoever owns the data.
func (s *Server) ownerDir(r *http.Request) string {
	a := s.actor(r)
	if a == "" {
		a = "anon"
	}
	return filepath.Join(s.cfg.DataDir, "library", a)
}

// fingerprint identifies a file cheaply: size plus the first and last MiB.
func fingerprint(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:", st.Size())
	const edge = 1 << 20
	if _, err := io.CopyN(h, f, edge); err != nil && err != io.EOF {
		return "", 0, err
	}
	if st.Size() > 2*edge {
		if _, err := f.Seek(-edge, io.SeekEnd); err != nil {
			return "", 0, err
		}
		if _, err := io.CopyN(h, f, edge); err != nil && err != io.EOF {
			return "", 0, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// commitUpload moves a finished chunked upload into the library.
func (s *Server) commitUpload(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		fail(w, http.StatusNotImplemented, "the library needs persistence, which is off")
		return
	}
	up := s.session(r.PathValue("id"))
	if up != nil && s.cfg.RequireAuth && up.owner != s.actor(r) {
		up = nil
	}
	if up == nil {
		fail(w, http.StatusNotFound, "no such upload (it may have expired)")
		return
	}
	owner := s.actor(r)
	if owner == "" {
		owner = "anon"
	}
	dir := s.ownerDir(r)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, "storage error")
		return
	}

	up.mu.Lock()
	type item struct {
		key string
		f   *uploadFile
	}
	var items []item
	for k, f := range up.files {
		if f.have() != f.total {
			up.mu.Unlock()
			fail(w, http.StatusBadRequest, fmt.Sprintf("%s did not finish uploading (%d of %d bytes)", f.name, f.have(), f.total))
			return
		}
		items = append(items, item{k, f})
	}
	up.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].f.path < items[j].f.path })

	var added []mediaView
	var problems []string
	for _, it := range items {
		kind := "clip"
		base := filepath.Base(it.f.path)
		switch {
		case strings.HasPrefix(base, "music"):
			kind = "music"
		case strings.HasPrefix(base, "inspo"):
			kind = "reference"
		}
		name := it.f.name
		if name == "" {
			name = base
		}
		hash, size, err := fingerprint(it.f.path)
		if err != nil {
			problems = append(problems, name+": could not read the file")
			continue
		}
		if dup, err := s.store.MediaByHash(owner, kind, hash); err == nil {
			v := viewMedia(dup)
			v.Existing = true
			added = append(added, v)
			continue
		}
		m := store.Media{ID: newID(), Owner: owner, Kind: kind, Name: name, Hash: hash, Size: size, Created: time.Now()}
		ext := strings.ToLower(filepath.Ext(base))
		m.Path = filepath.Join(dir, m.ID+ext)
		if kind != "music" { // audio has no picture to probe for size; probe works for both
		}
		info, err := ingest.Probe(it.f.path)
		if err != nil {
			problems = append(problems, name+": not a readable "+map[string]string{"clip": "video", "reference": "video", "music": "audio file"}[kind])
			continue
		}
		m.Duration, m.Width, m.Height, m.HasAudio = info.Duration, info.Width, info.Height, info.HasAudio
		if err := os.Rename(it.f.path, m.Path); err != nil {
			problems = append(problems, name+": storage error")
			continue
		}
		if kind != "music" {
			thumb := filepath.Join(dir, m.ID+".jpg")
			if err := understand.Frame(m.Path, info.Duration*0.25, 0, thumb); err == nil {
				m.Thumb = thumb
			}
		}
		if err := s.store.AddMedia(m); err != nil {
			os.Remove(m.Path)
			problems = append(problems, name+": storage error")
			continue
		}
		added = append(added, viewMedia(m))
	}

	// The session's leftovers (rejected files, empty dir) are no longer needed.
	s.mu.Lock()
	delete(s.uploads, r.PathValue("id"))
	s.mu.Unlock()
	os.RemoveAll(up.dir)

	code := http.StatusCreated
	if len(added) == 0 && len(problems) > 0 {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]any{"media": added, "problems": problems})
}

func (s *Server) listMedia(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, 200, map[string]any{"media": []mediaView{}})
		return
	}
	owner := s.actor(r)
	if owner == "" {
		owner = "anon"
	}
	rows, err := s.store.ListMedia(owner, r.URL.Query().Get("kind"))
	if err != nil {
		fail(w, 500, "storage error")
		return
	}
	out := make([]mediaView, 0, len(rows))
	for _, m := range rows {
		out = append(out, viewMedia(m))
	}
	bytes, _ := s.store.MediaBytes(owner)
	writeJSON(w, 200, map[string]any{"media": out, "bytes": bytes})
}

func (s *Server) mediaOf(r *http.Request) (store.Media, bool) {
	if s.store == nil {
		return store.Media{}, false
	}
	owner := s.actor(r)
	if owner == "" {
		owner = "anon"
	}
	m, err := s.store.GetMedia(owner, r.PathValue("id"))
	return m, err == nil
}

func (s *Server) mediaThumb(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaOf(r)
	if !ok || m.Thumb == "" {
		fail(w, http.StatusNotFound, "no such thumbnail")
		return
	}
	f, err := os.Open(m.Thumb)
	if err != nil {
		fail(w, http.StatusNotFound, "no such thumbnail")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "thumb.jpg", st.ModTime(), f)
}

func (s *Server) deleteMedia(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaOf(r)
	if !ok {
		fail(w, http.StatusNotFound, "no such media")
		return
	}
	// Refuse while a project still uses it, so no project silently breaks.
	var using []string
	s.mu.Lock()
	for _, j := range s.jobs {
		j.mu.Lock()
		if j.visitor == m.Owner || (m.Owner == "anon" && j.visitor == "") {
			for _, id := range j.mediaIDs {
				if id == m.ID {
					using = append(using, j.name)
					break
				}
			}
		}
		j.mu.Unlock()
	}
	s.mu.Unlock()
	if len(using) > 0 && r.URL.Query().Get("force") != "1" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "this footage is used by: " + strings.Join(using, ", "),
			"projects": using,
		})
		return
	}
	_ = s.store.DeleteMedia(m.Owner, m.ID)
	os.Remove(m.Path)
	if m.Thumb != "" {
		os.Remove(m.Thumb)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
