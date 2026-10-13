package server

import (
	"fmt"
	"net/http"
	"strings"

	"filmeditor/internal/project"
	"filmeditor/internal/schema"
)

// Manual timeline edits. A client batches any number of operations (trim,
// split, delete, reorder, per-cut settings, titles) and sends them in one
// request; they are applied together, validated, and rendered once as a new,
// undoable revision.

type editOp struct {
	Op string `json:"op"`
	// Cut index (trim, split, delete, set) or move source.
	I    int `json:"i"`
	From int `json:"from"`
	To   int `json:"to"`

	Start *float64 `json:"start,omitempty"` // trim in-point; also text start
	End   *float64 `json:"end,omitempty"`   // trim out-point; also text end
	At    *float64 `json:"at,omitempty"`    // split point, in source seconds

	// "set": only the fields that are present change.
	Speed         *float64 `json:"speed,omitempty"`
	Volume        *float64 `json:"volume,omitempty"`
	Zoom          *float64 `json:"zoom,omitempty"`
	Caption       *string  `json:"caption,omitempty"`
	Transition    *string  `json:"transition,omitempty"`
	TransitionDur *float64 `json:"transition_dur,omitempty"`
	Grayscale     *bool    `json:"grayscale,omitempty"`
	Brightness    *float64 `json:"brightness,omitempty"`
	Contrast      *float64 `json:"contrast,omitempty"`
	Saturation    *float64 `json:"saturation,omitempty"`

	// "add_text" / "remove_text" / "fades"
	Text     string   `json:"text,omitempty"`
	Position string   `json:"position,omitempty"`
	FadeIn   *float64 `json:"fade_in,omitempty"`
	FadeOut  *float64 `json:"fade_out,omitempty"`
}

const maxEditOps = 200

// applyOps runs ops against edl, in order, and returns the result. Any
// failing operation rejects the whole batch (nothing is half-applied).
func applyOps(edl schema.EDL, ops []editOp) (schema.EDL, error) {
	if len(ops) == 0 {
		return edl, fmt.Errorf("no changes to apply")
	}
	if len(ops) > maxEditOps {
		return edl, fmt.Errorf("too many changes at once (max %d)", maxEditOps)
	}
	p := project.New(edl)
	for n, o := range ops {
		var err error
		switch strings.ToLower(o.Op) {
		case "trim":
			if o.Start == nil || o.End == nil {
				err = fmt.Errorf("trim needs start and end")
				break
			}
			err = p.Trim(o.I, *o.Start, *o.End)
		case "split":
			if o.At == nil {
				err = fmt.Errorf("split needs a point")
				break
			}
			err = p.Split(o.I, *o.At)
		case "delete":
			err = p.Delete(o.I)
		case "move":
			err = p.Move(o.From, o.To)
		case "set":
			err = p.Update(o.I, func(c *schema.Cut) { applySet(c, o) })
		case "add_text":
			if o.Start == nil || o.End == nil {
				err = fmt.Errorf("a title needs a start and end time")
				break
			}
			err = p.AddText(schema.Text{Text: strings.TrimSpace(o.Text), Start: *o.Start, End: *o.End, Position: o.Position})
		case "remove_text":
			err = p.RemoveText(o.I)
		case "fades":
			err = p.SetFades(o.FadeIn, o.FadeOut)
		default:
			err = fmt.Errorf("unknown change %q", o.Op)
		}
		if err != nil {
			return edl, fmt.Errorf("change %d (%s): %w", n+1, o.Op, err)
		}
	}
	return p.EDL, nil
}

func applySet(c *schema.Cut, o editOp) {
	if o.Speed != nil {
		c.Speed = *o.Speed
		if c.Speed == 1 {
			c.Speed = 0
		}
	}
	if o.Volume != nil {
		v := *o.Volume
		if v == 1 {
			c.Volume = nil
		} else {
			c.Volume = &v
		}
	}
	if o.Zoom != nil {
		c.Zoom = *o.Zoom
		if c.Zoom == 1 {
			c.Zoom = 0
		}
	}
	if o.Caption != nil {
		c.Caption = strings.TrimSpace(*o.Caption)
	}
	if o.Transition != nil {
		c.Transition = *o.Transition
	}
	if o.TransitionDur != nil {
		c.TransitionDur = *o.TransitionDur
	}
	if o.Grayscale != nil || o.Brightness != nil || o.Contrast != nil || o.Saturation != nil {
		col := schema.Color{}
		if c.Color != nil {
			col = *c.Color
		}
		if o.Grayscale != nil {
			col.Grayscale = *o.Grayscale
		}
		if o.Brightness != nil {
			col.Brightness = *o.Brightness
		}
		if o.Contrast != nil {
			col.Contrast = *o.Contrast
		}
		if o.Saturation != nil {
			col.Saturation = *o.Saturation
		}
		if col == (schema.Color{}) {
			c.Color = nil
		} else {
			c.Color = &col
		}
	}
}

func (s *Server) editProject(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	var body struct {
		Ops []editOp `json:"ops"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {\"ops\": [...]} as JSON")
		return
	}

	var base *schema.EDL
	var busy bool
	j.set(func(j *job) {
		if j.status != "done" || j.edl == nil {
			busy = true
			return
		}
		e := j.edl.Clone()
		base = &e
	})
	if busy {
		fail(w, http.StatusConflict, "wait for the current render to finish")
		return
	}
	// Validate synchronously so a bad batch is reported at once, not after a render.
	next, err := applyOps(*base, body.Ops)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkSourceRanges(j, next); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	j.set(func(j *job) { j.status, j.stage, j.progress, j.errMsg = "running", "Applying your edits", 0.1, "" })
	task := func() {
		if err := renderRevision(j, next, base); err != nil {
			j.set(func(j *job) {
				j.status, j.stage, j.errMsg = "done", "Done", "could not apply those edits: "+err.Error()
			})
			return
		}
		j.set(func(j *job) {
			j.parent = j.rev
			j.edl, j.rev = &next, j.rev+1
			j.status, j.stage, j.progress, j.errMsg = "done", "Done", 1, ""
		})
		s.saveRevision(j, "manual edit")
		s.persist(j)
	}
	select {
	case s.queue <- task:
		writeJSON(w, http.StatusAccepted, map[string]string{"id": j.id})
	default:
		j.set(func(j *job) { j.status, j.stage = "done", "Done" })
		fail(w, http.StatusServiceUnavailable, "the editor is busy, try again in a few minutes")
	}
}

// checkSourceRanges makes sure no cut points past the end of its source clip,
// using the library's recorded durations (cheap, no probing).
func (s *Server) checkSourceRanges(j *job, e schema.EDL) error {
	if s.store == nil {
		return nil
	}
	owner := j.visitor
	if owner == "" {
		owner = "anon"
	}
	byPath := map[string]float64{}
	j.mu.Lock()
	ids := append([]string(nil), j.mediaIDs...)
	j.mu.Unlock()
	for _, id := range ids {
		if m, err := s.store.GetMedia(owner, id); err == nil {
			byPath[m.Path] = m.Duration
		}
	}
	for i, c := range e.Cuts {
		if d, ok := byPath[c.Clip]; ok && d > 0 && c.End > d+0.05 {
			return fmt.Errorf("cut %d ends at %.1fs but its clip is only %.1fs long", i+1, c.End, d)
		}
	}
	return nil
}
