// Command synth writes a small set of test clips with known content,
// including upside-down and phone-rotated ones, for trying the editor.
//
//	synth -out ./testfootage
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"filmeditor/internal/synth"
)

func main() {
	out := flag.String("out", "testfootage", "folder to write clips into")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	clips := []struct {
		name  string
		scene string
		opt   synth.Options
	}{
		{"01_landscape.mp4", synth.Landscape, synth.Options{Seconds: 6, Sound: true}},
		{"02_red_circle.mp4", synth.RedCircle, synth.Options{Seconds: 4}},
		{"03_landscape_UPSIDE_DOWN.mp4", synth.Landscape, synth.Options{Seconds: 5, UpsideDown: true}},
		{"04_green_square_phone_rotated.mp4", synth.GreenSquare, synth.Options{Seconds: 4, MetaRotateCCW: 90}},
		{"05_night.mp4", synth.Night, synth.Options{Seconds: 5}},
	}
	for _, c := range clips {
		p := filepath.Join(*out, c.name)
		if err := synth.WriteClip(p, c.scene, c.opt); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", p)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
