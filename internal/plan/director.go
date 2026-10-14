package plan

import (
	"context"
	"math"
	"sort"
	"strings"

	"filmeditor/internal/schema"
	"filmeditor/internal/understand"
)

// Director is the editing brain. Given analysed footage (usable shots only, with
// duplicates grouped) and optional AI labels, it builds the edit by rules a
// good editor follows: open on the strongest shot, then tour the property in a
// sensible order, never repeat a shot, vary the pace, and choose each cut's
// window at the shot's best moment. It is deterministic and cannot pick dead
// footage, so the AI's role is to *describe* the footage, not to pick times.
type Director struct {
	Labels map[string]ShotLabel // keyed by ShotKey(clipIndex, shotIndex)
	Style  Directive
}

// ShotLabel is what the AI says about one shot.
type ShotLabel struct {
	Room string  `json:"room"` // one of Rooms
	Wow  float64 `json:"wow"`  // 1..5, how well it sells the property
	Note string  `json:"note,omitempty"`
}

// Directive is the AI's reading of the brief, reduced to knobs the director
// understands. Zero values mean "use the defaults".
type Directive struct {
	AvgShot     float64 `json:"avg_shot"`    // seconds; 0 = recipe default
	Transitions string  `json:"transitions"` // "hard", "dissolve" or "mixed"
	HookRoom    string  `json:"hook_room"`   // open on this kind of shot if present
	Energy      string  `json:"energy"`      // "calm", "balanced" or "energetic"
}

// Rooms is the vocabulary shots are labelled with, in typical tour order.
var Rooms = []string{
	"exterior", "entry", "living", "dining", "kitchen", "hallway",
	"bedroom", "bathroom", "office", "outdoor", "view", "detail", "other",
}

var roomRank = func() map[string]int {
	m := map[string]int{}
	for i, r := range Rooms {
		m[r] = i
	}
	return m
}()

// ShotKey identifies a shot within the analysed clips.
func ShotKey(clip, shot int) string { return "c" + itoa(clip) + "s" + itoa(shot) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// slot is one planned cut: a shot and which window of it (long shots can
// supply more than one, well apart).
type slot struct {
	c  *cand
	wi int
}

type cand struct {
	clipIdx, shotIdx int
	clip             string
	shot             understand.Shot
	room             string
	wow              float64
	key              string
	// use bookkeeping
	windows []window
}

type window struct{ start, end float64 }

func (c *cand) value() float64 {
	w := c.wow
	if w == 0 {
		w = 3
	}
	return c.shot.Score*2 + w*0.35 // score is 0..1, wow 1..5
}

func (d Director) Name() string {
	if len(d.Labels) > 0 {
		return "ai-director"
	}
	return "director"
}

// Plan satisfies the Planner interface.
func (d Director) Plan(_ context.Context, in Input, _ string) (schema.EDL, error) {
	return d.build(in)
}

func (d Director) build(in Input) (schema.EDL, error) {
	// 1. Candidates: every usable shot, labelled.
	var all []*cand
	for ci, c := range in.Clips {
		for si, s := range c.Shots {
			lab := d.Labels[ShotKey(ci, si)]
			room := strings.ToLower(lab.Room)
			if _, ok := roomRank[room]; !ok {
				room = "other"
			}
			all = append(all, &cand{clipIdx: ci, shotIdx: si, clip: c.Path, shot: s, room: room, wow: lab.Wow, key: ShotKey(ci, si)})
		}
	}
	if len(all) == 0 {
		return schema.EDL{}, errNoShots
	}

	target := in.Recipe.TargetDuration
	avg := d.Style.AvgShot
	if avg <= 0 {
		avg = in.Recipe.AvgShotLength
	}
	if avg <= 0 {
		avg = 2.5
	}
	switch d.Style.Energy {
	case "energetic":
		avg *= 0.8
	case "calm":
		avg *= 1.25
	}
	avg = math.Min(math.Max(avg, 1.2), 5)

	// 2. De-duplicate: one shot per look-alike group, the best one. Duplicates
	// are held back as a reserve in case the footage is too short without them.
	// Look-alikes are the same view twice: the worse takes are dropped, never
	// used to pad the length.
	var primary, reserve []*cand
	bestOfGroup := map[int]*cand{}
	for _, c := range all {
		g := c.shot.Group
		if g == 0 {
			primary = append(primary, c)
			continue
		}
		if cur, ok := bestOfGroup[g]; !ok || c.value() > cur.value() {
			bestOfGroup[g] = c
		}
	}
	for _, c := range bestOfGroup {
		primary = append(primary, c)
	}
	sort.SliceStable(primary, func(i, j int) bool {
		return primary[i].clipIdx*1000+primary[i].shotIdx < primary[j].clipIdx*1000+primary[j].shotIdx
	})

	// A tour that lingers on one room feels padded: at most maxPerRoom shots of
	// any one kind of room (when labels say what they are); the rest wait in
	// reserve and are only used if the footage would otherwise run short.
	if len(d.Labels) > 0 {
		sort.SliceStable(primary, func(i, j int) bool { return primary[i].value() > primary[j].value() })
		perRoom := map[string]int{}
		var kept []*cand
		for _, c := range primary {
			if c.room != "other" && perRoom[c.room] >= maxPerRoom {
				reserve = append(reserve, c)
				continue
			}
			perRoom[c.room]++
			kept = append(kept, c)
		}
		primary = kept
		sort.SliceStable(primary, func(i, j int) bool {
			return primary[i].clipIdx*1000+primary[i].shotIdx < primary[j].clipIdx*1000+primary[j].shotIdx
		})
	}

	// 3. How many cuts, and which shots. Prefer fewer, held-longer shots to
	// padding: extras (a third shot of one kind of room, then a second window
	// of a long take) join only if even maximum holds cannot fill the length.
	wantCuts := int(math.Round(target / avg))
	if wantCuts < 1 {
		wantCuts = 1
	}
	chosen := append([]*cand(nil), primary...)
	byOrder := func(cs []*cand) {
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].clipIdx*1000+cs[i].shotIdx < cs[j].clipIdx*1000+cs[j].shotIdx })
	}
	if len(chosen) > wantCuts { // too much footage: keep the strongest
		sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].value() > chosen[j].value() })
		chosen = chosen[:wantCuts]
	}
	byOrder(chosen)

	capacity := func(cs []*cand) float64 {
		var c float64
		for _, x := range cs {
			c += math.Min(maxWindow, x.shot.Length())
		}
		return c
	}
	var slots []slot
	for _, c := range chosen {
		slots = append(slots, slot{c, 0})
	}
	need := target * 0.92
	if cap := capacity(chosen); cap < need {
		// 1st: shots held back by the per-room limit (different views).
		sort.SliceStable(reserve, func(i, j int) bool { return reserve[i].value() > reserve[j].value() })
		for _, c := range reserve {
			if cap >= need {
				break
			}
			slots = append(slots, slot{c, 0})
			cap += math.Min(maxWindow, c.shot.Length())
		}
		// 2nd: a second window from a long take, away from the first.
		if cap < need {
			longest := append([]*cand(nil), chosen...)
			sort.SliceStable(longest, func(i, j int) bool { return longest[i].shot.Length() > longest[j].shot.Length() })
			for _, c := range longest {
				if cap >= need {
					break
				}
				if c.shot.Length() >= longShot {
					slots = append(slots, slot{c, 1})
					cap += math.Min(maxWindow, c.shot.Length()/2)
				}
			}
		}
	}

	// 4. Order: hook first, then the tour.
	hookIdx := pickHook(slots2cands(slots), d.Style.HookRoom)
	hook := slots[hookIdx]
	rest := append(append([]slot(nil), slots[:hookIdx]...), slots[hookIdx+1:]...)
	sort.SliceStable(rest, func(i, j int) bool {
		a, b := rest[i], rest[j]
		ra, rb := roomRank[a.c.room], roomRank[b.c.room]
		if ra != rb {
			return ra < rb
		}
		if a.c.clipIdx != b.c.clipIdx {
			return a.c.clipIdx < b.c.clipIdx
		}
		if a.c.shotIdx != b.c.shotIdx {
			return a.c.shotIdx < b.c.shotIdx
		}
		return a.wi < b.wi
	})
	ordered := append([]slot{hook}, rest...)
	n := len(ordered)

	// 5. Transitions decided before timing, so the overlap can be paid back.
	trans := make([]string, n)
	var overlap float64
	for i := 1; i < n; i++ {
		prev, cur := ordered[i-1].c, ordered[i].c
		if useDissolve(d.Style.Transitions, prev, cur) {
			trans[i] = "dissolve"
			overlap += dissolveSeconds
		}
	}

	// 6. Pacing: a curve over the edit, nudged by how fast each shot moves,
	// scaled so the finished length (after transition overlap) hits the target.
	dur := pacing(ordered, avg, target+overlap)

	// 7. Windows at each shot's best moment.
	var edl schema.EDL
	usedWin := map[string][]window{}
	for i, s := range ordered {
		w := pickWindow(s.c, dur[i], usedWin[s.c.key])
		if w.end-w.start < 0.4 {
			continue
		}
		usedWin[s.c.key] = append(usedWin[s.c.key], w)
		cut := schema.Cut{Clip: s.c.clip, Start: round2(w.start), End: round2(w.end)}
		if trans[i] != "" && len(edl.Cuts) > 0 {
			cut.Transition, cut.TransitionDur = trans[i], dissolveSeconds
		}
		edl.Cuts = append(edl.Cuts, cut)
	}
	if len(edl.Cuts) == 0 {
		return schema.EDL{}, errNoShots
	}
	return edl, nil
}

const (
	minWindow       = 0.9
	maxWindow       = 5.0
	dissolveSeconds = 0.4
)

var errNoShots = errString("no usable footage to edit")

type errString string

func (e errString) Error() string { return string(e) }

func slots2cands(s []slot) []*cand {
	out := make([]*cand, len(s))
	for i, v := range s {
		out[i] = v.c
	}
	return out
}

// pickHook chooses the opening shot: the requested room if present, else the
// shot that sells the property best. Very short shots and bare walls don't open.
func pickHook(cs []*cand, wantRoom string) int {
	best, bestV := 0, -1.0
	for i, c := range cs {
		v := c.value()
		if c.shot.Length() < 1.5 {
			v -= 1
		}
		if wantRoom != "" && c.room == wantRoom {
			v += 3
		}
		switch c.room {
		case "exterior", "outdoor", "view": // classic openers
			v += 0.6
		}
		if v > bestV {
			best, bestV = i, v
		}
	}
	return best
}

func useDissolve(mode string, prev, cur *cand) bool {
	switch mode {
	case "hard":
		return false
	case "dissolve":
		return prev.clip != cur.clip
	}
	// "mixed" (default): dissolve between different rooms when both shots are
	// slow enough to read as a calm move; hard cut when the camera is whipping.
	if prev.clip == cur.clip {
		return false
	}
	if prev.room == cur.room && prev.room != "other" { // "other" means unknown, not "same"
		return false
	}
	return prev.shot.Motion < 18 && cur.shot.Motion < 18
}

// pacing returns a duration per cut summing to total. The shape: a slightly
// longer hook, a brisk and varied middle, a held finish. Faster camera moves
// get shorter cuts (they read quickly), slow ones a little longer.
func pacing(ordered []slot, avg, total float64) []float64 {
	n := len(ordered)
	w := make([]float64, n)
	var motions []float64
	for _, s := range ordered {
		motions = append(motions, s.c.shot.Motion)
	}
	med := median(motions)
	for i := range ordered {
		p := 0.5
		if n > 1 {
			p = float64(i) / float64(n-1)
		}
		f := 1.0
		switch {
		case i == 0:
			f = 1.25 // let the hook land
		case p < 0.2:
			f = 1.1 // settle in
		case p > 0.85:
			f = 1.3 // hold the finish
		default:
			f = 1.0 - 0.15*math.Sin(p*math.Pi) // quicken toward the middle
		}
		if med > 0 {
			m := ordered[i].c.shot.Motion / med // >1 = faster than typical
			f *= math.Min(1.15, math.Max(0.8, 1.1-0.25*(m-1)))
		}
		w[i] = avg * f
	}
	// Scale to the target length, respecting each shot's own length and limits.
	d := make([]float64, n)
	for pass := 0; pass < 4; pass++ {
		var sum, free float64
		for i := range w {
			sum += w[i]
		}
		scale := total / math.Max(sum, 0.1)
		free = 0
		for i := range w {
			v := w[i] * scale
			maxLen := math.Min(maxWindow, ordered[i].c.shot.Length())
			if ordered[i].wi > 0 || windowsOf(ordered, i) > 1 {
				maxLen = math.Min(maxLen, ordered[i].c.shot.Length()/2-0.1)
			}
			v = math.Min(math.Max(v, minWindow), math.Max(maxLen, minWindow))
			d[i] = v
			free += v
		}
		for i := range w {
			w[i] = d[i]
		}
		if math.Abs(free-total) < 0.15 {
			break
		}
	}
	return d
}

// windowsOf counts how many cuts draw from the same shot as cut i.
func windowsOf(ordered []slot, i int) int {
	n := 0
	for _, s := range ordered {
		if s.c.key == ordered[i].c.key {
			n++
		}
	}
	return n
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[len(c)/2]
}

// pickWindow chooses a window of length d inside the shot, centred on its best
// moment, away from windows already used from the same shot.
func pickWindow(c *cand, d float64, used []window) window {
	lo, hi := c.shot.Start, c.shot.End
	if d > hi-lo {
		d = hi - lo
	}
	center := c.shot.Peak
	if len(used) > 0 {
		// A second window: the end of the shot farthest from the first.
		first := used[0]
		if first.start-lo > hi-first.end {
			center = lo + (first.start-lo)/2
		} else {
			center = first.end + (hi-first.end)/2
		}
	}
	start := center - d/2
	if start < lo {
		start = lo
	}
	if start+d > hi {
		start = hi - d
	}
	w := window{start: start, end: start + d}
	// Keep clear of any earlier window from this shot.
	for _, u := range used {
		if w.start < u.end && w.end > u.start {
			if u.start-lo >= d {
				w = window{start: u.start - d, end: u.start}
			} else if hi-u.end >= d {
				w = window{start: u.end, end: u.end + d}
			} else {
				return window{} // no room: caller drops it
			}
		}
	}
	return w
}

const (
	maxPerRoom = 2   // a tour should not linger on one kind of room
	longShot   = 6.0 // only shots this long can supply a second window
)
