package plan

import (
	"math"

	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

// Repair makes any edit safe to render, whoever planned it: every cut is moved
// inside a usable shot of its own clip (so no blank walls), cuts with nothing
// usable behind them are dropped, and a cut that mostly re-shows a moment
// already used is dropped too. Transitions, captions and effects are kept.
func Repair(edl schema.EDL, clips []understand.ClipSummary) schema.EDL {
	byPath := map[string]understand.ClipSummary{}
	for _, c := range clips {
		byPath[c.Path] = c
	}
	out := edl.Clone()
	out.Cuts = out.Cuts[:0]
	type span struct{ s, e float64 }
	used := map[string][]span{}

	for _, cut := range edl.Cuts {
		c, ok := byPath[cut.Clip]
		if !ok || len(c.Shots) == 0 {
			if ok && len(c.Shots) == 0 && c.Duration > 0 {
				continue // analysed, and nothing in it is usable
			}
			out.Cuts = append(out.Cuts, cut)
			continue
		}
		want := cut.End - cut.Start
		// The shot this cut overlaps most; if none, the nearest one.
		best, bestOv, nearest, nearestD := -1, 0.0, 0, math.MaxFloat64
		for i, sh := range c.Shots {
			ov := math.Min(cut.End, sh.End) - math.Max(cut.Start, sh.Start)
			if ov > bestOv {
				best, bestOv = i, ov
			}
			d := math.Max(sh.Start-cut.End, cut.Start-sh.End)
			if d < nearestD {
				nearest, nearestD = i, d
			}
		}
		if best < 0 {
			best = nearest
		}
		sh := c.Shots[best]
		start, end := cut.Start, cut.End
		if want > sh.Length() {
			want = sh.Length()
		}
		if start < sh.Start || end > sh.End {
			start = math.Min(math.Max(start, sh.Start), sh.End-want)
			end = start + want
		}
		if end-start < 0.4 {
			continue
		}
		dup := false
		for _, u := range used[cut.Clip] {
			ov := math.Min(end, u.e) - math.Max(start, u.s)
			if ov > 0.6*(end-start) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		used[cut.Clip] = append(used[cut.Clip], span{start, end})
		cut.Start, cut.End = math.Round(start*100)/100, math.Round(end*100)/100
		out.Cuts = append(out.Cuts, cut)
	}
	if len(out.Cuts) > 0 { // a transition into the first cut means nothing
		out.Cuts[0].Transition, out.Cuts[0].TransitionDur = "", 0
	}
	return out
}
