package understand

import (
	"image"
	_ "image/jpeg" // register the decoder for thumbnails
	_ "image/png"
	"math/bits"
	"os"
)

// HashFile returns a 64-bit difference hash of the image at path. Near-identical
// frames (the same room framed the same way) differ in only a few bits, which
// lets repeated shots be found without understanding what is in them.
func HashFile(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0, err
	}
	return dHash(img), nil
}

// dHash shrinks to 9x8 grey cells (box averaged) and records, for each row,
// whether each cell is brighter than its right neighbour.
func dHash(img image.Image) uint64 {
	b := img.Bounds()
	const cols, rows = 9, 8
	var grid [rows][cols]float64
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			x0 := b.Min.X + c*b.Dx()/cols
			x1 := b.Min.X + (c+1)*b.Dx()/cols
			y0 := b.Min.Y + r*b.Dy()/rows
			y1 := b.Min.Y + (r+1)*b.Dy()/rows
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var sum float64
			var n int
			// Sample on a coarse lattice: plenty for a 9x8 summary.
			stepX, stepY := max(1, (x1-x0)/6), max(1, (y1-y0)/6)
			for y := y0; y < y1; y += stepY {
				for x := x0; x < x1; x += stepX {
					r32, g32, b32, _ := img.At(x, y).RGBA()
					sum += 0.299*float64(r32>>8) + 0.587*float64(g32>>8) + 0.114*float64(b32>>8)
					n++
				}
			}
			grid[r][c] = sum / float64(n)
		}
	}
	var h uint64
	for r := 0; r < rows; r++ {
		for c := 0; c < cols-1; c++ {
			h <<= 1
			if grid[r][c] > grid[r][c+1] {
				h |= 1
			}
		}
	}
	return h
}

// Distance is the number of differing bits between two hashes.
func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Cluster assigns Group ids so that shots that look alike (hash distance <=
// maxDist), within or across clips, share a non-zero group. Singletons stay 0.
func Cluster(clips []ClipSummary, maxDist int) {
	type ref struct{ c, s int }
	var refs []ref
	for ci, c := range clips {
		for si, s := range c.Shots {
			if s.Hash != 0 {
				refs = append(refs, ref{ci, si})
			}
		}
	}
	parent := make([]int, len(refs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := range refs {
		for j := i + 1; j < len(refs); j++ {
			if Distance(clips[refs[i].c].Shots[refs[i].s].Hash, clips[refs[j].c].Shots[refs[j].s].Hash) <= maxDist {
				parent[find(i)] = find(j)
			}
		}
	}
	size := map[int]int{}
	for i := range refs {
		size[find(i)]++
	}
	ids := map[int]int{}
	for i, r := range refs {
		root := find(i)
		g := 0
		if size[root] > 1 {
			if _, ok := ids[root]; !ok {
				ids[root] = len(ids) + 1
			}
			g = ids[root]
		}
		clips[r.c].Shots[r.s].Group = g
	}
}
