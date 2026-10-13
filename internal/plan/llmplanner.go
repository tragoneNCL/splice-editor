package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"filmeditor/internal/llm"
	"filmeditor/internal/schema"
)

// LLM plans with any model backend (claude CLI locally, Gemini deployed).
type LLM struct {
	AI    llm.Asker
	Label string // reported as the planner name
}

func (l LLM) Name() string {
	if l.Label != "" {
		return l.Label
	}
	return "llm"
}

func (l LLM) Plan(ctx context.Context, in Input, feedback string) (schema.EDL, error) {
	prompt, images := buildPrompt(in, feedback)
	reply, err := l.AI.Ask(ctx, prompt, images)
	if err != nil {
		return schema.EDL{}, err
	}
	return parseEDL(reply)
}

func buildPrompt(in Input, feedback string) (string, []llm.Image) {
	footage, images := summarize(in.Clips)
	var b strings.Builder
	b.WriteString("You are a professional video editor. Choose the cuts for a finished video from the footage below.\n")
	if len(images) > 0 {
		b.WriteString("Each shot lists a frame label such as f3; you are shown those frames. Look at them so your choices are based on what is actually in the footage.\n")
	}
	fmt.Fprintf(&b, "\nBrief: %s\n", orDefault(in.Prompt, "a good, well-paced edit"))
	fmt.Fprintf(&b, "Target length: %.0f seconds. Average shot length: about %.1f seconds.\n\n",
		in.Recipe.TargetDuration, in.Recipe.AvgShotLength)
	b.WriteString("Footage (shots have a quality score 0..1; higher is better):\n")
	b.WriteString(footage)
	b.WriteString("\nRules:\n")
	b.WriteString("- Follow the brief. Open with the strongest, most attention-grabbing moment that fits it.\n")
	b.WriteString("- Use only the clip paths listed, and keep start/end inside the clip's duration.\n")
	b.WriteString("- Prefer high-score shots; avoid using the same moment twice.\n")
	b.WriteString("- Total length must be close to the target.\n\n")
	b.WriteString("Respond with ONLY a JSON object, no prose, no code fences:\n")
	b.WriteString(`{"cuts":[{"clip":"<path>","start":0.0,"end":0.0}]}` + "\n")
	if feedback != "" {
		fmt.Fprintf(&b, "\nYour previous answer was rejected: %s\nFix that and answer again.\n", feedback)
	}
	return b.String(), images
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// parseEDL pulls the first JSON object out of a model reply.
func parseEDL(reply string) (schema.EDL, error) {
	raw, err := llm.JSONObject(reply)
	if err != nil {
		return schema.EDL{}, err
	}
	var edl schema.EDL
	if err := json.Unmarshal([]byte(raw), &edl); err != nil {
		return schema.EDL{}, fmt.Errorf("reply was not valid edl json: %w", err)
	}
	return edl, nil
}
