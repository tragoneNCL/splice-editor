package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeminiSendsImagesAndKeyHeaderAndParsesReply(t *testing.T) {
	var gotKey, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("x-goog-api-key"), r.URL.Path
		if r.URL.RawQuery != "" {
			t.Errorf("key must not be in the query string: %q", r.URL.RawQuery)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"c0\":180}"}]}}]}`))
	}))
	defer srv.Close()

	img := filepath.Join(t.TempDir(), "f.jpg")
	if err := os.WriteFile(img, []byte("jpegbytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := Gemini{Key: "k123", Model: "m1", BaseURL: srv.URL}
	reply, err := g.Ask(context.Background(), "hello", []Image{{Label: "c0", Path: img}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != `{"c0":180}` {
		t.Errorf("reply = %q", reply)
	}
	if gotKey != "k123" || !strings.HasSuffix(gotPath, "/models/m1:generateContent") {
		t.Errorf("key=%q path=%q", gotKey, gotPath)
	}
	raw, _ := json.Marshal(gotBody)
	if !strings.Contains(string(raw), "inline_data") || !strings.Contains(string(raw), "Image c0:") {
		t.Errorf("request lacks labelled image: %s", raw)
	}
}

func TestGeminiRateLimitIsFriendly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "quota", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := Gemini{Key: "k", BaseURL: srv.URL}.Ask(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("err = %v, want a quota message", err)
	}
}
