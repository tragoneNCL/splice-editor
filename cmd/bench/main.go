// Command bench measures the whole system on realistic footage: generate
// phone-like clips, upload them through the real chunked-upload API, run the
// real pipeline, and report time per stage with real-time factors.
//
//	go run ./cmd/bench -clips 6 -secs 120 -mbps 20 -res 1080
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"filmeditor/internal/server"
)

func main() {
	n := flag.Int("clips", 6, "number of clips")
	secs := flag.Int("secs", 120, "seconds per clip")
	mbps := flag.Int("mbps", 20, "source bitrate in Mbit/s (phone 1080p is ~17, 4K ~50)")
	res := flag.Int("res", 1080, "source height: 1080 or 2160")
	target := flag.Int("target", 30, "output length in seconds")
	dir := flag.String("dir", filepath.Join(os.TempDir(), "splice-bench"), "where footage is generated")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	w := *res * 16 / 9
	var paths []string
	var total int64
	fmt.Printf("== footage: %d × %ds %dp @ %d Mbit/s\n", *n, *secs, *res, *mbps)
	genStart := time.Now()
	for i := 0; i < *n; i++ {
		p := filepath.Join(*dir, fmt.Sprintf("bench_%dp_%ds_%dm_%02d.mp4", *res, *secs, *mbps, i))
		if st, err := os.Stat(p); err != nil || st.Size() < 1<<20 {
			// Noise keeps the bitrate honest: flat test patterns compress to nothing.
			args := []string{"-y", "-v", "error",
				"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30:d=%d", w, *res, *secs),
				"-f", "lavfi", "-i", fmt.Sprintf("sine=f=%d:d=%d", 300+i*40, *secs),
				"-vf", "noise=alls=30:allf=t",
				"-c:v", "libx264", "-preset", "ultrafast", "-b:v", fmt.Sprintf("%dM", *mbps),
				"-maxrate", fmt.Sprintf("%dM", *mbps), "-bufsize", fmt.Sprintf("%dM", *mbps*2),
				"-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", p}
			if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				fatal(fmt.Errorf("generate: %v\n%s", err, b))
			}
		}
		st, _ := os.Stat(p)
		total += st.Size()
		paths = append(paths, p)
	}
	fmt.Printf("   generated/reused in %.0fs, total %.0f MB\n", time.Since(genStart).Seconds(), float64(total)/1e6)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data, _ := os.MkdirTemp("", "bench-data-*")
	defer os.RemoveAll(data)
	s, err := server.New(ctx, server.Config{DataDir: data, RatePerHour: 1000, Workers: 1})
	if err != nil {
		fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// ── upload ──
	t0 := time.Now()
	var sess struct {
		UploadID   string `json:"upload_id"`
		ChunkBytes int64  `json:"chunk_bytes"`
	}
	mustJSON(http.Post(ts.URL+"/api/uploads", "application/json", nil))(&sess)
	type task struct {
		idx  int
		path string
		off  int64
		size int64
	}
	var tasks []task
	for i, p := range paths {
		st, _ := os.Stat(p)
		for off := int64(0); off < st.Size(); off += sess.ChunkBytes {
			tasks = append(tasks, task{i, p, off, st.Size()})
		}
	}
	ch := make(chan task)
	var wg sync.WaitGroup
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				f, _ := os.Open(t.path)
				buf := make([]byte, min64(sess.ChunkBytes, t.size-t.off))
				f.ReadAt(buf, t.off)
				f.Close()
				req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/uploads/"+sess.UploadID+"/chunk", bytes.NewReader(buf))
				req.Header.Set("X-Kind", "clip")
				req.Header.Set("X-Index", fmt.Sprint(t.idx))
				req.Header.Set("X-Name", filepath.Base(t.path))
				req.Header.Set("X-Offset", fmt.Sprint(t.off))
				req.Header.Set("X-Total", fmt.Sprint(t.size))
				resp, err := http.DefaultClient.Do(req)
				if err != nil || resp.StatusCode != 200 {
					fatal(fmt.Errorf("chunk upload failed: %v", err))
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for _, t := range tasks {
		ch <- t
	}
	close(ch)
	wg.Wait()
	upload := time.Since(t0)

	// ── job ──
	body, _ := json.Marshal(map[string]string{"upload_id": sess.UploadID, "duration": fmt.Sprint(*target), "preset": "9:16", "prompt": "bench"})
	var created struct{ ID string }
	mustJSON(http.Post(ts.URL+"/api/jobs", "application/json", bytes.NewReader(body)))(&created)
	jobStart := time.Now()
	stageAt := map[string]time.Time{}
	var order []string
	var last string
	for {
		var v struct{ Status, Stage, Error string }
		mustJSON(http.Get(ts.URL + "/api/jobs/" + created.ID))(&v)
		key := stageKey(v.Stage)
		if key != last {
			stageAt[key] = time.Now()
			order = append(order, key)
			last = key
		}
		if v.Status == "error" {
			fatal(fmt.Errorf("job failed: %s", v.Error))
		}
		if v.Status == "done" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	jobEnd := time.Now()

	mb := float64(total) / 1e6
	footageSecs := float64(*n * *secs)
	fmt.Println("\n== results")
	fmt.Printf("   upload (local, 4 streams)  %7.1fs   %8.0f MB/s\n", upload.Seconds(), mb/upload.Seconds())
	prev := jobStart
	for i, k := range order {
		end := jobEnd
		if i+1 < len(order) {
			end = stageAt[order[i+1]]
		}
		_ = prev
		fmt.Printf("   %-26s %7.1fs\n", k, end.Sub(stageAt[k]).Seconds())
	}
	proc := jobEnd.Sub(jobStart)
	fmt.Printf("   ── processing total        %7.1fs   (%.1f× real time on %.0f min of footage)\n",
		proc.Seconds(), footageSecs/proc.Seconds(), footageSecs/60)
	fmt.Printf("   ── end to end              %7.1fs   for %.0f MB → %ds video\n", (upload + proc).Seconds(), mb, *target)
}

func stageKey(s string) string {
	switch {
	case len(s) >= 9 && s[:9] == "Analyzing":
		return "Analyze clips"
	case len(s) >= 8 && s[:8] == "Studying":
		return "Study reference"
	}
	return s
}

func mustJSON(resp *http.Response, err error) func(any) {
	if err != nil {
		fatal(err)
	}
	return func(v any) {
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			fatal(fmt.Errorf("http %d: %s", resp.StatusCode, b))
		}
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			fatal(err)
		}
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
