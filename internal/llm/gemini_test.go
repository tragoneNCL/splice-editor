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

// Google retires model names. A 404 on the default should heal itself by
// finding the newest flash model the key can use.
func TestGeminiRetiredModelIsDiscoveredAndRetried(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ":generateContent"):
			tried = append(tried, r.URL.Path)
			if strings.Contains(r.URL.Path, "gemini-3.8-flash:") {
				http.Error(w, `{"error":{"code":404,"message":"This model is no longer available to new users. Please update."}}`, 404)
				return
			}
			w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Write([]byte(`{"models":[
				{"name":"models/gemini-3.8-flash","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-4.0-flash","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-4.0-flash-lite","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-4.5-pro","supportedGenerationMethods":["generateContent"]},
				{"name":"models/gemini-9.0-flash","supportedGenerationMethods":["embedContent"]}]}`))
		}
	}))
	defer srv.Close()

	discMu.Lock()
	discModel = ""
	discMu.Unlock()
	got, err := Gemini{Key: "k", BaseURL: srv.URL}.Ask(context.Background(), "hi", nil)
	if err != nil || got != "ok" {
		t.Fatalf("got %q err %v", got, err)
	}
	if len(tried) != 2 || !strings.Contains(tried[1], "gemini-4.0-flash:") {
		t.Errorf("should retry on the newest usable flash model, tried %v", tried)
	}
}

func TestGeminiErrorsAreShortAndKeyFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":400,"message":"Something odd happened. Details: `+strings.Repeat("x", 500)+`"}}`, 400)
	}))
	defer srv.Close()
	_, err := Gemini{Key: "secret-key-123", Model: "m", BaseURL: srv.URL}.Ask(context.Background(), "x", nil)
	if err == nil || len(err.Error()) > 220 || strings.Contains(err.Error(), "secret-key-123") {
		t.Errorf("err = %v", err)
	}
}
