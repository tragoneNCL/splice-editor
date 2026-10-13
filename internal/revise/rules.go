package revise

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"filmeditor/internal/schema"
)

// Rules applies common plain-language edits without any AI: a small, strict
// grammar covering the things people ask for most. It is all-or-nothing: if
// any part of the instruction is not understood, nothing is changed and the
// error says what to try, so a visitor is never silently ignored.
//
// It returns the new EDL and a short summary of what changed.
func Rules(edl schema.EDL, instruction string) (schema.EDL, string, error) {
	next := edl.Clone()
	rest := strings.TrimSpace(instruction)
	var did []string

	// "black and white" contains "and"; normalise before anything splits on it.
	rest = bwRe.ReplaceAllString(rest, "grayscale")

	// Titles carry free text (which may contain commas or "and"), so they are
	// pulled out first, ending at the next recognisable command or sentence end.
	for {
		loc := titleStartRe.FindStringIndex(rest)
		if loc == nil {
			break
		}
		tail := rest[loc[1]:]
		end := len(tail)
		if m := titleEndRe.FindStringIndex(tail); m != nil {
			end = m[0]
		}
		text := strings.Trim(strings.TrimSpace(tail[:end]), "\"“”'. ")
		if text == "" {
			return edl, "", fmt.Errorf("what should the title say? e.g. add a title: Summer 2026")
		}
		next.Texts = append(next.Texts, schema.Text{Text: text, Start: 0.3, End: minF(3.3, totalLen(next)), Position: "center"})
		did = append(did, fmt.Sprintf("added the title “%s”", text))
		rest = rest[:loc[0]] + " " + tail[end:]
	}
	for {
		m := captionRe.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		text, target := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		idx, err := targets(next, target, true)
		if err != nil {
			return edl, "", err
		}
		if target == "" {
			idx = []int{len(next.Cuts) - 1}
		}
		for _, i := range idx {
			next.Cuts[i].Caption = text
		}
		did = append(did, fmt.Sprintf("captioned %s “%s”", describe(idx), text))
		rest = strings.Replace(rest, m[0], " ", 1)
	}

	shortened := false
	for _, clause := range splitRe.Split(rest, -1) {
		c := strings.ToLower(strings.TrimSpace(clause))
		c = strings.Trim(c, " .!")
		if c == "" || strings.HasPrefix(c, "keep ") || strings.HasPrefix(c, "but ") && len(c) < 5 {
			continue
		}
		if shorterRe.MatchString(c) {
			if shortened {
				continue // "shorter and punchier" is one request, not two
			}
			shortened = true
		}
		msg, err := applyClause(&next, c)
		if err != nil {
			return edl, "", err
		}
		did = append(did, msg)
	}
	if len(did) == 0 {
		return edl, "", unknown(instruction)
	}
	if err := next.Validate(); err != nil {
		return edl, "", err
	}
	return next, strings.Join(did, "; "), nil
}

var (
	titleStartRe = regexp.MustCompile(`(?i)\badd (?:a |the )?title\b\s*(?:of|:|saying|called)?\s*["“']?`)
	titleEndRe   = regexp.MustCompile(`(?i)(?:\s+and\s+|\s*;\s*|\.\s+)(?:slow|speed|mute|remove|delete|drop|make|add|zoom|fade|keep|turn|shorten|trim)`)
	bwRe         = regexp.MustCompile(`(?i)black\s*(?:and|&)\s*white`)
	captionRe    = regexp.MustCompile(`(?i)\badd (?:a )?caption\s*(?:of|:|saying)?\s*["“']([^"”']+)["”']\s*(?:(?:to|on|for)\s+(?:the\s+)?([a-z0-9 ]+?))?\s*(?:\s+and\s+|[.;]|$)`)
	shorterRe    = regexp.MustCompile(`shorter|shorten|tighten|punchier|trim it down|cut it down`)
	splitRe      = regexp.MustCompile(`\s+and\s+|\s*[,;.]\s*|\s+then\s+`)
	tgtRe        = regexp.MustCompile(`\b(first|second|third|fourth|fifth|last|final|opening|(?:shot|clip|cut)\s+(\d+))\b`)
)

var ordinal = map[string]int{"first": 0, "opening": 0, "second": 1, "third": 2, "fourth": 3, "fifth": 4}

// targets resolves "the second shot" / "clip 3" / "last" / "all" to indices.
// With none given it returns every cut, unless required is set.
func targets(e schema.EDL, s string, allowNone bool) ([]int, error) {
	n := len(e.Cuts)
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	s = strings.ToLower(s)
	if strings.TrimSpace(s) == "" || strings.Contains(s, "everything") || strings.Contains(s, "all ") || strings.Contains(s, "every") || strings.HasSuffix(s, " all") {
		if !allowNone && strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("say which shot, e.g. “the second shot”")
		}
		return all, nil
	}
	m := tgtRe.FindStringSubmatch(s)
	if m == nil {
		if strings.Contains(s, "best") {
			return []int{0}, nil // the opener is the strongest shot
		}
		return all, nil
	}
	switch word := m[1]; {
	case word == "last" || word == "final":
		return []int{n - 1}, nil
	case m[2] != "":
		k, _ := strconv.Atoi(m[2])
		if k < 1 || k > n {
			return nil, fmt.Errorf("there is no shot %d (this edit has %d)", k, n)
		}
		return []int{k - 1}, nil
	default:
		k := ordinal[word]
		if k >= n {
			return nil, fmt.Errorf("there is no %s shot (this edit has %d)", word, n)
		}
		return []int{k}, nil
	}
}

func describe(idx []int) string {
	if len(idx) == 1 {
		return fmt.Sprintf("shot %d", idx[0]+1)
	}
	return "all shots"
}

func applyClause(e *schema.EDL, c string) (string, error) {
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(c, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("slow motion", "slow-mo", "slow mo", "slowmo", "slow down", "slower"):
		idx, err := targets(*e, c, true)
		if err != nil {
			return "", err
		}
		for _, i := range idx {
			e.Cuts[i].Speed = 0.5
		}
		return "slowed " + describe(idx) + " to half speed", nil

	case has("speed up", "faster", "fast forward", "time lapse", "timelapse") && !has("faster paced", "faster-paced", "fast-paced"):
		idx, err := targets(*e, c, true)
		if err != nil {
			return "", err
		}
		for _, i := range idx {
			e.Cuts[i].Speed = 2
		}
		return "sped up " + describe(idx) + " to 2×", nil

	case has("shorter", "shorten", "tighten", "punchier", "trim it down", "cut it down"):
		for i := range e.Cuts {
			c := &e.Cuts[i]
			if keep := (c.End - c.Start) * 0.75; keep >= 0.6 {
				c.End = c.Start + keep
			}
		}
		return "trimmed every shot by 25%", nil

	case has("mute", "silence", "no sound", "remove the audio", "remove audio"):
		idx, err := targets(*e, c, true)
		if err != nil {
			return "", err
		}
		zero := 0.0
		for _, i := range idx {
			v := zero
			e.Cuts[i].Volume = &v
		}
		return "muted " + describe(idx), nil

	case has("louder", "turn up", "volume up"):
		idx, _ := targets(*e, c, true)
		for _, i := range idx {
			v := 1.5
			e.Cuts[i].Volume = &v
		}
		return "turned up " + describe(idx), nil

	case has("remove", "delete", "drop", "cut out", "get rid of"):
		idx, err := targets(*e, c, false)
		if err != nil {
			return "", err
		}
		if len(idx) >= len(e.Cuts) {
			return "", fmt.Errorf("that would remove every shot")
		}
		if len(idx) != 1 {
			return "", fmt.Errorf("say which one shot to remove, e.g. “remove the third shot”")
		}
		e.Cuts = append(e.Cuts[:idx[0]], e.Cuts[idx[0]+1:]...)
		return fmt.Sprintf("removed shot %d", idx[0]+1), nil

	case has("black and white", "black & white", "grayscale", "greyscale", "b&w", "monochrome"):
		idx, _ := targets(*e, c, true)
		for _, i := range idx {
			col := e.Cuts[i].Color
			if col == nil {
				col = &schema.Color{}
			}
			col.Grayscale = true
			e.Cuts[i].Color = col
		}
		return "made " + describe(idx) + " black and white", nil

	case has("zoom in", "punch in", "closer"):
		idx, _ := targets(*e, c, true)
		for _, i := range idx {
			e.Cuts[i].Zoom = 1.3
		}
		return "zoomed in on " + describe(idx), nil

	case has("fade out"):
		e.FadeOut = 1
		return "added a fade out", nil
	case has("fade in"):
		e.FadeIn = 1
		return "added a fade in", nil
	case has("dissolve", "crossfade", "cross-fade", "transitions", "transition"):
		for i := 1; i < len(e.Cuts); i++ {
			e.Cuts[i].Transition = "dissolve"
		}
		return "added dissolves between shots", nil
	case has("no transitions", "hard cuts"):
		for i := range e.Cuts {
			e.Cuts[i].Transition = ""
		}
		return "switched to hard cuts", nil
	}
	return "", unknown(c)
}

func unknown(s string) error {
	return fmt.Errorf("I couldn't do “%s” without AI. Without AI I can: slow-mo or speed up a shot, make it shorter, "+
		"mute a shot, remove a shot, add a title or caption, zoom in, black and white, fades and dissolves. "+
		"Connect an AI key for anything more open-ended", strings.TrimSpace(s))
}

func totalLen(e schema.EDL) float64 {
	var t float64
	for _, c := range e.Cuts {
		t += c.Duration()
	}
	return t
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
