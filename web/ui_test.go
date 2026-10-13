package web

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"testing"
)

// Every element id the script touches must exist in the page. A missing one
// once froze every upload at 0% (the null deref happened outside a handler).
func TestEveryScriptElementExistsInThePage(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bid="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(string(html), -1) {
		have[m[1]] = true
	}
	var missing []string
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\("([A-Za-z0-9_-]+)"\)`),
		regexp.MustCompile(`getElementById\("([A-Za-z0-9_-]+)"\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(js), -1) {
			if !have[m[1]] {
				missing = append(missing, m[1])
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js uses ids that index.html does not define: %v", missing)
	}
}

// Runs the real script end to end against a fake server (needs Node).
func TestUploadFlowRunsEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "ui_smoke_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log(string(out))
}
