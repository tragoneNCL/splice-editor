package orient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"filmeditor/internal/llm"
	"filmeditor/internal/synth"
)

func ip(v int) *int { return &v }

// fake answers per clip index; "c<clip>f<n>" ids; nil means "can't tell".
type fake map[string]*int

func (f fake) Detect(_ context.Context, frames map[string]string) (map[string]*int, error) {
	for _, p := range frames {
		if _, err := os.Stat(p); err != nil {
			return nil, err // frames must exist on disk for the model to read
		}
	}
	return f, nil
}

func clips(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "c"+string(rune('a'+i))+".mp4")
		if err := synth.WriteClip(p, synth.Landscape, synth.Options{}); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestFixMapsFramesToClipsByMajorityAndDropsZeros(t *testing.T) {
	cs := clips(t, 2)
	got, err := Fix(context.Background(), fake{
		"c0f0": ip(0), "c0f1": ip(0), "c0f2": ip(0),
		"c1f0": ip(180), "c1f1": ip(180), "c1f2": ip(0), // majority says 180
	}, cs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[cs[1]] != 180 {
		t.Errorf("fixes = %v, want only %s:180", got, cs[1])
	}
}

func TestUndecidedClipFollowsItsGroup(t *testing.T) {
	// Three clips shot the same way; one opens on a blank wall (all null).
	cs := clips(t, 3)
	got, err := Fix(context.Background(), fake{
		"c0f0": ip(90), "c0f1": ip(90), "c0f2": nil,
		"c1f0": ip(90), "c1f1": nil, "c1f2": ip(90),
		"c2f0": nil, "c2f1": nil, "c2f2": nil, // blank wall: cannot tell
	}, cs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if got[c] != 90 {
			t.Errorf("%s fix = %d, want 90 (the group's answer)", filepath.Base(c), got[c])
		}
	}
}

func TestGroupNeverOverridesAClipsOwnEvidence(t *testing.T) {
	// Same stored size, but one clip is clearly already upright.
	cs := clips(t, 3)
	got, err := Fix(context.Background(), fake{
		"c0f0": ip(90), "c0f1": ip(90), "c0f2": ip(90),
		"c1f0": ip(0), "c1f1": ip(0), "c1f2": ip(0), // confidently upright
		"c2f0": nil, "c2f1": nil, "c2f2": nil,
	}, cs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got[cs[0]] != 90 {
		t.Errorf("clip 0 = %d, want 90", got[cs[0]])
	}
	if _, rotated := got[cs[1]]; rotated {
		t.Errorf("a clip the model was sure about must stay as is, got %d", got[cs[1]])
	}
	if _, rotated := got[cs[2]]; rotated {
		t.Errorf("one decided clip each way is no majority, so the undecided clip must not guess: got %d", got[cs[2]])
	}
}

func TestFixRejectsInvalidRotation(t *testing.T) {
	cs := clips(t, 1)
	if _, err := Fix(context.Background(), fake{"c0f0": ip(45)}, cs, t.TempDir()); err == nil {
		t.Error("expected error for 45 degrees")
	}
}

// Real listing footage: most clips from one gimbal need 90, one needs 270.
// A blank-wall clip follows the clear majority; the odd clip keeps its own answer.
func TestOneOddClipDoesNotCancelTheGroup(t *testing.T) {
	cs := clips(t, 6)
	got, err := Fix(context.Background(), fake{
		"c0f0": ip(90), "c0f1": ip(90), "c0f2": ip(90),
		"c1f0": ip(90), "c1f1": ip(90), "c1f2": nil,
		"c2f0": ip(270), "c2f1": ip(270), "c2f2": ip(270), // the bathroom shot the other way
		"c3f0": ip(90), "c3f1": nil, "c3f2": nil,
		"c4f0": ip(90), "c4f1": ip(90), "c4f2": ip(90),
		"c5f0": nil, "c5f1": nil, "c5f2": nil, // blank wall
	}, cs, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []int{90, 90, 270, 90, 90, 90}
	for i, c := range cs {
		if got[c] != want[i] {
			t.Errorf("clip %d fix = %d, want %d", i, got[c], want[i])
		}
	}
}

// The model sees the same frame four ways; its letter must map to the right
// rotation, and each letter must name the panel that really is rotated that way.
func TestPanelStripShowsEachRotation(t *testing.T) {
	cs := clips(t, 1)
	work := t.TempDir()
	src := filepath.Join(work, "f.jpg")
	// A tall frame so the four panels differ visibly.
	if err := exec.Command("ffmpeg", "-y", "-v", "error", "-i", cs[0], "-frames:v", "1", "-q:v", "3", src).Run(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "p.jpg")
	if err := makePanels(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() == 0 {
		t.Fatalf("panels not written: %v", err)
	}
	if panelDegrees["A"] != 0 || panelDegrees["B"] != 90 || panelDegrees["C"] != 180 || panelDegrees["D"] != 270 {
		t.Errorf("panelDegrees = %v", panelDegrees)
	}
}

type letterAI struct{ reply string }

func (l letterAI) Ask(_ context.Context, _ string, _ []llm.Image) (string, error) {
	return l.reply, nil
}

func TestVisionMapsPanelLettersToRotations(t *testing.T) {
	v := Vision{AI: letterAI{`{"c0f0":"B","c0f1":"d","c0f2":null,"c1f0":"A","c1f1":"Z"}`}}
	got, err := v.Detect(context.Background(), map[string]string{"c0f0": "x", "c0f1": "x", "c0f2": "x", "c1f0": "x", "c1f1": "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"c0f0": 90, "c0f1": 270, "c1f0": 0}
	for id, deg := range want {
		if got[id] == nil || *got[id] != deg {
			t.Errorf("%s = %v, want %d", id, got[id], deg)
		}
	}
	if got["c0f2"] != nil || got["c1f1"] != nil {
		t.Errorf("null and unknown letters must be 'cannot tell': %v %v", got["c0f2"], got["c1f1"])
	}
}
