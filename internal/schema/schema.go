// Package schema holds the data models shared by every pipeline stage.
// The EDL is the project file: the AI planner writes it, the renderer plays
// it, and a person (UI) or a chat command edits the very same document.
package schema

import (
	"errors"
	"fmt"
)

type Canvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	FPS    int `json:"fps"`
}

// Presets are the standard export aspect ratios.
var Presets = map[string]Canvas{
	"9:16": {Width: 1080, Height: 1920, FPS: 30}, // Reels, TikTok, Shorts
	"1:1":  {Width: 1080, Height: 1080, FPS: 30},
	"4:5":  {Width: 1080, Height: 1350, FPS: 30},
	"16:9": {Width: 1920, Height: 1080, FPS: 30},
}

// Recipe is the style spec, produced from a prompt and/or an inspiration video.
type Recipe struct {
	Canvas         Canvas  `json:"canvas"`
	TargetDuration float64 `json:"target_duration"`
	AvgShotLength  float64 `json:"avg_shot_length"`
	Captions       bool    `json:"captions"`
	// Fit is how clips are placed on the canvas: "contain" (letterbox,
	// default) or "cover" (fill and crop: the usual way to reframe landscape
	// footage into a vertical video).
	Fit string `json:"fit,omitempty"`
	// CRF is x264 quality, lower is better (default 20).
	CRF int `json:"crf,omitempty"`
	// Loudnorm normalises the final mix to streaming loudness.
	Loudnorm bool `json:"loudnorm,omitempty"`
}

// DefaultRecipe is a 9:16 social short.
func DefaultRecipe() Recipe {
	return Recipe{
		Canvas:         Presets["9:16"],
		TargetDuration: 30,
		AvgShotLength:  2.5,
		Fit:            "cover",
		Loudnorm:       true,
	}
}

// Color is a basic grade. Zero values mean "unchanged".
type Color struct {
	Brightness float64 `json:"brightness,omitempty"` // -1..1, 0 = unchanged
	Contrast   float64 `json:"contrast,omitempty"`   // 0 = unchanged (1.0)
	Saturation float64 `json:"saturation,omitempty"` // 0 = unchanged (1.0); use a tiny value for grayscale
	Grayscale  bool    `json:"grayscale,omitempty"`
}

// Cut is one segment of a source clip, in seconds of source time.
type Cut struct {
	Clip  string  `json:"clip"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`

	Speed  float64  `json:"speed,omitempty"`   // 0 = 1.0; 0.25..4
	Volume *float64 `json:"volume,omitempty"`  // nil = 1.0; 0 mutes
	Zoom   float64  `json:"zoom,omitempty"`    // 0 = 1.0; >1 punches in
	FocusX float64  `json:"focus_x,omitempty"` // 0..1, 0 = centre (0.5)
	FocusY float64  `json:"focus_y,omitempty"`
	Color  *Color   `json:"color,omitempty"`

	// Transition into this cut from the previous one: "" (hard cut),
	// "dissolve" or "fade" (through black). TransitionDur defaults to 0.5s.
	// On the first cut, any transition means fade-in from black.
	Transition    string  `json:"transition,omitempty"`
	TransitionDur float64 `json:"transition_dur,omitempty"`

	// Caption is shown for the duration of this cut, bottom of frame.
	Caption string `json:"caption,omitempty"`
}

// Text is a title or free-placed overlay on the finished timeline.
type Text struct {
	Text     string  `json:"text"`
	Start    float64 `json:"start"` // timeline seconds
	End      float64 `json:"end"`
	Position string  `json:"position,omitempty"` // "top", "center", "bottom" (default)
	Size     int     `json:"size,omitempty"`     // px height; default canvas height/18
	Color    string  `json:"color,omitempty"`    // "white" (default) or "#rrggbb"
}

// Music is a background track mixed under the programme audio.
type Music struct {
	Path    string  `json:"path"`
	Volume  float64 `json:"volume,omitempty"` // 0 = default 0.3
	Duck    bool    `json:"duck,omitempty"`   // lower under speech / programme audio
	FadeOut float64 `json:"fade_out,omitempty"`
}

// EDL is the edit: an ordered list of cuts plus timeline-level elements.
type EDL struct {
	Cuts []Cut `json:"cuts"`
	// Fixes maps a clip path to extra clockwise degrees (90/180/270) that
	// correct pixels stored the wrong way up with no rotation metadata.
	Fixes   map[string]int `json:"fixes,omitempty"`
	Texts   []Text         `json:"texts,omitempty"`
	Music   *Music         `json:"music,omitempty"`
	FadeIn  float64        `json:"fade_in,omitempty"`  // seconds from black
	FadeOut float64        `json:"fade_out,omitempty"` // seconds to black
}

// Clone returns a deep copy, so history snapshots never alias live state.
func (e EDL) Clone() EDL {
	c := e
	c.Cuts = make([]Cut, len(e.Cuts))
	for i, cut := range e.Cuts {
		if cut.Volume != nil {
			v := *cut.Volume
			cut.Volume = &v
		}
		if cut.Color != nil {
			col := *cut.Color
			cut.Color = &col
		}
		c.Cuts[i] = cut
	}
	if e.Fixes != nil {
		c.Fixes = make(map[string]int, len(e.Fixes))
		for k, v := range e.Fixes {
			c.Fixes[k] = v
		}
	}
	c.Texts = append([]Text(nil), e.Texts...)
	if e.Music != nil {
		m := *e.Music
		c.Music = &m
	}
	return c
}

// SpeedOrDefault returns the cut's speed, defaulting to 1.
func (c Cut) SpeedOrDefault() float64 {
	if c.Speed <= 0 {
		return 1
	}
	return c.Speed
}

// Duration is the cut's length on the timeline (after speed), in seconds.
func (c Cut) Duration() float64 { return (c.End - c.Start) / c.SpeedOrDefault() }

// Validate checks structural sanity. Range checks against real clip
// durations happen where the durations are known (plan.Check, render).
func (e EDL) Validate() error {
	if len(e.Cuts) == 0 {
		return errors.New("edl has no cuts")
	}
	for i, c := range e.Cuts {
		if c.Clip == "" {
			return fmt.Errorf("cut %d: missing clip", i)
		}
		if c.Start < 0 || c.End <= c.Start {
			return fmt.Errorf("cut %d: invalid range %.2f-%.2f", i, c.Start, c.End)
		}
		if c.Speed != 0 && (c.Speed < 0.25 || c.Speed > 4) {
			return fmt.Errorf("cut %d: speed %.2f outside 0.25..4", i, c.Speed)
		}
		if c.Volume != nil && (*c.Volume < 0 || *c.Volume > 4) {
			return fmt.Errorf("cut %d: volume %.2f outside 0..4", i, *c.Volume)
		}
		if c.Zoom != 0 && (c.Zoom < 1 || c.Zoom > 4) {
			return fmt.Errorf("cut %d: zoom %.2f outside 1..4", i, c.Zoom)
		}
		switch c.Transition {
		case "", "dissolve", "fade":
		default:
			return fmt.Errorf("cut %d: unknown transition %q", i, c.Transition)
		}
	}
	for i, t := range e.Texts {
		if t.Text == "" || t.End <= t.Start || t.Start < 0 {
			return fmt.Errorf("text %d: needs text and a valid start/end", i)
		}
	}
	return nil
}
