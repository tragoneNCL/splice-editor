// Command analyze prints what Splice understands about each clip in a folder:
// usable shots (blank footage removed), quality, detail and motion.
//
//	analyze samples/property-1/raw
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"filmeditor/internal/understand"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: analyze <folder>")
		os.Exit(2)
	}
	paths, _ := filepath.Glob(filepath.Join(os.Args[1], "*.mp4"))
	more, _ := filepath.Glob(filepath.Join(os.Args[1], "*.mov"))
	paths = append(paths, more...)
	sort.Strings(paths)
	for _, p := range paths {
		c, err := understand.Analyze(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, p, err)
			continue
		}
		var usable float64
		for _, s := range c.Shots {
			usable += s.Length()
		}
		fmt.Printf("%-18s %4.1fs  usable %4.1fs in %d shot(s)\n", filepath.Base(p), c.Duration, usable, len(c.Shots))
		for _, s := range c.Shots {
			fmt.Printf("    %5.1f-%5.1f  score %.2f  detail %4.1f  motion %4.1f  blur %4.1f  peak %.1f\n",
				s.Start, s.End, s.Score, s.Detail, s.Motion, s.Blur, s.Peak)
		}
	}
}
