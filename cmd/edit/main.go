// Command edit renders an EDL JSON file to an mp4.
//
//	edit -edl edit.json -out final.mp4 [-w 1080 -h 1920 -fps 30]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"filmeditor/internal/render"
	"filmeditor/internal/schema"
)

func main() {
	edlPath := flag.String("edl", "", "path to EDL JSON ({\"cuts\":[{\"clip\",\"start\",\"end\"}]})")
	out := flag.String("out", "out.mp4", "output file")
	r := schema.DefaultRecipe()
	flag.IntVar(&r.Canvas.Width, "w", r.Canvas.Width, "canvas width")
	flag.IntVar(&r.Canvas.Height, "h", r.Canvas.Height, "canvas height")
	flag.IntVar(&r.Canvas.FPS, "fps", r.Canvas.FPS, "frame rate")
	flag.Parse()

	if *edlPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	raw, err := os.ReadFile(*edlPath)
	if err != nil {
		fatal(err)
	}
	var edl schema.EDL
	if err := json.Unmarshal(raw, &edl); err != nil {
		fatal(err)
	}
	if err := render.Render(edl, r, *out); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", *out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
