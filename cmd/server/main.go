// Command server runs the editor's HTTP API.
//
// Environment:
//
//	PORT            listen port (default 7860, what Hugging Face Spaces expects)
//	DATA_DIR        where uploads and results live (default: temp dir)
//	ALLOWED_ORIGIN  CORS origin, e.g. https://yourname.github.io (default *)
//	GEMINI_API_KEY  enables the AI planner and orientation check
//	GEMINI_MODEL    override the Gemini model name
//	MAX_UPLOAD_MB (default 100000), MAX_CLIPS (200), WORKERS, RATE_PER_HOUR, TTL_MINUTES  limits
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"filmeditor/internal/llm"
	"filmeditor/internal/server"
	"filmeditor/web"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	openBrowser := flag.Bool("open", false, "open the editor in your browser when it starts")
	flag.Parse()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("WARNING: ffmpeg was not found. Install it first:\n%s", ffmpegHelp())
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		if c, err := os.UserCacheDir(); err == nil {
			dataDir = filepath.Join(c, "splice")
		}
	}
	cfg := server.Config{
		DataDir:        dataDir,
		AllowedOrigin:  os.Getenv("ALLOWED_ORIGIN"),
		MaxUploadBytes: int64(envInt("MAX_UPLOAD_MB", 100000)) << 20,
		MaxClips:       envInt("MAX_CLIPS", 200),
		Workers:        envInt("WORKERS", 1),
		RatePerHour:    envInt("RATE_PER_HOUR", 6),
		TTL:            time.Duration(envInt("TTL_MINUTES", 60)) * time.Minute,
		UI:             web.FS,
		Port:           strconv.Itoa(envInt("PORT", 7860)),
	}
	if g := llm.NewGeminiFromEnv(); g != nil {
		cfg.AI, cfg.AILabel = *g, "gemini"
		log.Printf("AI: gemini (%s)", g.Model)
	} else {
		log.Printf("AI: none (rule-based planner only; set GEMINI_API_KEY to enable)")
	}
	s, err := server.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	addr := ":" + strconv.Itoa(envInt("PORT", 7860))
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	url := "http://localhost:" + strconv.Itoa(envInt("PORT", 7860))
	log.Printf("Splice is running: open %s", url)
	if *openBrowser {
		go func() { time.Sleep(500 * time.Millisecond); openURL(url) }()
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func ffmpegHelp() string {
	switch runtime.GOOS {
	case "darwin":
		return "  brew install ffmpeg"
	case "windows":
		return "  winget install Gyan.FFmpeg   (then restart this program)"
	default:
		return "  sudo apt install ffmpeg      (or your distro's equivalent)"
	}
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
