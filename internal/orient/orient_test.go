package orient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filmeditor/internal/synth"
)

type fake map[string]int

func (f fake) Detect(_ context.Context, frames map[string]string) (map[string]int, error) {
	for id, p := range frames {
		if _, err := os.Stat(p); err != nil {
			return nil, err // frames must exist on disk for the model to read
		}
		_ = id
	}
	return f, nil
}

func TestFixMapsIDsToClipsAndDropsZeros(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	for _, p := range []string{a, b} {
		if err := synth.WriteClip(p, synth.Landscape, synth.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Fix(context.Background(), fake{"c0": 0, "c1": 180}, []string{a, b}, filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[b] != 180 {
		t.Errorf("fixes = %v, want only %s:180", got, b)
	}
}

func TestFixRejectsInvalidRotation(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	if err := synth.WriteClip(a, synth.Landscape, synth.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Fix(context.Background(), fake{"c0": 45}, []string{a}, filepath.Join(dir, "w")); err == nil {
		t.Error("expected error for 45 degrees")
	}
}
