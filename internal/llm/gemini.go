package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Gemini calls Google's Generative Language REST API. It has a free tier, so
// it is the default backend for a deployed, zero-cost site. The key lives on
// the backend only.
type Gemini struct {
	Key     string
	Model   string       // default DefaultGeminiModel; override with GEMINI_MODEL
	BaseURL string       // default Google's endpoint; tests point it elsewhere
	Client  *http.Client // default has a 2 minute timeout
}

// DefaultGeminiModel is a fast, vision-capable model with a free tier. Model
// names change over time, so it is overridable via GEMINI_MODEL.
const DefaultGeminiModel = "gemini-2.5-flash"

// NewGeminiFromEnv returns nil when GEMINI_API_KEY is not set.
func NewGeminiFromEnv() *Gemini {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil
	}
	return &Gemini{Key: key, Model: os.Getenv("GEMINI_MODEL")}
}

type gPart struct {
	Text       string       `json:"text,omitempty"`
	InlineData *gInlineData `json:"inline_data,omitempty"`
}

type gInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

func (g Gemini) Ask(ctx context.Context, prompt string, images []Image) (string, error) {
	model := g.Model
	if model == "" {
		model = DefaultGeminiModel
	}
	base := g.BaseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}

	parts := []gPart{{Text: prompt}}
	for _, im := range images {
		raw, err := os.ReadFile(im.Path)
		if err != nil {
			return "", err
		}
		parts = append(parts,
			gPart{Text: "Image " + im.Label + ":"},
			gPart{InlineData: &gInlineData{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(raw)}})
	}
	body, _ := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"parts": parts}},
		"generationConfig": map[string]any{"responseMimeType": "application/json", "temperature": 0.3},
	})

	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(base, "/"), url.PathEscape(model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.Key) // header, not query string, so it never lands in logs
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return "", fmt.Errorf("gemini: rate limit or free-tier quota reached, try again later")
		}
		return "", fmt.Errorf("gemini: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("gemini: bad response: %w", err)
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("gemini: no candidates in response")
	}
	var text strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	return text.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
