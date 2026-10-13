// Package server exposes the editor over HTTP for a static front end
// (e.g. GitHub Pages) to call. Uploads stream to disk, jobs run in a small
// worker pool, and everything is bounded: sizes, clip counts, queue length,
// per-IP rate, and how long results are kept.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"filmeditor/internal/ingest"
	"filmeditor/internal/llm"
	"filmeditor/internal/orient"
	"filmeditor/internal/pipeline"
	"filmeditor/internal/plan"
	"filmeditor/internal/project"
	"filmeditor/internal/revise"
	"filmeditor/internal/schema"
)

type Config struct {
	DataDir        string
	MaxUploadBytes int64         // whole request
	MaxClips       int           // per job
	MaxFootageSecs float64       // total source duration per job
	Workers        int           // concurrent renders
	QueueSize      int           // waiting jobs before 503
	TTL            time.Duration // how long a job's files are kept
	AllowedOrigin  string        // CORS; "*" for any, or the Pages origin
	RatePerHour    int           // new jobs per IP per hour
	JobTimeout     time.Duration

	// AI is optional. Without it the rule-based planner is used and
	// plain-language revisions are unavailable.
	AI      llm.Asker
	AILabel string

	// UI, if set, is served at / so one container hosts site and API.
	UI fs.FS
}

func (c *Config) defaults() {
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = 400 << 20
	}
	if c.MaxClips == 0 {
		c.MaxClips = 12
	}
	if c.MaxFootageSecs == 0 {
		c.MaxFootageSecs = 15 * 60
	}
	if c.Workers == 0 {
		c.Workers = 1
	}
	if c.QueueSize == 0 {
		c.QueueSize = 8
	}
	if c.TTL == 0 {
		c.TTL = time.Hour
	}
	if c.AllowedOrigin == "" {
		c.AllowedOrigin = "*"
	}
	if c.RatePerHour == 0 {
		c.RatePerHour = 6
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 20 * time.Minute
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(os.TempDir(), "filmeditor-data")
	}
}

type Server struct {
	cfg   Config
	queue chan func()

	mu   sync.Mutex
	jobs map[string]*job
	hits map[string][]time.Time
}

type job struct {
	mu       sync.Mutex
	id       string
	dir      string
	created  time.Time
	status   string // queued | running | done | error
	stage    string
	progress float64
	errMsg   string
	planner  string
	edl      *schema.EDL
	recipe   schema.Recipe
	clips    []string
	rev      int // bumps on each re-render so clients can bust caches
}

// JobView is the JSON a client sees.
type JobView struct {
	ID       string      `json:"id"`
	Status   string      `json:"status"`
	Stage    string      `json:"stage"`
	Progress float64     `json:"progress"`
	Error    string      `json:"error,omitempty"`
	Planner  string      `json:"planner,omitempty"`
	Revision int         `json:"revision"`
	EDL      *schema.EDL `json:"edl,omitempty"`
}

func (j *job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobView{ID: j.id, Status: j.status, Stage: j.stage, Progress: j.progress,
		Error: j.errMsg, Planner: j.planner, Revision: j.rev, EDL: j.edl}
}

func (j *job) set(fn func(j *job)) {
	j.mu.Lock()
	fn(j)
	j.mu.Unlock()
}

// New starts the worker pool and the cleanup loop; cancel ctx to stop them.
func New(ctx context.Context, cfg Config) (*Server, error) {
	cfg.defaults()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, queue: make(chan func(), cfg.QueueSize),
		jobs: map[string]*job{}, hits: map[string][]time.Time{}}
	for i := 0; i < cfg.Workers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case fn := <-s.queue:
					fn()
				}
			}
		}()
	}
	go s.cleanupLoop(ctx)
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/verify", s.verify)
	mux.HandleFunc("POST /api/jobs", s.create)
	mux.HandleFunc("GET /api/jobs/{id}", s.status)
	mux.HandleFunc("GET /api/jobs/{id}/video", s.video)
	mux.HandleFunc("POST /api/jobs/{id}/revise", s.reviseJob)
	if s.cfg.UI != nil {
		mux.Handle("GET /", http.FileServerFS(s.cfg.UI))
	}
	return s.cors(mux)
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", s.cfg.AllowedOrigin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-AI-Provider, X-AI-Key")
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	ai := "none"
	if s.cfg.AI != nil {
		ai = s.cfg.AILabel
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ai": ai, "server_ai": s.cfg.AI != nil, "max_clips": s.cfg.MaxClips,
		"max_upload_mb": s.cfg.MaxUploadBytes >> 20})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allow(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	kept := s.hits[ip][:0]
	for _, t := range s.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= s.cfg.RatePerHour {
		s.hits[ip] = kept
		return false
	}
	s.hits[ip] = append(kept, time.Now())
	return true
}

// askerFor resolves which AI backend serves this request: the visitor's own
// key (sent per request, never stored or logged) wins over the server's.
func (s *Server) askerFor(r *http.Request) (llm.Asker, string, error) {
	key := strings.TrimSpace(r.Header.Get("X-AI-Key"))
	if key == "" {
		if s.cfg.AI != nil {
			return s.cfg.AI, s.cfg.AILabel, nil
		}
		return nil, "", nil
	}
	if len(key) < 8 || len(key) > 300 || strings.ContainsAny(key, " \t\r\n") {
		return nil, "", fmt.Errorf("that doesn't look like an API key")
	}
	switch strings.ToLower(r.Header.Get("X-AI-Provider")) {
	case "claude", "anthropic":
		return llm.Anthropic{Key: key}, "claude", nil
	case "gemini":
		return llm.Gemini{Key: key, Model: os.Getenv("GEMINI_MODEL")}, "gemini", nil
	}
	return nil, "", fmt.Errorf("unknown AI provider (use claude or gemini)")
}

// verify makes one tiny call so the UI can confirm a pasted key works.
func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	if !s.allow("verify:" + clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many attempts; wait a bit and try again")
		return
	}
	ai, label, err := s.askerFor(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ai == nil {
		fail(w, http.StatusBadRequest, "no key provided")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := ai.Ask(ctx, `Reply with exactly: {"ok":true}`, nil); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "provider": label})
}

var allowedExt = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true}
var allowedAudio = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".ogg": true}

type params struct {
	prompt   string
	preset   string
	duration float64
	title    string
	music    string
	clips    []string
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many videos from this address; try again in a while")
		return
	}
	if len(s.queue) >= cap(s.queue) {
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
		return
	}
	ai, aiLabel, err := s.askerFor(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	id := newID()
	dir := filepath.Join(s.cfg.DataDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, "storage error")
		return
	}
	p, err := s.readUpload(r, dir)
	if err != nil {
		os.RemoveAll(dir)
		code := http.StatusBadRequest
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			code = http.StatusRequestEntityTooLarge
		}
		fail(w, code, err.Error())
		return
	}

	canvas := schema.Presets[p.preset]
	rec := schema.DefaultRecipe()
	rec.Canvas, rec.TargetDuration = canvas, p.duration
	j := &job{id: id, dir: dir, created: time.Now(), status: "queued", stage: "Queued", recipe: rec, clips: p.clips}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	select {
	case s.queue <- func() { s.runJob(j, p, ai, aiLabel) }:
	default:
		s.drop(id)
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

// readUpload streams multipart parts to disk with sanitised, generated names.
func (s *Server) readUpload(r *http.Request, dir string) (params, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return params{}, fmt.Errorf("expected a multipart upload")
	}
	p := params{preset: "9:16", duration: 30}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return params{}, err
		}
		name := part.FormName()
		if part.FileName() == "" {
			val, _ := io.ReadAll(io.LimitReader(part, 4096))
			if err := p.setField(name, strings.TrimSpace(string(val))); err != nil {
				return params{}, err
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(part.FileName()))
		switch name {
		case "clips":
			if !allowedExt[ext] {
				return params{}, fmt.Errorf("%q is not a supported video type (use mp4, mov, m4v, mkv or webm)", filepath.Base(part.FileName()))
			}
			if len(p.clips) >= s.cfg.MaxClips {
				return params{}, fmt.Errorf("too many clips (max %d)", s.cfg.MaxClips)
			}
			dst, err := savePart(part, filepath.Join(dir, fmt.Sprintf("clip%02d%s", len(p.clips), ext)))
			if err != nil {
				return params{}, err
			}
			p.clips = append(p.clips, dst)
		case "music":
			if !allowedAudio[ext] {
				return params{}, fmt.Errorf("music must be mp3, m4a, aac, wav or ogg")
			}
			dst, err := savePart(part, filepath.Join(dir, "music"+ext))
			if err != nil {
				return params{}, err
			}
			p.music = dst
		default:
			io.Copy(io.Discard, part)
		}
	}
	if len(p.clips) == 0 {
		return params{}, fmt.Errorf("add at least one video clip")
	}
	return p, nil
}

func (p *params) setField(name, val string) error {
	switch name {
	case "prompt":
		if len(val) > 1000 {
			return fmt.Errorf("prompt is too long (max 1000 characters)")
		}
		p.prompt = val
	case "title":
		if len(val) > 80 {
			return fmt.Errorf("title is too long (max 80 characters)")
		}
		p.title = val
	case "preset":
		if _, ok := schema.Presets[val]; !ok {
			return fmt.Errorf("unknown preset %q", val)
		}
		p.preset = val
	case "duration":
		d, err := strconv.ParseFloat(val, 64)
		if err != nil || d < 5 || d > 120 {
			return fmt.Errorf("duration must be between 5 and 120 seconds")
		}
		p.duration = d
	}
	return nil
}

func savePart(src io.Reader, path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) runJob(j *job, p params, ai llm.Asker, aiLabel string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.JobTimeout)
	defer cancel()
	j.set(func(j *job) { j.status, j.stage = "running", "Starting" })

	// Reject files that are not video and cap total footage.
	var total float64
	for _, c := range p.clips {
		info, err := ingest.Probe(c)
		if err != nil {
			s.failJob(j, "one of the files is not a readable video")
			return
		}
		total += info.Duration
	}
	if total > s.cfg.MaxFootageSecs {
		s.failJob(j, fmt.Sprintf("too much footage (%.0f min); the limit is %.0f min",
			total/60, s.cfg.MaxFootageSecs/60))
		return
	}

	opts := pipeline.Options{
		Clips: p.clips, Prompt: p.prompt, Recipe: j.recipe,
		Out: filepath.Join(j.dir, "out.mp4"), WorkDir: filepath.Join(j.dir, "work"),
		Music: p.music, Duck: true, Title: p.title, Fade: 0.5,
		Progress: func(stage string, f float64) {
			j.set(func(j *job) { j.stage, j.progress = stage, f })
		},
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		s.failJob(j, "storage error")
		return
	}
	if ai != nil {
		opts.Planners = []plan.Planner{plan.LLM{AI: ai, Label: aiLabel}}
		opts.Vision = orient.Vision{AI: ai}
	}
	res, err := pipeline.Run(ctx, opts)
	if err != nil {
		log.Printf("job %s failed: %v", j.id, err)
		s.failJob(j, "could not edit these clips; try different footage")
		return
	}
	j.set(func(j *job) {
		j.status, j.stage, j.progress = "done", "Done", 1
		j.planner, j.edl, j.rev = res.Planner, &res.EDL, 1
	})
}

func (s *Server) failJob(j *job, msg string) {
	j.set(func(j *job) { j.status, j.stage, j.errMsg = "error", "Failed", msg })
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) *job {
	id := r.PathValue("id")
	if len(id) != 32 {
		fail(w, http.StatusNotFound, "no such job")
		return nil
	}
	if _, err := hex.DecodeString(id); err != nil {
		fail(w, http.StatusNotFound, "no such job")
		return nil
	}
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		fail(w, http.StatusNotFound, "no such job (results are kept for a limited time)")
		return nil
	}
	return j
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if j := s.lookup(w, r); j != nil {
		writeJSON(w, 200, j.view())
	}
}

func (s *Server) video(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	if v := j.view(); v.Status != "done" {
		fail(w, http.StatusConflict, "video is not ready")
		return
	}
	f, err := os.Open(filepath.Join(j.dir, "out.mp4"))
	if err != nil {
		fail(w, http.StatusNotFound, "video expired")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "video.mp4", st.ModTime(), f)
}

// reviseJob applies a plain-language change and re-renders.
func (s *Server) reviseJob(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	ai, _, err := s.askerFor(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ai == nil {
		fail(w, http.StatusNotImplemented, "plain-language edits need an AI connection: add your API key first")
		return
	}
	var body struct {
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil || strings.TrimSpace(body.Instruction) == "" || len(body.Instruction) > 500 {
		fail(w, http.StatusBadRequest, "send {\"instruction\": \"...\"} (max 500 characters)")
		return
	}
	var base *schema.EDL
	var busy bool
	j.set(func(j *job) {
		if j.status != "done" || j.edl == nil {
			busy = true
			return
		}
		j.status, j.stage, j.progress, j.errMsg = "running", "Applying your change", 0.1, ""
		e := j.edl.Clone()
		base = &e
	})
	if busy {
		fail(w, http.StatusConflict, "wait for the current edit to finish")
		return
	}
	task := func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.JobTimeout)
		defer cancel()
		next, err := revise.Apply(ctx, ai, *base, body.Instruction)
		if err == nil {
			err = renderRevision(j, next, base)
		}
		if err != nil {
			// Keep the previous version playable; report why the change failed.
			j.set(func(j *job) {
				j.status, j.stage, j.errMsg = "done", "Done", "could not apply that change: "+err.Error()
			})
			return
		}
		j.set(func(j *job) {
			j.status, j.stage, j.progress, j.errMsg = "done", "Done", 1, ""
			j.edl, j.rev = &next, j.rev+1
		})
	}
	select {
	case s.queue <- task:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": j.id})
	default:
		j.set(func(j *job) { j.status, j.stage = "done", "Done" })
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
	}
}

func renderRevision(j *job, next schema.EDL, prev *schema.EDL) error {
	p := project.New(*prev)
	if err := p.Replace(next); err != nil {
		return err
	}
	tmp := filepath.Join(j.dir, "out.next.mp4")
	if err := renderTo(j, p.EDL, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(j.dir, "out.mp4"))
}

func (s *Server) drop(id string) {
	s.mu.Lock()
	j := s.jobs[id]
	delete(s.jobs, id)
	s.mu.Unlock()
	if j != nil {
		os.RemoveAll(j.dir)
	}
}

func (s *Server) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(time.Now())
		}
	}
}

func (s *Server) sweep(now time.Time) {
	var expired []string
	s.mu.Lock()
	for id, j := range s.jobs {
		j.mu.Lock()
		old := now.Sub(j.created) > s.cfg.TTL && j.status != "running" && j.status != "queued"
		j.mu.Unlock()
		if old {
			expired = append(expired, id)
		}
	}
	s.mu.Unlock()
	for _, id := range expired {
		s.drop(id)
	}
}
