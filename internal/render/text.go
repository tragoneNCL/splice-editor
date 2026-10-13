package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"filmeditor/internal/schema"
)

// This ffmpeg build has no text filters (no libfreetype/libass), so titles and
// captions are rasterised here, to a full-canvas transparent PNG, and then
// composited with `overlay`. The font is embedded, so it works in a container.

// writeTextPNG renders t onto a transparent w x h canvas at path.
func writeTextPNG(path string, w, h int, t schema.Text) error {
	f, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return err
	}
	size := t.Size
	if size <= 0 {
		size = h / 18
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer face.Close()

	fg, err := parseColor(t.Color)
	if err != nil {
		return err
	}
	lines := wrap(face, t.Text, int(float64(w)*0.88))
	lineH := face.Metrics().Height.Ceil() * 11 / 10
	blockH := lineH * len(lines)

	var top int
	switch t.Position {
	case "top":
		top = h / 12
	case "center":
		top = (h - blockH) / 2
	default: // bottom
		top = h - blockH - h/9
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	d := &font.Drawer{Dst: img, Face: face}
	outline := image.NewUniform(color.RGBA{0, 0, 0, 255})
	body := image.NewUniform(fg)
	r := size/14 + 1 // outline thickness
	for i, line := range lines {
		width := font.MeasureString(face, line).Ceil()
		x := (w - width) / 2
		y := top + i*lineH + face.Metrics().Ascent.Ceil()
		d.Src = outline
		for dx := -r; dx <= r; dx++ {
			for dy := -r; dy <= r; dy++ {
				if dx*dx+dy*dy <= r*r {
					d.Dot = fixed.P(x+dx, y+dy)
					d.DrawString(line)
				}
			}
		}
		d.Src = body
		d.Dot = fixed.P(x, y)
		d.DrawString(line)
	}
	// Premultiplied alpha is what image/draw produced; encode as-is.
	out := image.NewNRGBA(img.Bounds())
	draw.Draw(out, out.Bounds(), img, image.Point{}, draw.Src)
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	return png.Encode(fh, out)
}

func wrap(face font.Face, text string, maxW int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if font.MeasureString(face, cur+" "+w).Ceil() <= maxW {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	return append(lines, cur)
}

func parseColor(s string) (color.RGBA, error) {
	if s == "" || strings.EqualFold(s, "white") {
		return color.RGBA{255, 255, 255, 255}, nil
	}
	if strings.EqualFold(s, "yellow") {
		return color.RGBA{255, 220, 0, 255}, nil
	}
	if strings.HasPrefix(s, "#") && len(s) == 7 {
		v, err := strconv.ParseUint(s[1:], 16, 32)
		if err == nil {
			return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}, nil
		}
	}
	return color.RGBA{}, fmt.Errorf("unknown color %q (use white, yellow or #rrggbb)", s)
}
