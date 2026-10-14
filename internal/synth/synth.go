// Package synth generates test footage with known, checkable content, so the
// pipeline's decisions (orientation, picking, pacing) can be asserted on.
package synth

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Scene names. Each is visually unambiguous to a person or a vision model.
const (
	Landscape   = "landscape"    // blue sky + yellow sun on top, green grass + tree at the bottom
	RedCircle   = "red_circle"   // one big red circle on a white background
	GreenSquare = "green_square" // one big green square on a black background
	Night       = "night"        // dark sky, white moon, small white stars
	Wall        = "wall"         // a plain, featureless wall: dead footage
)

var Scenes = []string{Landscape, RedCircle, GreenSquare, Night, Wall}

// Image renders a scene at w x h.
func Image(scene string, w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill := func(r image.Rectangle, c color.Color) {
		draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
	}
	disc := func(cx, cy, rad int, c color.Color) {
		for y := cy - rad; y <= cy+rad; y++ {
			for x := cx - rad; x <= cx+rad; x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= rad*rad && image.Pt(x, y).In(img.Bounds()) {
					img.Set(x, y, c)
				}
			}
		}
	}
	switch scene {
	case Landscape:
		fill(image.Rect(0, 0, w, h*6/10), color.RGBA{90, 170, 255, 255})
		fill(image.Rect(0, h*6/10, w, h), color.RGBA{50, 160, 60, 255})
		disc(w*8/10, h*15/100, h/10, color.RGBA{255, 220, 40, 255})
		fill(image.Rect(w*3/10-w/50, h*40/100, w*3/10+w/50, h*75/100), color.RGBA{110, 70, 30, 255})
		disc(w*3/10, h*34/100, h/9, color.RGBA{20, 110, 30, 255})
	case RedCircle:
		fill(img.Bounds(), color.White)
		disc(w/2, h/2, h/3, color.RGBA{220, 20, 20, 255})
	case GreenSquare:
		fill(img.Bounds(), color.Black)
		fill(image.Rect(w/2-h/3, h/2-h/3, w/2+h/3, h/2+h/3), color.RGBA{20, 220, 40, 255})
	case Night:
		fill(img.Bounds(), color.RGBA{8, 10, 40, 255})
		disc(w/4, h/4, h/8, color.RGBA{245, 245, 235, 255})
		for i := 0; i < 25; i++ {
			disc((i*7919)%w, (i*104729)%(h*6/10), 2, color.White)
		}
	case Wall:
		fill(img.Bounds(), color.RGBA{236, 232, 224, 255})
	default:
		panic("unknown scene " + scene)
	}
	return img
}

// Options control how a clip is stored.
type Options struct {
	Width, Height int     // stored size; default 640x360
	Seconds       float64 // default 3
	Sound         bool    // add a sine tone
	// UpsideDown stores the pixels rotated 180 degrees with NO metadata,
	// simulating a camera that was mounted/held upside down.
	UpsideDown bool
	// MetaRotateCCW tags the file with a display rotation (what phones do).
	MetaRotateCCW int
	// Texture adds film grain so the clip has the edge detail real footage has.
	Texture bool
}

// WriteClip renders a scene to an mp4 at path.
func WriteClip(path, scene string, o Options) error {
	if o.Width == 0 {
		o.Width, o.Height = 640, 360
	}
	if o.Seconds == 0 {
		o.Seconds = 3
	}
	dir := filepath.Dir(path)
	pngPath := path + ".png"
	f, err := os.Create(pngPath)
	if err != nil {
		return err
	}
	if err := png.Encode(f, Image(scene, o.Width, o.Height)); err != nil {
		f.Close()
		return err
	}
	f.Close()
	defer os.Remove(pngPath)

	raw := path
	if o.MetaRotateCCW != 0 {
		raw = filepath.Join(dir, ".raw_"+filepath.Base(path))
		defer os.Remove(raw)
	}
	args := []string{"-y", "-v", "error", "-loop", "1", "-framerate", "30", "-i", pngPath}
	if o.Sound {
		args = append(args, "-f", "lavfi", "-i", "sine=f=440")
	}
	args = append(args, "-t", fmt.Sprint(o.Seconds))
	var vf []string
	if o.UpsideDown {
		vf = append(vf, "hflip", "vflip")
	}
	if o.Texture {
		vf = append(vf, "noise=alls=40:allf=t")
	}
	if len(vf) > 0 {
		args = append(args, "-vf", strings.Join(vf, ","))
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p")
	if o.Sound {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, raw)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("synth clip: %w\n%s", err, b)
	}
	if o.MetaRotateCCW != 0 {
		b, err := exec.Command("ffmpeg", "-y", "-v", "error",
			"-display_rotation", strconv.Itoa(o.MetaRotateCCW), "-i", raw, "-c", "copy", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("tag rotation: %w\n%s", err, b)
		}
	}
	return nil
}

// Concat joins already-written clips back to back (used to make an
// "inspiration video" with a known cut rhythm).
func Concat(out string, clips ...string) error {
	args := []string{"-y", "-v", "error"}
	filter := ""
	for i, c := range clips {
		args = append(args, "-i", c)
		filter += fmt.Sprintf("[%d:v]scale=640:360,setsar=1,fps=30[v%d];", i, i)
	}
	for i := range clips {
		filter += fmt.Sprintf("[v%d]", i)
	}
	filter += fmt.Sprintf("concat=n=%d:v=1:a=0[v]", len(clips))
	args = append(args, "-filter_complex", filter, "-map", "[v]", "-c:v", "libx264", "-pix_fmt", "yuv420p", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("concat: %w\n%s", err, b)
	}
	return nil
}
