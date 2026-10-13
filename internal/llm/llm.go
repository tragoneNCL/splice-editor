// Package llm abstracts the model backend. Locally that is the `claude` CLI
// (uses the developer's own login, so never on a public server); deployed it
// is the Gemini API with a key held by the backend.
package llm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Image is a frame the model should look at. Label is how the prompt refers
// to it ("f3"); Path is where it lives on disk.
type Image struct {
	Label string
	Path  string
}

// Asker sends a prompt (plus optional images) and returns the reply text.
type Asker interface {
	Ask(ctx context.Context, prompt string, images []Image) (string, error)
}

// CLI wraps the locally installed `claude` CLI in headless mode.
type CLI struct {
	Bin     string        // defaults to "claude"
	Timeout time.Duration // defaults to 4m
}

// Ask sends prompt via stdin (variadic CLI flags would swallow it as an
// argument). Images are listed in the prompt and the model reads them with
// its Read tool.
func (c CLI) Ask(ctx context.Context, prompt string, images []Image) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", "--output-format", "text"}
	if len(images) > 0 {
		var b strings.Builder
		b.WriteString(prompt)
		b.WriteString("\n\nImage files (view each with the Read tool):\n")
		dirs := map[string]bool{}
		for _, im := range images {
			fmt.Fprintf(&b, "%s: %s\n", im.Label, im.Path)
			dirs[filepath.Dir(im.Path)] = true
		}
		prompt = b.String()
		args = append(args, "--allowedTools", "Read")
		var list []string
		for d := range dirs {
			list = append(list, d)
		}
		sort.Strings(list)
		for _, d := range list {
			args = append(args, "--add-dir", d)
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude cli: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// JSONObject returns the first {...} span in a model reply.
func JSONObject(reply string) (string, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("reply contained no JSON object")
	}
	return reply[start : end+1], nil
}
