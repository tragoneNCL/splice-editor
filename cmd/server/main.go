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
	"log"
	"net/http"
	"os"
	"os/signal"
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

	cfg := server.Config{
		DataDir:        os.Getenv("DATA_DIR"),
		AllowedOrigin:  os.Getenv("ALLOWED_ORIGIN"),
		MaxUploadBytes: int64(envInt("MAX_UPLOAD_MB", 100000)) << 20,
		MaxClips:       envInt("MAX_CLIPS", 200),
		Workers:        envInt("WORKERS", 1),
		RatePerHour:    envInt("RATE_PER_HOUR", 6),
		TTL:            time.Duration(envInt("TTL_MINUTES", 60)) * time.Minute,
		UI:             web.FS,
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
	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
