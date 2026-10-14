package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"filmeditor/internal/llm"
	"filmeditor/internal/understand"
)

const maxLabelShots = 60 // images sent per request; more would only add cost

// LabelShots shows the model every usable shot (as a frame) plus the brief, and
// asks for two things: what each shot is and how well it sells the property,
// and how the brief translates into pacing/transition/hook choices. It never
// asks for timestamps: the director owns timing, so nothing the model says can
// land on dead footage.
func LabelShots(ctx context.Context, ai llm.Asker, brief string, clips []understand.ClipSummary) (map[string]ShotLabel, Directive, error) {
	var images []llm.Image
	var b strings.Builder
	type ref struct{ key, img string }
	var refs []ref
	for ci, c := range clips {
		for si, s := range c.Shots {
			if s.Thumb == "" || len(refs) >= maxLabelShots {
				continue
			}
			img := fmt.Sprintf("f%d", len(refs))
			refs = append(refs, ref{ShotKey(ci, si), img})
			images = append(images, llm.Image{Label: img, Path: s.Thumb})
			fmt.Fprintf(&b, "- %s = image %s (%.1fs, motion %.0f)\n", ShotKey(ci, si), img, s.Length(), s.Motion)
		}
	}
	if len(refs) == 0 {
		return nil, Directive{}, fmt.Errorf("no frames to label")
	}

	var p strings.Builder
	p.WriteString("You are the creative director for a real estate listing video. Below are frames from the raw footage, one per usable shot.\n\n")
	p.WriteString("Shots:\n" + b.String() + "\n")
	fmt.Fprintf(&p, "Brief from the client (may be empty): %q\n\n", strings.TrimSpace(brief))
	p.WriteString("Do two things.\n")
	p.WriteString("1. For every shot say what it shows and how much it sells the property.\n")
	fmt.Fprintf(&p, "   room must be one of: %s.\n", strings.Join(Rooms, ", "))
	p.WriteString("   wow is 1 to 5: 5 = the shot a buyer would stop scrolling for (stunning view, striking kitchen, pool, grand entry), 1 = filler.\n")
	p.WriteString("2. Translate the brief into editing choices (use the defaults when the brief says nothing):\n")
	p.WriteString("   avg_shot: seconds per shot, 1.2 to 5 (fast/energetic ~1.6, balanced ~2.5, calm/cinematic ~3.5).\n")
	p.WriteString("   transitions: \"hard\", \"dissolve\" or \"mixed\".\n")
	p.WriteString("   energy: \"calm\", \"balanced\" or \"energetic\".\n")
	p.WriteString("   hook_room: the room kind to open on, or \"\" to let the director choose.\n\n")
	p.WriteString("Respond with ONLY this JSON, no prose:\n")
	p.WriteString(`{"shots":{"c0s0":{"room":"kitchen","wow":4}},"style":{"avg_shot":2.5,"transitions":"mixed","energy":"balanced","hook_room":""}}` + "\n")

	reply, err := ai.Ask(ctx, p.String(), images)
	if err != nil {
		return nil, Directive{}, err
	}
	raw, err := llm.JSONObject(reply)
	if err != nil {
		return nil, Directive{}, err
	}
	var parsed struct {
		Shots map[string]ShotLabel `json:"shots"`
		Style Directive            `json:"style"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, Directive{}, fmt.Errorf("label reply not valid: %w", err)
	}

	known := map[string]bool{}
	for _, r := range refs {
		known[r.key] = true
	}
	labels := map[string]ShotLabel{}
	for k, l := range parsed.Shots {
		if !known[k] {
			continue // the model invented a shot id
		}
		l.Room = strings.ToLower(strings.TrimSpace(l.Room))
		if _, ok := roomRank[l.Room]; !ok {
			l.Room = "other"
		}
		if l.Wow < 1 {
			l.Wow = 1
		}
		if l.Wow > 5 {
			l.Wow = 5
		}
		labels[k] = l
	}
	st := parsed.Style
	if st.AvgShot != 0 && (st.AvgShot < 1.2 || st.AvgShot > 5) {
		st.AvgShot = 0
	}
	switch st.Transitions {
	case "hard", "dissolve", "mixed":
	default:
		st.Transitions = ""
	}
	switch st.Energy {
	case "calm", "balanced", "energetic":
	default:
		st.Energy = ""
	}
	if _, ok := roomRank[strings.ToLower(st.HookRoom)]; !ok {
		st.HookRoom = ""
	} else {
		st.HookRoom = strings.ToLower(st.HookRoom)
	}
	return labels, st, nil
}
