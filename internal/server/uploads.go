package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Large footage is uploaded in chunks: public tunnels and proxies cap a single
// request at around 100 MB, and a 20 GB upload should survive a dropped
// connection. Chunks carry an explicit byte offset and may arrive in any order
// and several at a time, which is what makes uploads fast on high-latency
// links. A retried chunk simply overwrites the same range.

const (
	maxChunkBytes = 96 << 20
	chunkSize     = 32 << 20 // what the client is told to send
)

type uploadSession struct {
	owner   string
	mu      sync.Mutex
	dir     string
	created time.Time
	bytes   int64
	files   map[string]*uploadFile // kind+index -> file
}

type uploadFile struct {
	path     string
	total    int64           // declared full size
	received map[int64]int64 // offset -> length of each stored chunk
}

func (f *uploadFile) have() int64 {
	var n int64
	for _, l := range f.received {
		n += l
	}
	return n
}

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many uploads from this address; try again in a while")
		return
	}
	if err := s.checkDisk(0); err != nil {
		fail(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	id := newID()
	dir := filepath.Join(s.cfg.DataDir, "up-"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, "storage error")
		return
	}
	s.mu.Lock()
	s.uploads[id] = &uploadSession{owner: s.actor(r), dir: dir, created: time.Now(), files: map[string]*uploadFile{}}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"upload_id": id, "max_bytes": s.cfg.MaxUploadBytes, "chunk_bytes": chunkSize,
	})
}

func (s *Server) session(id string) *uploadSession {
	if len(id) != 32 {
		return nil
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploads[id]
}

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	up := s.session(r.PathValue("id"))
	if up != nil && s.cfg.RequireAuth && up.owner != s.actor(r) {
		up = nil
	}
	if up == nil {
		fail(w, http.StatusNotFound, "no such upload (it may have expired)")
		return
	}
	kind := r.Header.Get("X-Kind")
	idx, _ := strconv.Atoi(r.Header.Get("X-Index"))
	offset, err := strconv.ParseInt(r.Header.Get("X-Offset"), 10, 64)
	name := r.Header.Get("X-Name")
	if err != nil || offset < 0 || idx < 0 {
		fail(w, http.StatusBadRequest, "bad chunk headers")
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	var fname string
	switch kind {
	case "clip":
		if !allowedExt[ext] {
			fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a supported video type (use mp4, mov, m4v, mkv or webm)", filepath.Base(name)))
			return
		}
		if idx >= s.cfg.MaxClips {
			fail(w, http.StatusBadRequest, fmt.Sprintf("too many clips (max %d)", s.cfg.MaxClips))
			return
		}
		fname = fmt.Sprintf("clip%03d%s", idx, ext)
	case "inspo":
		if !allowedExt[ext] {
			fail(w, http.StatusBadRequest, "the reference must be a video file")
			return
		}
		fname = "inspo" + ext
	case "music":
		if !allowedAudio[ext] {
			fail(w, http.StatusBadRequest, "music must be mp3, m4a, aac, wav or ogg")
			return
		}
		fname = "music" + ext
	default:
		fail(w, http.StatusBadRequest, "unknown kind")
		return
	}

	total, _ := strconv.ParseInt(r.Header.Get("X-Total"), 10, 64)
	if total <= 0 || offset >= total {
		fail(w, http.StatusBadRequest, "bad chunk headers")
		return
	}
	if total > s.cfg.MaxUploadBytes {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("that file is more than the %d MB limit for one edit", s.cfg.MaxUploadBytes>>20))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxChunkBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, http.StatusBadRequest, "upload interrupted; retry the chunk")
		return
	}
	if offset+int64(len(data)) > total {
		fail(w, http.StatusBadRequest, "chunk runs past the declared file size")
		return
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	key := kind + strconv.Itoa(idx)
	f := up.files[key]
	if f == nil {
		f = &uploadFile{path: filepath.Join(up.dir, fname), total: total, received: map[int64]int64{}}
		up.files[key] = f
	}
	if f.total != total {
		fail(w, http.StatusBadRequest, "file size changed mid-upload")
		return
	}
	newBytes := int64(len(data)) - f.received[offset]
	if up.bytes+newBytes > s.cfg.MaxUploadBytes {
		fail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("that's more than the %d MB limit for one edit", s.cfg.MaxUploadBytes>>20))
		return
	}
	if err := s.checkDisk(newBytes); err != nil {
		fail(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	out, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fail(w, 500, "storage error")
		return
	}
	_, werr := out.WriteAt(data, offset)
	out.Close()
	if werr != nil {
		fail(w, 500, "storage error")
		return
	}
	f.received[offset] = int64(len(data))
	up.bytes += newBytes
	writeJSON(w, 200, map[string]any{"have": f.have(), "total": f.total, "complete": f.have() == f.total})
}

// checkDisk refuses work that would leave the host with too little free space.
func (s *Server) checkDisk(incoming int64) error {
	free, ok := freeBytes(s.cfg.DataDir)
	if !ok {
		return nil // can't tell; don't block
	}
	if free-incoming < s.cfg.MinFreeBytes {
		return errors.New("the editor's disk is nearly full right now; try a smaller upload or try again later")
	}
	return nil
}

// jobRequest is the JSON body for creating a job from a finished upload.
type jobRequest struct {
	UploadID string `json:"upload_id"`
	Prompt   string `json:"prompt"`
	Title    string `json:"title"`
	Preset   string `json:"preset"`
	Duration string `json:"duration"`
	InspoURL string `json:"inspo_url"`
}

// createFromUpload turns a finished chunked upload into a job.
func (s *Server) createFromUpload(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "send the upload id and options as JSON")
		return
	}
	ai, aiLabel, err := s.askerFor(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	up := s.session(req.UploadID)
	if up != nil && s.cfg.RequireAuth && up.owner != s.actor(r) {
		up = nil
	}
	if up == nil {
		fail(w, http.StatusNotFound, "no such upload (it may have expired)")
		return
	}
	p := params{preset: "9:16", duration: 30}
	for k, v := range map[string]string{"prompt": req.Prompt, "title": req.Title, "preset": req.Preset,
		"duration": req.Duration, "inspo_url": req.InspoURL} {
		if v == "" {
			continue
		}
		if err := p.setField(k, strings.TrimSpace(v)); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	up.mu.Lock()
	type named struct {
		name string
		f    *uploadFile
	}
	var clips []named
	for k, f := range up.files {
		if f.have() != f.total {
			up.mu.Unlock()
			fail(w, http.StatusBadRequest, fmt.Sprintf("%s did not finish uploading (%d of %d bytes)",
				filepath.Base(f.path), f.have(), f.total))
			return
		}
		base := filepath.Base(f.path)
		switch {
		case strings.HasPrefix(base, "clip"):
			clips = append(clips, named{base, f})
		case strings.HasPrefix(base, "music"):
			p.music = f.path
		case strings.HasPrefix(base, "inspo"):
			p.inspo = f.path
		}
		_ = k
	}
	up.mu.Unlock()
	sort.Slice(clips, func(i, j int) bool { return clips[i].name < clips[j].name })
	if len(clips) == 0 {
		fail(w, http.StatusBadRequest, "add at least one video clip")
		return
	}

	// Adopt the upload directory as the job directory.
	id := newID()
	dir := filepath.Join(s.cfg.DataDir, id)
	if err := os.Rename(up.dir, dir); err != nil {
		fail(w, 500, "storage error")
		return
	}
	s.mu.Lock()
	delete(s.uploads, req.UploadID)
	s.mu.Unlock()
	rebase := func(p string) string {
		if p == "" {
			return ""
		}
		return filepath.Join(dir, filepath.Base(p))
	}
	p.music, p.inspo = rebase(p.music), rebase(p.inspo)
	for _, c := range clips {
		p.clips = append(p.clips, filepath.Join(dir, c.name))
	}
	s.launch(w, r, id, dir, p, ai, aiLabel)
}
