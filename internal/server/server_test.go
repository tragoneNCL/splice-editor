package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filmeditor/internal/ingest"
	"filmeditor/internal/llm"
	"filmeditor/internal/synth"
)

func newTestServer(t *testing.T, mod func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := Config{DataDir: t.TempDir(), RatePerHour: 100}
	if mod != nil {
		mod(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

type file struct {
	field, name string
	data        []byte
}

func upload(t *testing.T, url string, fields map[string]string, files []file) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for _, f := range files {
		fw, _ := mw.CreateFormFile(f.field, f.name)
		fw.Write(f.data)
	}
	mw.Close()
	resp, err := http.Post(url+"/api/jobs", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func clipBytes(t *testing.T, scene string, secs float64) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.mp4")
	if err := synth.WriteClip(p, scene, synth.Options{Seconds: secs, Sound: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	return b
}

func decode[T any](t *testing.T, r *http.Response) T {
	t.Helper()
	defer r.Body.Close()
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitDone(t *testing.T, url, id string) JobView {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/api/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		v := decode[JobView](t, resp)
		if v.Status == "done" || v.Status == "error" {
			return v
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return JobView{}
}

func TestEndToEndUploadEditDownload(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL,
		map[string]string{"prompt": "a calm edit", "preset": "1:1", "duration": "6", "title": "Hello"},
		[]file{
			{"clips", "a.mp4", clipBytes(t, synth.Landscape, 4)},
			{"clips", "b.mov", clipBytes(t, synth.RedCircle, 4)},
		})
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "done" {
		t.Fatalf("job ended %q: %s", v.Status, v.Error)
	}
	if v.EDL == nil || len(v.EDL.Cuts) == 0 || v.Planner != "rule-based" {
		t.Errorf("view = %+v", v)
	}

	vid, err := http.Get(ts.URL + "/api/jobs/" + id + "/video")
	if err != nil {
		t.Fatal(err)
	}
	defer vid.Body.Close()
	if vid.StatusCode != 200 || vid.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("video: %d %s", vid.StatusCode, vid.Header.Get("Content-Type"))
	}
	out := filepath.Join(t.TempDir(), "got.mp4")
	f, _ := os.Create(out)
	io.Copy(f, vid.Body)
	f.Close()
	info, err := ingest.Probe(out)
	if err != nil {
		t.Fatalf("downloaded file is not a video: %v", err)
	}
	if info.Width != 1080 || info.Height != 1080 {
		t.Errorf("output %dx%d, want 1080x1080 (preset 1:1)", info.Width, info.Height)
	}
	if info.Duration < 4 || info.Duration > 8 {
		t.Errorf("duration %.1f, want ~6", info.Duration)
	}
}

func TestRejectsBadUploads(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.MaxClips = 2 })
	good := clipBytes(t, synth.RedCircle, 1)
	cases := []struct {
		name   string
		fields map[string]string
		files  []file
		want   int
	}{
		{"no clips", nil, nil, 400},
		{"wrong type", nil, []file{{"clips", "evil.exe", []byte("MZ")}}, 400},
		{"too many", nil, []file{{"clips", "1.mp4", good}, {"clips", "2.mp4", good}, {"clips", "3.mp4", good}}, 400},
		{"bad preset", map[string]string{"preset": "7:3"}, []file{{"clips", "1.mp4", good}}, 400},
		{"bad duration", map[string]string{"duration": "9999"}, []file{{"clips", "1.mp4", good}}, 400},
		{"long prompt", map[string]string{"prompt": strings.Repeat("x", 2000)}, []file{{"clips", "1.mp4", good}}, 400},
	}
	for _, c := range cases {
		resp := upload(t, ts.URL, c.fields, c.files)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
		resp.Body.Close()
	}
}

func TestOversizedUploadIs413(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.MaxUploadBytes = 20 << 10 })
	resp := upload(t, ts.URL, nil, []file{{"clips", "big.mp4", bytes.Repeat([]byte("x"), 200<<10)}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
}

func TestNotAVideoFailsTheJobCleanly(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL, nil, []file{{"clips", "fake.mp4", []byte("this is not a video")}})
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "error" || v.Error == "" {
		t.Errorf("view = %+v, want a friendly error", v)
	}
	if strings.Contains(v.Error, "/") {
		t.Errorf("error leaks a path: %q", v.Error)
	}
}

func TestUploadedFilenamesCannotEscapeJobDir(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp := upload(t, ts.URL, nil, []file{{"clips", "../../../evil.mp4", clipBytes(t, synth.RedCircle, 1)}})
	id := decode[map[string]string](t, resp)["id"]
	waitDone(t, ts.URL, id)
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "..", "evil.mp4")); err == nil {
		t.Error("file escaped the data dir")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, id, "clip00.mp4")); err != nil {
		t.Errorf("clip should be stored under a generated name: %v", err)
	}
}

func TestUnknownAndMalformedJobIDs(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, id := range []string{"nope", strings.Repeat("a", 32), "../../etc/passwd", strings.Repeat("z", 32)} {
		resp, err := http.Get(ts.URL + "/api/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("id %q: status %d, want 404", id, resp.StatusCode)
		}
	}
}

func TestRateLimit(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.RatePerHour = 2 })
	var codes []int
	for i := 0; i < 3; i++ {
		resp := upload(t, ts.URL, nil, nil) // fails validation but still counts as an attempt
		codes = append(codes, resp.StatusCode)
		resp.Body.Close()
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("codes = %v, want third to be 429", codes)
	}
}

func TestCORSAndHealth(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.AllowedOrigin = "https://me.github.io" })
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/jobs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://me.github.io" {
		t.Errorf("CORS origin = %q", got)
	}
	h, _ := http.Get(ts.URL + "/api/health")
	m := decode[map[string]any](t, h)
	if m["ok"] != true || m["ai"] != "none" {
		t.Errorf("health = %v", m)
	}
}

func TestReviseNeedsAI(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL, nil, []file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)}})
	id := decode[map[string]string](t, resp)["id"]
	waitDone(t, ts.URL, id)
	r, err := http.Post(ts.URL+"/api/jobs/"+id+"/revise", "application/json",
		strings.NewReader(`{"instruction":"make it shorter"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501 without an AI backend", r.StatusCode)
	}
}

func TestSweepRemovesExpiredJobs(t *testing.T) {
	ts, s := newTestServer(t, func(c *Config) { c.TTL = time.Minute })
	resp := upload(t, ts.URL, nil, []file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 2)}})
	id := decode[map[string]string](t, resp)["id"]
	waitDone(t, ts.URL, id)
	s.sweep(time.Now().Add(2 * time.Minute))
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, id)); err == nil {
		t.Error("expired job directory should be removed")
	}
	r, _ := http.Get(ts.URL + "/api/jobs/" + id)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("expired job status %d, want 404", r.StatusCode)
	}
}

// scriptedAI answers revision prompts by slowing the first cut to half speed.
// For planning and orientation prompts it returns junk, so those fall back to
// the rule-based path, which is itself part of what is being tested.
type scriptedAI struct{}

func (scriptedAI) Ask(_ context.Context, prompt string, _ []llm.Image) (string, error) {
	const start, end = "Current project:\n", "\n\nInstruction:"
	i, j := strings.Index(prompt, start), strings.Index(prompt, end)
	if i < 0 || j < 0 {
		return "no json here", nil
	}
	var edl map[string]any
	if err := json.Unmarshal([]byte(prompt[i+len(start):j]), &edl); err != nil {
		return "", err
	}
	edl["cuts"].([]any)[0].(map[string]any)["speed"] = 0.5
	b, _ := json.Marshal(edl)
	return string(b), nil
}

func TestReviseReRendersAndBumpsRevision(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.AI, c.AILabel = scriptedAI{}, "scripted" })
	resp := upload(t, ts.URL, map[string]string{"duration": "5"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)}})
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "done" || v.Revision != 1 {
		t.Fatalf("first render: %+v", v)
	}
	before := v.EDL.Cuts[0].End - v.EDL.Cuts[0].Start

	r, err := http.Post(ts.URL+"/api/jobs/"+id+"/revise", "application/json",
		strings.NewReader(`{"instruction":"slow the first shot"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("revise status %d", r.StatusCode)
	}
	// Wait for the revision counter to move.
	deadline := time.Now().Add(60 * time.Second)
	var got JobView
	for time.Now().Before(deadline) {
		resp, _ := http.Get(ts.URL + "/api/jobs/" + id)
		got = decode[JobView](t, resp)
		if got.Status == "done" && got.Revision == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got.Revision != 2 || got.EDL.Cuts[0].Speed != 0.5 {
		t.Fatalf("after revise: %+v", got)
	}
	vid, err := http.Get(ts.URL + "/api/jobs/" + id + "/video")
	if err != nil {
		t.Fatal(err)
	}
	defer vid.Body.Close()
	out := filepath.Join(t.TempDir(), "r.mp4")
	f, _ := os.Create(out)
	io.Copy(f, vid.Body)
	f.Close()
	info, err := ingest.Probe(out)
	if err != nil {
		t.Fatal(err)
	}
	// Half speed doubles the first cut's screen time.
	if info.Duration < before*1.5 {
		t.Errorf("duration %.1f after slow-mo of a %.1fs cut; expected it to grow", info.Duration, before)
	}
}
