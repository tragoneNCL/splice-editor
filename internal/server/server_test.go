package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	cfg := Config{DataDir: t.TempDir(), RatePerHour: 100, PublicIPURL: "-"}
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

func TestReviseWithoutAIUsesRules(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL, map[string]string{"duration": "5"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)}})
	id := decode[map[string]string](t, resp)["id"]
	waitDone(t, ts.URL, id)

	post := func(instr string) *http.Response {
		r, err := http.Post(ts.URL+"/api/jobs/"+id+"/revise", "application/json",
			strings.NewReader(`{"instruction":"`+instr+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Something open-ended is refused immediately, with guidance.
	r := post("make it feel nostalgic")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "without AI") {
		t.Errorf("open-ended: status %d body %s", r.StatusCode, b)
	}
	// A supported command works with no AI configured.
	r = post("mute the first shot")
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("rule command status %d", r.StatusCode)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := http.Get(ts.URL + "/api/jobs/" + id)
		v := decode[JobView](t, resp)
		if v.Status == "done" && v.Error != "" {
			t.Fatalf("revision failed: %s", v.Error)
		}
		if v.Status == "done" && v.Revision == 2 {
			if v.EDL.Cuts[0].Volume == nil || *v.EDL.Cuts[0].Volume != 0 {
				t.Errorf("first cut not muted: %+v", v.EDL.Cuts[0])
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("revision never completed")
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

// putChunk sends one chunk of a chunked upload.
func putChunk(t *testing.T, base, uid, kind string, idx int, name string, total int64, offset int64, data []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, base+"/api/uploads/"+uid+"/chunk", bytes.NewReader(data))
	req.Header.Set("X-Kind", kind)
	req.Header.Set("X-Index", fmt.Sprint(idx))
	req.Header.Set("X-Name", name)
	req.Header.Set("X-Total", fmt.Sprint(total))
	req.Header.Set("X-Offset", fmt.Sprint(offset))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChunkedUploadOutOfOrderThenEdit(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r, err := http.Post(ts.URL+"/api/uploads", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	uid := decode[map[string]any](t, r)["upload_id"].(string)

	for idx, scene := range []string{synth.Landscape, synth.RedCircle} {
		data := clipBytes(t, scene, 3)
		total := int64(len(data))
		third := total / 3
		// Deliberately out of order, with one retry of the same chunk.
		parts := [][2]int64{{2 * third, total}, {0, third}, {0, third}, {third, 2 * third}}
		for _, p := range parts {
			resp := putChunk(t, ts.URL, uid, "clip", idx, fmt.Sprintf("c%d.mp4", idx), total, p[0], data[p[0]:p[1]])
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("chunk %v: %d %s", p, resp.StatusCode, b)
			}
			resp.Body.Close()
		}
	}
	body := `{"upload_id":"` + uid + `","duration":"6","preset":"1:1","prompt":"x"}`
	resp, err := http.Post(ts.URL+"/api/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "done" {
		t.Fatalf("job: %+v", v)
	}
	if len(v.EDL.Cuts) < 2 {
		t.Errorf("expected cuts from both uploaded clips, got %d", len(v.EDL.Cuts))
	}
}

func TestIncompleteChunkedUploadIsRejected(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r, _ := http.Post(ts.URL+"/api/uploads", "application/json", nil)
	uid := decode[map[string]any](t, r)["upload_id"].(string)
	data := clipBytes(t, synth.RedCircle, 2)
	resp := putChunk(t, ts.URL, uid, "clip", 0, "a.mp4", int64(len(data)), 0, data[:len(data)/2])
	resp.Body.Close()
	jr, _ := http.Post(ts.URL+"/api/jobs", "application/json", strings.NewReader(`{"upload_id":"`+uid+`"}`))
	b, _ := io.ReadAll(jr.Body)
	jr.Body.Close()
	if jr.StatusCode != 400 || !strings.Contains(string(b), "did not finish") {
		t.Errorf("status %d body %s", jr.StatusCode, b)
	}
}

func TestChunkLimitsAndValidation(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.MaxUploadBytes = 1 << 20 })
	r, _ := http.Post(ts.URL+"/api/uploads", "application/json", nil)
	uid := decode[map[string]any](t, r)["upload_id"].(string)

	cases := []struct {
		name string
		resp *http.Response
		want int
	}{
		{"bad type", putChunk(t, ts.URL, uid, "clip", 0, "x.exe", 10, 0, []byte("0123456789")), 400},
		{"too big", putChunk(t, ts.URL, uid, "clip", 0, "x.mp4", 5<<20, 0, []byte("abc")), 413},
		{"past end", putChunk(t, ts.URL, uid, "clip", 1, "x.mp4", 3, 0, []byte("abcdef")), 400},
		{"unknown upload", putChunk(t, ts.URL, strings.Repeat("a", 32), "clip", 0, "x.mp4", 3, 0, []byte("abc")), 404},
	}
	for _, c := range cases {
		c.resp.Body.Close()
		if c.resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, c.resp.StatusCode, c.want)
		}
	}
}

func TestReferenceLinkMustBeYouTube(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL, map[string]string{"inspo_url": "https://evil.example.com/v.mp4"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 1)}})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status %d, want 400 for a non-YouTube reference", resp.StatusCode)
	}
}

func TestUploadedReferenceIsDeletedAfterTheJob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp := upload(t, ts.URL, map[string]string{"duration": "5"}, []file{
		{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)},
		{"inspo", "ref.mp4", clipBytes(t, synth.Landscape, 3)},
	})
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "done" {
		t.Fatalf("job: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, id, "inspo.mp4")); err == nil {
		t.Error("reference video should be deleted after the job")
	}
}

const visitorA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const visitorB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func getJSON[T any](t *testing.T, url string, headers map[string]string) T {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return decode[T](t, resp)
}

func waitRev(t *testing.T, url, id string, rev int) JobView {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		v := getJSON[JobView](t, url+"/api/jobs/"+id, nil)
		if v.Status == "done" && v.Error != "" {
			t.Fatalf("job error: %s", v.Error)
		}
		if v.Status == "done" && v.Revision == rev {
			return v
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("revision %d never arrived", rev)
	return JobView{}
}

func TestUndoStepsBackAndPersists(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := upload(t, ts.URL, map[string]string{"duration": "5"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)}})
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.CanUndo {
		t.Error("a fresh edit has nothing to undo")
	}

	post := func(path, body string) int {
		r, err := http.Post(ts.URL+"/api/jobs/"+id+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post("/undo", ""); c != http.StatusBadRequest {
		t.Errorf("undo with nothing to undo: %d", c)
	}
	if c := post("/revise", `{"instruction":"mute the first shot"}`); c != http.StatusAccepted {
		t.Fatalf("revise: %d", c)
	}
	v = waitRev(t, ts.URL, id, 2)
	if v.EDL.Cuts[0].Volume == nil || !v.CanUndo {
		t.Fatalf("after revise: %+v", v)
	}
	if c := post("/undo", ""); c != http.StatusAccepted {
		t.Fatalf("undo: %d", c)
	}
	v = waitRev(t, ts.URL, id, 3)
	if v.EDL.Cuts[0].Volume != nil {
		t.Errorf("undo should restore the unmuted cut: %+v", v.EDL.Cuts[0])
	}
	if v.CanUndo {
		t.Error("back at the original, nothing further to undo")
	}
}

func TestJobsSurviveServerRestart(t *testing.T) {
	dir := t.TempDir()
	mk := func() (*httptest.Server, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		s, err := New(ctx, Config{DataDir: dir, RatePerHour: 100})
		if err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(s.Handler()), cancel
	}
	ts1, stop1 := mk()
	resp := upload(t, ts1.URL, map[string]string{"duration": "5", "title": "Keep me"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)}})
	id := decode[map[string]string](t, resp)["id"]
	before := waitDone(t, ts1.URL, id)
	ts1.Close()
	stop1()
	time.Sleep(200 * time.Millisecond)

	ts2, stop2 := mk()
	defer func() { ts2.Close(); stop2() }()
	after := getJSON[JobView](t, ts2.URL+"/api/jobs/"+id, nil)
	if after.Status != "done" || len(after.EDL.Cuts) != len(before.EDL.Cuts) || after.Revision != 1 {
		t.Fatalf("after restart: %+v", after)
	}
	vid, err := http.Get(ts2.URL + "/api/jobs/" + id + "/video")
	if err != nil {
		t.Fatal(err)
	}
	vid.Body.Close()
	if vid.StatusCode != 200 {
		t.Errorf("video after restart: %d", vid.StatusCode)
	}
}

func TestVisitorPrefsAreIsolatedAndWhitelisted(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	put := func(visitor, body string) int {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/me/prefs", strings.NewReader(body))
		if visitor != "" {
			req.Header.Set("X-Visitor", visitor)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := put("", `{}`); c != 400 {
		t.Errorf("no visitor: %d", c)
	}
	if c := put("not-hex", `{}`); c != 400 {
		t.Errorf("bad visitor: %d", c)
	}
	if c := put(visitorA, `{"preset":"1:1","duration":"45","apiKey":"sk-secret","evil":{"x":1}}`); c != 200 {
		t.Fatalf("put: %d", c)
	}
	me := getJSON[map[string]any](t, ts.URL+"/api/me", map[string]string{"X-Visitor": visitorA})
	prefs := me["prefs"].(map[string]any)
	if prefs["preset"] != "1:1" || prefs["duration"] != "45" {
		t.Errorf("prefs = %v", prefs)
	}
	if _, bad := prefs["apiKey"]; bad {
		t.Error("unlisted keys must be dropped: prefs are never a place to keep secrets")
	}
	if _, bad := prefs["evil"]; bad {
		t.Error("unlisted keys must be dropped")
	}
	other := getJSON[map[string]any](t, ts.URL+"/api/me", map[string]string{"X-Visitor": visitorB})
	if len(other["prefs"].(map[string]any)) != 0 {
		t.Errorf("prefs leaked to another visitor: %v", other["prefs"])
	}
}

func TestRecentEditsAreListedPerVisitor(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	body, ct := multipartBody(t, map[string]string{"duration": "5", "title": "Mine"},
		[]file{{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 2)}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/jobs", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Visitor", visitorA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	id := decode[map[string]string](t, resp)["id"]
	waitDone(t, ts.URL, id)

	mine := getJSON[struct{ Recent []recentJob }](t, ts.URL+"/api/me", map[string]string{"X-Visitor": visitorA})
	if len(mine.Recent) != 1 || mine.Recent[0].ID != id || !mine.Recent[0].Playable || mine.Recent[0].Title != "Mine" {
		t.Errorf("recent = %+v", mine.Recent)
	}
	theirs := getJSON[struct{ Recent []recentJob }](t, ts.URL+"/api/me", map[string]string{"X-Visitor": visitorB})
	if len(theirs.Recent) != 0 {
		t.Errorf("another visitor sees edits that aren't theirs: %+v", theirs.Recent)
	}
}

func multipartBody(t *testing.T, fields map[string]string, files []file) (*bytes.Buffer, string) {
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
	return &buf, mw.FormDataContentType()
}

// A provider that is down must never wedge a job: the edit finishes with the
// automatic editor, quickly, and says so.
func TestOverloadedAIFallsBackToAutomaticEditQuickly(t *testing.T) {
	overloaded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":503,"message":"This model is currently experiencing high demand."}}`, 503)
	}))
	defer overloaded.Close()
	ts, _ := newTestServer(t, func(c *Config) {
		c.AI = llm.Gemini{Key: "k", Model: "m", BaseURL: overloaded.URL, Tries: 2, Backoff: time.Millisecond}
		c.AILabel = "gemini"
	})
	start := time.Now()
	resp := upload(t, ts.URL, map[string]string{"duration": "5"}, []file{
		{"clips", "a.mp4", clipBytes(t, synth.RedCircle, 3)},
		{"clips", "b.mp4", clipBytes(t, synth.Landscape, 3)},
	})
	id := decode[map[string]string](t, resp)["id"]
	v := waitDone(t, ts.URL, id)
	if v.Status != "done" || v.Planner != "rule-based" {
		t.Fatalf("job = %+v", v)
	}
	if len(v.Notes) == 0 || !strings.Contains(v.Notes[len(v.Notes)-1], "busy") {
		t.Errorf("user should be told the AI was busy: %v", v.Notes)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("fallback took %v; an overloaded AI must not stall the edit", d)
	}
}

func TestHealthFlagsVisitorsOnTheServersOwnNetwork(t *testing.T) {
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.9\n"))
	}))
	defer ipSrv.Close()
	ts, _ := newTestServer(t, func(c *Config) { c.PublicIPURL = ipSrv.URL; c.Port = "7860" })

	var same map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) { // the public IP is learned in the background
		same = getJSON[map[string]any](t, ts.URL+"/api/health", map[string]string{"CF-Connecting-IP": "203.0.113.9"})
		if e, _ := same["extra"].(map[string]any); e["same_network"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	extra, _ := same["extra"].(map[string]any)
	if extra["same_network"] != true {
		t.Fatalf("same address not detected: %v", same)
	}
	urls, _ := extra["local_urls"].([]any)
	if len(urls) == 0 || urls[0] != "http://localhost:7860" {
		t.Errorf("local_urls = %v", urls)
	}

	other := getJSON[map[string]any](t, ts.URL+"/api/health", map[string]string{"CF-Connecting-IP": "198.51.100.7"})
	if e, _ := other["extra"].(map[string]any); e["same_network"] == true {
		t.Errorf("a visitor elsewhere must not be told to use local addresses: %v", other)
	}
}
