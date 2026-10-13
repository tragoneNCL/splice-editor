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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"filmeditor/internal/ingest"
	"filmeditor/internal/inspo"
	"filmeditor/internal/llm"
	"filmeditor/internal/orient"
	"filmeditor/internal/pipeline"
	"filmeditor/internal/plan"
	"filmeditor/internal/project"
	"filmeditor/internal/revise"
	"filmeditor/internal/schema"
	"filmeditor/internal/store"
	"filmeditor/internal/update"
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
	MinFreeBytes   int64 // keep at least this much disk free
	// PublicIPURL is queried to learn the server's public address ("-" = off).
	PublicIPURL string
	// Port is the listen port, used to build direct local addresses.
	Port string
	// RequireAuth makes every API call (except health and sign-in) need a
	// signed-in account. ClosedSignup allows only the first account.
	RequireAuth  bool
	ClosedSignup bool
	// Version is shown in the UI; Updater and Restart enable self-update.
	Version string
	Updater *update.Updater
	Restart func()
	// DBPath is the SQLite file; "-" disables persistence. Default: DataDir/splice.db.
	DBPath string

	// AI is optional. Without it the rule-based planner is used and
	// plain-language revisions are unavailable.
	AI      llm.Asker
	AILabel string

	// UI, if set, is served at / so one container hosts site and API.
	UI fs.FS
}

func (c *Config) defaults() {
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = 100000 << 20 // 100,000 MB per edit
	}
	if c.MaxClips == 0 {
		c.MaxClips = 200
	}
	if c.MaxFootageSecs == 0 {
		c.MaxFootageSecs = 6 * 3600
	}
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = 5 << 30
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
	if c.RatePerHour == 0 {
		c.RatePerHour = 6
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 4 * time.Hour
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(os.TempDir(), "filmeditor-data")
	}
}

type Server struct {
	cfg   Config
	queue chan func()

	store    *store.Store
	authHits map[string][]time.Time
	updates  updateCache
	net      netInfo
	port     string
	mu       sync.Mutex
	jobs     map[string]*job
	hits     map[string][]time.Time
	uploads  map[string]*uploadSession
}

type edlT = schema.EDL

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
	parent   int // revision this one was derived from (0 = original); drives undo
	notes    []string
	visitor  string
	title    string
	preset   string
	prompt   string

	// Project fields.
	name          string
	mediaIDs      []string
	settings      projectSettings
	keepReference bool // true when the inputs are library files (never deleted by a job)
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
	Notes    []string    `json:"notes,omitempty"`
	CanUndo  bool        `json:"can_undo"`
	EDL      *schema.EDL `json:"edl,omitempty"`
}

func (j *job) view() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobView{ID: j.id, Status: j.status, Stage: j.stage, Progress: j.progress,
		Error: j.errMsg, Planner: j.planner, Revision: j.rev, Notes: j.notes, CanUndo: j.parent > 0, EDL: j.edl}
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
		jobs: map[string]*job{}, hits: map[string][]time.Time{}, authHits: map[string][]time.Time{}, uploads: map[string]*uploadSession{}}
	if cfg.DBPath != "-" {
		path := cfg.DBPath
		if path == "" {
			path = filepath.Join(cfg.DataDir, "splice.db")
		}
		st, err := store.Open(path)
		if err != nil {
			return nil, err
		}
		s.store = st
		s.recoverJobs()
		go func() { <-ctx.Done(); st.Close() }()
	}
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
	s.port = cfg.Port
	if s.port == "" {
		s.port = "7860"
	}
	go s.watchPublicIP(ctx, cfg.PublicIPURL)
	go s.cleanupLoop(ctx)
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/auth/signup", s.signup)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/auth/me", s.authMe)
	mux.HandleFunc("POST /api/auth/password", s.changePassword)
	mux.HandleFunc("POST /api/verify", s.verify)
	mux.HandleFunc("GET /api/update", s.updateInfo)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("POST /api/ping", func(w http.ResponseWriter, r *http.Request) {
		// Speed probe: swallow up to 4 MB so the client can time the upload.
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, 4<<20))
		writeJSON(w, 200, map[string]any{"ok": true, "bytes": n})
	})
	mux.HandleFunc("POST /api/uploads", s.createUpload)
	mux.HandleFunc("POST /api/uploads/{id}/commit", s.commitUpload)
	mux.HandleFunc("GET /api/media", s.listMedia)
	mux.HandleFunc("GET /api/media/{id}/thumb", s.mediaThumb)
	mux.HandleFunc("DELETE /api/media/{id}", s.deleteMedia)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.patchProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{id}/generate", s.generateProject)
	mux.HandleFunc("GET /api/projects/{id}/video", s.video)
	mux.HandleFunc("POST /api/projects/{id}/revise", s.reviseJob)
	mux.HandleFunc("POST /api/projects/{id}/undo", s.undo)
	mux.HandleFunc("PUT /api/uploads/{id}/chunk", s.uploadChunk)
	mux.HandleFunc("POST /api/jobs", s.create)
	mux.HandleFunc("GET /api/jobs/{id}", s.status)
	mux.HandleFunc("GET /api/jobs/{id}/video", s.video)
	mux.HandleFunc("POST /api/jobs/{id}/revise", s.reviseJob)
	mux.HandleFunc("POST /api/jobs/{id}/undo", s.undo)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("PUT /api/me/prefs", s.setPrefs)
	if s.cfg.UI != nil {
		static := http.FileServerFS(s.cfg.UI)
		mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The UI ships inside the binary and changes with it; never let a
			// browser keep an old script (it caused a "stuck at 0%" report).
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			static.ServeHTTP(w, r)
		}))
	}
	return s.cors(s.withAuth(mux))
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if s.cfg.AllowedOrigin == "" {
			// Same-origin only (the safe default for a locally run app: other
			// websites cannot drive it from the visitor's browser).
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", s.cfg.AllowedOrigin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-Splice, X-AI-Provider, X-AI-Key, X-Visitor, X-Kind, X-Index, X-Name, X-Offset, X-Total")
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

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ai := "none"
	if s.cfg.AI != nil {
		ai = s.cfg.AILabel
	}
	extra := map[string]any{}
	if s.sameNetwork(r) {
		extra["same_network"] = true
		extra["local_urls"] = localURLs(s.port)
	}
	_, ffErr := exec.LookPath("ffmpeg")
	_, fpErr := exec.LookPath("ffprobe")
	writeJSON(w, 200, map[string]any{"ok": true, "ai": ai, "server_ai": s.cfg.AI != nil, "max_clips": s.cfg.MaxClips,
		"ffmpeg":        ffErr == nil && fpErr == nil,
		"version":       s.cfg.Version,
		"extra":         extra,
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
		if errors.Is(err, llm.ErrBusy) {
			// The provider answered, so the key is real; it is just overloaded.
			// Accept it, and say edits may fall back to automatic for now.
			writeJSON(w, 200, map[string]any{"ok": true, "provider": label,
				"warning": "Your key works, but the AI service is very busy right now. Edits will use the automatic editor until it frees up."})
			return
		}
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
	inspo    string // reference video file
	inspoURL string // YouTube link
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
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		s.createFromUpload(w, r)
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

	s.launch(w, r, id, dir, p, ai, aiLabel)
}

// launch registers the job and queues it.
func (s *Server) launch(w http.ResponseWriter, r *http.Request, id, dir string, p params, ai llm.Asker, aiLabel string) {
	canvas := schema.Presets[p.preset]
	rec := schema.DefaultRecipe()
	rec.Canvas, rec.TargetDuration = canvas, p.duration
	j := &job{id: id, dir: dir, created: time.Now(), status: "queued", stage: "Queued", recipe: rec, clips: p.clips,
		visitor: s.actor(r), title: p.title, preset: p.preset, prompt: p.prompt,
		name: projectNameFor(p.title), settings: projectSettings{Prompt: p.prompt, Preset: p.preset, Duration: p.duration, Title: p.title}}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	s.persist(j)

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
		case "inspo":
			if !allowedExt[ext] {
				return params{}, fmt.Errorf("the reference must be a video file")
			}
			dst, err := savePart(part, filepath.Join(dir, "inspo"+ext))
			if err != nil {
				return params{}, err
			}
			p.inspo = dst
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
	case "inspo_url":
		u, err := inspo.ValidateYouTubeURL(val)
		if err != nil {
			return err
		}
		p.inspoURL = u
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
	// A reference video is only measured, never kept: remove it as soon as
	// the job ends, whatever the outcome.
	if p.inspo != "" {
		defer func() {
			j.mu.Lock()
			keep := j.keepReference
			j.mu.Unlock()
			if !keep {
				os.Remove(p.inspo)
			}
		}()
	}

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
		Inspo: p.inspo, InspoURL: p.inspoURL,
		Progress: func(stage string, f float64) {
			j.set(func(j *job) { j.stage, j.progress = stage, f })
		},
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		s.failJob(j, "storage error")
		return
	}
	var guarded *llm.Breaker
	if ai != nil {
		// One failure disables the AI for the rest of this job so it falls
		// straight back to the automatic edit instead of re-waiting per step.
		guarded = llm.NewBreaker(ai)
		opts.Planners = []plan.Planner{plan.LLM{AI: guarded, Label: aiLabel}}
		opts.Vision = orient.Vision{AI: guarded}
		opts.StyleAI = guarded
	}
	res, err := pipeline.Run(ctx, opts)
	if err == nil && guarded != nil && guarded.Err() != nil {
		msg := "The AI couldn't be reached for this edit, so it used the automatic editor."
		if errors.Is(guarded.Err(), llm.ErrBusy) {
			msg = "The AI service was too busy for this edit, so it used the automatic editor. Try again in a few minutes."
		}
		res.Notes = append(res.Notes, msg)
	}
	if err != nil {
		log.Printf("job %s failed: %v", j.id, err)
		s.failJob(j, "could not edit these clips; try different footage")
		return
	}
	j.set(func(j *job) {
		j.status, j.stage, j.progress = "done", "Done", 1
		j.planner, j.edl, j.notes = res.Planner, &res.EDL, res.Notes
		if j.rev > 0 { // regenerating: the previous version stays reachable through undo
			j.parent, j.rev = j.rev, j.rev+1
		} else {
			j.parent, j.rev = 0, 1
		}
	})
	s.saveRevision(j, "generate")
	s.persist(j)
}

func (s *Server) failJob(j *job, msg string) {
	j.set(func(j *job) { j.status, j.stage, j.errMsg = "error", "Failed", msg })
	s.persist(j)
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
	if j == nil || (s.cfg.RequireAuth && j.visitor != s.actor(r)) {
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
	var body struct {
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil || strings.TrimSpace(body.Instruction) == "" || len(body.Instruction) > 500 {
		fail(w, http.StatusBadRequest, "send {\"instruction\": \"...\"} (max 500 characters)")
		return
	}
	if ai == nil {
		// No AI: common commands still work through the deterministic rules.
		// Say right away if this one is not among them.
		if cur := j.view().EDL; cur != nil {
			if _, _, err := revise.Rules(*cur, body.Instruction); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
		}
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
		var next schema.EDL
		var err error
		if ai != nil {
			next, err = revise.Apply(ctx, ai, *base, body.Instruction)
		} else {
			next, _, err = revise.Rules(*base, body.Instruction)
		}
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
			j.parent = j.rev
			j.edl, j.rev = &next, j.rev+1
		})
		s.saveRevision(j, body.Instruction)
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
		if s.store != nil {
			_ = s.store.DeleteJob(id)
		}
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
		old := !s.cfg.RequireAuth && now.Sub(j.created) > s.cfg.TTL && j.status != "running" && j.status != "queued"
		j.mu.Unlock()
		if old {
			expired = append(expired, id)
		}
	}
	s.mu.Unlock()
	for _, id := range expired {
		s.drop(id)
	}
	// Abandoned chunked uploads.
	var stale []string
	s.mu.Lock()
	for id, u := range s.uploads {
		if now.Sub(u.created) > 12*time.Hour {
			stale = append(stale, id)
			delete(s.uploads, id)
			os.RemoveAll(u.dir)
		}
	}
	s.mu.Unlock()
	_ = stale
}

func projectNameFor(title string) string {
	if t := cleanName(title); t != "" {
		return t
	}
	return "Untitled project"
}

func validateYouTube(u string) (string, error) { return inspo.ValidateYouTubeURL(u) }
