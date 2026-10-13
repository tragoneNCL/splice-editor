// Package project holds the editable EDL with undo/redo. Every edit, whether
// from a person at a timeline, a chat command, or the AI, goes through here,
// so there is one source of truth and one history.
package project

import (
	"encoding/json"
	"fmt"
	"os"

	"filmeditor/internal/schema"
)

const maxHistory = 200

type Project struct {
	EDL  schema.EDL
	undo []schema.EDL
	redo []schema.EDL
}

func New(edl schema.EDL) *Project { return &Project{EDL: edl.Clone()} }

func Load(path string) (*Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var edl schema.EDL
	if err := json.Unmarshal(b, &edl); err != nil {
		return nil, err
	}
	return New(edl), nil
}

func (p *Project) Save(path string) error {
	b, err := json.MarshalIndent(p.EDL, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// apply runs fn on a copy; on success it becomes the new state and the old
// one is pushed to history. A failed edit leaves the project untouched.
func (p *Project) apply(fn func(e *schema.EDL) error) error {
	next := p.EDL.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	p.undo = append(p.undo, p.EDL)
	if len(p.undo) > maxHistory {
		p.undo = p.undo[1:]
	}
	p.redo = nil
	p.EDL = next
	return nil
}

func (p *Project) Undo() bool {
	if len(p.undo) == 0 {
		return false
	}
	p.redo = append(p.redo, p.EDL)
	p.EDL = p.undo[len(p.undo)-1]
	p.undo = p.undo[:len(p.undo)-1]
	return true
}

func (p *Project) Redo() bool {
	if len(p.redo) == 0 {
		return false
	}
	p.undo = append(p.undo, p.EDL)
	p.EDL = p.redo[len(p.redo)-1]
	p.redo = p.redo[:len(p.redo)-1]
	return true
}

func idx(e *schema.EDL, i int) error {
	if i < 0 || i >= len(e.Cuts) {
		return fmt.Errorf("no cut %d (have %d)", i, len(e.Cuts))
	}
	return nil
}

// Trim sets a cut's source in/out points.
func (p *Project) Trim(i int, start, end float64) error {
	return p.apply(func(e *schema.EDL) error {
		if err := idx(e, i); err != nil {
			return err
		}
		e.Cuts[i].Start, e.Cuts[i].End = start, end
		return nil
	})
}

// Split cuts cut i in two at source time at. Both halves keep the cut's
// settings; a caption and an incoming transition stay on the first half.
func (p *Project) Split(i int, at float64) error {
	return p.apply(func(e *schema.EDL) error {
		if err := idx(e, i); err != nil {
			return err
		}
		c := e.Cuts[i]
		if at <= c.Start+0.05 || at >= c.End-0.05 {
			return fmt.Errorf("split point %.2f must be inside the cut (%.2f-%.2f)", at, c.Start, c.End)
		}
		first, second := c, c
		first.End = at
		second.Start = at
		second.Caption, second.Transition, second.TransitionDur = "", "", 0
		cuts := append([]schema.Cut{}, e.Cuts[:i]...)
		cuts = append(cuts, first, second)
		e.Cuts = append(cuts, e.Cuts[i+1:]...)
		return nil
	})
}

func (p *Project) Delete(i int) error {
	return p.apply(func(e *schema.EDL) error {
		if err := idx(e, i); err != nil {
			return err
		}
		if len(e.Cuts) == 1 {
			return fmt.Errorf("cannot delete the only cut")
		}
		e.Cuts = append(e.Cuts[:i], e.Cuts[i+1:]...)
		return nil
	})
}

// Move reorders: the cut at from ends up at index to.
func (p *Project) Move(from, to int) error {
	return p.apply(func(e *schema.EDL) error {
		if err := idx(e, from); err != nil {
			return err
		}
		if err := idx(e, to); err != nil {
			return err
		}
		c := e.Cuts[from]
		rest := append(append([]schema.Cut{}, e.Cuts[:from]...), e.Cuts[from+1:]...)
		cuts := append([]schema.Cut{}, rest[:to]...)
		cuts = append(cuts, c)
		e.Cuts = append(cuts, rest[to:]...)
		return nil
	})
}

// Update applies fn to cut i (speed, volume, zoom, colour, caption,
// transition...).
func (p *Project) Update(i int, fn func(c *schema.Cut)) error {
	return p.apply(func(e *schema.EDL) error {
		if err := idx(e, i); err != nil {
			return err
		}
		fn(&e.Cuts[i])
		return nil
	})
}

func (p *Project) AddText(t schema.Text) error {
	return p.apply(func(e *schema.EDL) error { e.Texts = append(e.Texts, t); return nil })
}

func (p *Project) SetMusic(m *schema.Music) error {
	return p.apply(func(e *schema.EDL) error { e.Music = m; return nil })
}

// Replace swaps in a whole new EDL (e.g. a revision from the AI) as one
// undoable step.
func (p *Project) Replace(edl schema.EDL) error {
	return p.apply(func(e *schema.EDL) error { *e = edl.Clone(); return nil })
}

// Duration is the programme length on the timeline, ignoring clip probing:
// cut durations after speed, minus overlap from transitions.
func (p *Project) Duration() float64 {
	var total float64
	for i, c := range p.EDL.Cuts {
		total += c.Duration()
		if i > 0 && c.Transition != "" {
			t := c.TransitionDur
			if t <= 0 {
				t = 0.5
			}
			total -= t
		}
	}
	return total
}

// RemoveText deletes the title at index i.
func (p *Project) RemoveText(i int) error {
	return p.apply(func(e *schema.EDL) error {
		if i < 0 || i >= len(e.Texts) {
			return fmt.Errorf("no title %d (have %d)", i, len(e.Texts))
		}
		e.Texts = append(e.Texts[:i], e.Texts[i+1:]...)
		return nil
	})
}

// SetFades sets the programme fade in/out (nil leaves a value unchanged).
func (p *Project) SetFades(in, out *float64) error {
	return p.apply(func(e *schema.EDL) error {
		if in != nil {
			if *in < 0 || *in > 10 {
				return fmt.Errorf("fade in must be between 0 and 10 seconds")
			}
			e.FadeIn = *in
		}
		if out != nil {
			if *out < 0 || *out > 10 {
				return fmt.Errorf("fade out must be between 0 and 10 seconds")
			}
			e.FadeOut = *out
		}
		return nil
	})
}
