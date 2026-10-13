// Command genicons renders the app icons (gradient tile with the two-block
// mark). Run once; the PNGs are committed under web/icons.
//
//	go run ./scripts/genicons
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }

func gradient(t float64) color.RGBA {
	// violet -> pink -> orange, matching the site's accent gradient
	stops := []color.RGBA{{124, 92, 255, 255}, {255, 92, 147, 255}, {255, 179, 92, 255}}
	if t < 0.55 {
		u := t / 0.55
		return color.RGBA{lerp(stops[0].R, stops[1].R, u), lerp(stops[0].G, stops[1].G, u), lerp(stops[0].B, stops[1].B, u), 255}
	}
	u := (t - 0.55) / 0.45
	return color.RGBA{lerp(stops[1].R, stops[2].R, u), lerp(stops[1].G, stops[2].G, u), lerp(stops[1].B, stops[2].B, u), 255}
}

// inQuad reports whether (x,y) is inside the convex quad p0..p3.
func inQuad(x, y float64, p [4][2]float64) bool {
	sign := 0.0
	for i := 0; i < 4; i++ {
		a, b := p[i], p[(i+1)%4]
		c := (b[0]-a[0])*(y-a[1]) - (b[1]-a[1])*(x-a[0])
		if c == 0 {
			continue
		}
		if sign == 0 {
			sign = c
		} else if (c > 0) != (sign > 0) {
			return false
		}
	}
	return true
}

func render(size int, maskable bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	radius := s * 0.22
	if maskable {
		radius = 0 // full bleed; the OS applies its own mask
	}
	// Mark occupies the middle; maskable icons keep it inside the 80% safe zone.
	scale := 1.0
	if maskable {
		scale = 0.78
	}
	m := func(x, y float64) [2]float64 { return [2]float64{s * (0.5 + (x-0.5)*scale), s * (0.5 + (y-0.5)*scale)} }
	left := [4][2]float64{m(0.22, 0.30), m(0.48, 0.30), m(0.40, 0.70), m(0.14, 0.70)}
	right := [4][2]float64{m(0.52, 0.30), m(0.78, 0.30), m(0.70, 0.70), m(0.44, 0.70)}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			// rounded square mask
			if radius > 0 {
				cx := math.Min(math.Max(fx, radius), s-radius)
				cy := math.Min(math.Max(fy, radius), s-radius)
				if (fx-cx)*(fx-cx)+(fy-cy)*(fy-cy) > radius*radius {
					continue
				}
			}
			c := gradient((fx + fy) / (2 * s))
			if inQuad(fx, fy, left) || inQuad(fx, fy, right) {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func write(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func main() {
	dir := filepath.Join("web", "icons")
	os.MkdirAll(dir, 0o755)
	write(filepath.Join(dir, "icon-192.png"), render(192, false))
	write(filepath.Join(dir, "icon-512.png"), render(512, false))
	write(filepath.Join(dir, "icon-maskable-512.png"), render(512, true))
	write(filepath.Join(dir, "apple-touch-icon.png"), render(180, true))
}
