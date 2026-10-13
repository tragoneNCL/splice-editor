package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnthropicSendsKeyModelAndImages(t *testing.T) {
	var key, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("x-api-key"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"{\"c0\":0}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	img := filepath.Join(t.TempDir(), "f.jpg")
	os.WriteFile(img, []byte("jpegbytes"), 0o644)
	got, err := Anthropic{Key: "sk-test", Model: "claude-opus-5-5", BaseURL: srv.URL}.
		Ask(context.Background(), "hello", []Image{{Label: "c0", Path: img}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"c0":0}` {
		t.Errorf("reply = %q", got)
	}
	if key != "sk-test" || !strings.HasSuffix(path, "/v1/messages") {
		t.Errorf("key=%q path=%q", key, path)
	}
	raw, _ := json.Marshal(body)
	for _, want := range []string{`"claude-opus-5-5"`, `"image"`, "Image c0:", "hello"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("request missing %s: %s", want, raw)
		}
	}
}

func TestAnthropicBadKeyIsFriendlyAndLeaksNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key sk-secret"}}`))
	}))
	defer srv.Close()
	_, err := Anthropic{Key: "sk-secret", BaseURL: srv.URL}.Ask(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("err = %v, want a friendly message with no key", err)
	}
}
