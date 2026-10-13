// Package revise applies a plain-language instruction ("make it shorter",
// "slow-mo the second shot", "add the title Summer 2026") to an existing EDL.
// The model returns a whole new EDL; it is validated and may only use clips
// that were already in the project, so a bad answer is rejected, not rendered.
package revise

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"filmeditor/internal/llm"
	"filmeditor/internal/schema"
)

const fields = `Cut fields: clip (path, must be one already in the project), start, end (source seconds),
speed (0.25-4, 1=normal), volume (0-4, 0=mute), zoom (1-4), focus_x/focus_y (0-1),
color {brightness -1..1, contrast, saturation, grayscale bool},
transition ("", "dissolve", "fade") with transition_dur seconds (into this cut from the previous),
caption (text shown during the cut).
Top level: cuts[], texts[{text,start,end (timeline seconds),position top|center|bottom,size,color}],
music {path,volume,duck,fade_out}, fade_in, fade_out (seconds), fixes (leave as is).`

const attempts = 2

// Apply returns the revised EDL.
func Apply(ctx context.Context, ai llm.Asker, edl schema.EDL, instruction string) (schema.EDL, error) {
	current, _ := json.MarshalIndent(edl, "", "  ")
	known := map[string]bool{}
	for _, c := range edl.Cuts {
		known[c.Clip] = true
	}
	feedback := ""
	for i := 0; i < attempts; i++ {
		var b strings.Builder
		b.WriteString("You are editing a video project stored as JSON. Apply the instruction and return the complete updated project.\n\n")
		b.WriteString(fields + "\n\nCurrent project:\n")
		b.Write(current)
		fmt.Fprintf(&b, "\n\nInstruction: %s\n\n", instruction)
		b.WriteString("Change only what the instruction asks for; keep everything else exactly as is.\n")
		b.WriteString("Respond with ONLY the full project JSON object, no prose, no code fences.\n")
		if feedback != "" {
			fmt.Fprintf(&b, "\nYour previous answer was rejected: %s\n", feedback)
		}
		reply, err := ai.Ask(ctx, b.String(), nil)
		if err != nil {
			return schema.EDL{}, err
		}
		next, err := parse(reply, known)
		if err == nil {
			return next, nil
		}
		feedback = err.Error()
	}
	return schema.EDL{}, fmt.Errorf("could not apply %q: %s", instruction, feedback)
}

func parse(reply string, known map[string]bool) (schema.EDL, error) {
	raw, err := llm.JSONObject(reply)
	if err != nil {
		return schema.EDL{}, err
	}
	var edl schema.EDL
	if err := json.Unmarshal([]byte(raw), &edl); err != nil {
		return schema.EDL{}, fmt.Errorf("not valid project json: %w", err)
	}
	if err := edl.Validate(); err != nil {
		return schema.EDL{}, err
	}
	for i, c := range edl.Cuts {
		if !known[c.Clip] {
			return schema.EDL{}, fmt.Errorf("cut %d uses unknown clip %q; only existing clips are allowed", i, c.Clip)
		}
	}
	return edl, nil
}
