// Command auto edits a folder of clips into one video, hands-off.
//
//	auto -clips ./footage -prompt "upbeat travel reel" -out final.mp4
//
// The planner is chosen from the environment: GEMINI_API_KEY uses Gemini;
// otherwise -planner claude uses the local `claude` CLI; -planner rule needs no AI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filmeditor/internal/llm"
	"filmeditor/internal/orient"
	"filmeditor/internal/pipeline"
	"filmeditor/internal/schema"
)

var videoExt = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true}

func main() {
	dir := flag.String("clips", "", "folder of source clips")
	prompt := flag.String("prompt", "", "what kind of video you want")
	out := flag.String("out", "out.mp4", "output file")
	saveEDL := flag.String("save-edl", "", "also write the chosen edit list to this JSON file")
	which := flag.String("planner", "auto", "auto (Gemini if GEMINI_API_KEY set, else claude CLI), claude, gemini or rule")
	preset := flag.String("preset", "9:16", "output aspect ratio: 9:16, 1:1, 4:5 or 16:9")
	music := flag.String("music", "", "background music file")
	duck := flag.Bool("duck", true, "lower the music under the clips' own audio")
	title := flag.String("title", "", "title text shown at the start")
	fade := flag.Float64("fade", 0.5, "fade in/out seconds (0 = none)")
	work := flag.String("work", "", "folder for sampled frames (default: a temp dir)")
	r := schema.DefaultRecipe()
	flag.Float64Var(&r.TargetDuration, "duration", r.TargetDuration, "target length in seconds")
	flag.Float64Var(&r.AvgShotLength, "shot", r.AvgShotLength, "average shot length in seconds")
	flag.Parse()

	canvas, ok := schema.Presets[*preset]
	if !ok {
		fatal(fmt.Errorf("unknown preset %q", *preset))
	}
	r.Canvas = canvas
	if *dir == "" {
		flag.Usage()
		os.Exit(2)
	}
	paths, err := findClips(*dir)
	if err != nil {
		fatal(err)
	}
	workDir := *work
	if workDir == "" {
		if workDir, err = os.MkdirTemp("", "filmeditor-*"); err != nil {
			fatal(err)
		}
		defer os.RemoveAll(workDir)
	}

	var ai llm.Asker
	switch *which {
	case "rule":
	case "gemini":
		g := llm.NewGeminiFromEnv()
		if g == nil {
			fatal(fmt.Errorf("GEMINI_API_KEY is not set"))
		}
		ai = *g
	case "claude":
		ai = llm.CLI{}
	default: // auto
		if g := llm.NewGeminiFromEnv(); g != nil {
			ai = *g
		} else {
			ai = llm.CLI{}
		}
	}

	opts := pipeline.Options{
		Clips: paths, Prompt: *prompt, Recipe: r, Out: *out, WorkDir: workDir,
		Music: *music, Duck: *duck, Title: *title, Fade: *fade,
		Progress: func(stage string, f float64) { fmt.Printf("[%3.0f%%] %s\n", f*100, stage) },
	}
	if ai != nil {
		opts.Vision = orient.Vision{AI: ai}
		opts.StyleAI = ai
	}
	res, err := pipeline.Run(context.Background(), opts)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("planned %d cuts using %s\n", len(res.EDL.Cuts), res.Planner)
	for p, deg := range res.EDL.Fixes {
		fmt.Printf("rotated %s by %d degrees\n", filepath.Base(p), deg)
	}
	if *saveEDL != "" {
		b, _ := json.MarshalIndent(res.EDL, "", "  ")
		if err := os.WriteFile(*saveEDL, b, 0o644); err != nil {
			fatal(err)
		}
	}
	fmt.Println("wrote", *out)
}

func findClips(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if videoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no video files in %s", dir)
	}
	return paths, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
