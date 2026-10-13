// Package ingest probes clips deterministically. Orientation is resolved
// here, never by an AI.
package ingest

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
)

// ClipInfo describes a clip as it should be DISPLAYED (after rotation).
type ClipInfo struct {
	Path     string
	Width    int // display width
	Height   int // display height
	Rotation int // degrees clockwise a player must rotate: 0/90/180/270
	Duration float64
	HasAudio bool
}

type probeOut struct {
	Streams []struct {
		CodecType    string `json:"codec_type"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SideDataList []struct {
			Rotation *float64 `json:"rotation"`
		} `json:"side_data_list"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Probe runs ffprobe on path and returns display-oriented info.
func Probe(path string) (ClipInfo, error) {
	raw, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", path).Output()
	if err != nil {
		return ClipInfo{}, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var p probeOut
	if err := json.Unmarshal(raw, &p); err != nil {
		return ClipInfo{}, err
	}

	info := ClipInfo{Path: path}
	foundVideo := false
	for _, s := range p.Streams {
		switch s.CodecType {
		case "audio":
			info.HasAudio = true
		case "video":
			if foundVideo {
				continue
			}
			foundVideo = true
			info.Width, info.Height = s.Width, s.Height
			info.Rotation = rotationOf(s.SideDataList, s.Tags)
		}
	}
	if !foundVideo {
		return ClipInfo{}, fmt.Errorf("%s: no video stream", path)
	}
	if info.Rotation == 90 || info.Rotation == 270 {
		info.Width, info.Height = info.Height, info.Width
	}
	info.Duration, err = strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil {
		return ClipInfo{}, fmt.Errorf("%s: bad duration %q", path, p.Format.Duration)
	}
	return info, nil
}

func rotationOf(side []struct {
	Rotation *float64 `json:"rotation"`
}, tags map[string]string) int {
	for _, sd := range side {
		if sd.Rotation != nil {
			// ffprobe reports counter-clockwise degrees; convert to clockwise.
			return normalize(-*sd.Rotation)
		}
	}
	if t, ok := tags["rotate"]; ok {
		if v, err := strconv.ParseFloat(t, 64); err == nil {
			return normalize(v)
		}
	}
	return 0
}

func normalize(deg float64) int {
	d := int(math.Round(deg/90.0)) * 90
	return ((d % 360) + 360) % 360
}

// RotateFilter returns the ffmpeg filter that rotates frames clockwise by
// deg (0/90/180/270), or "" for no rotation. It is applied on top of the
// container's own rotation metadata, to correct clips whose pixels are stored
// the wrong way up with no metadata to say so.
func RotateFilter(deg int) string {
	switch ((deg % 360) + 360) % 360 {
	case 90:
		return "transpose=1"
	case 180:
		return "hflip,vflip"
	case 270:
		return "transpose=2"
	}
	return ""
}
