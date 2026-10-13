package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

	// Retries for transient failures (overload, rate limit); default 3 tries
	// with Backoff doubling between them (default 1s).
	Tries   int
	Backoff time.Duration
}

// DefaultGeminiModel is a fast, vision-capable model with a free tier. Model
// names change over time, so it is overridable via GEMINI_MODEL.
const DefaultGeminiModel = "gemini-3.8-flash"

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

// discovered caches a working model found via ListModels, so a retired
// default heals itself instead of breaking every request.
var (
	discMu    sync.Mutex
	discModel string
	discAt    time.Time
)

func (g Gemini) Ask(ctx context.Context, prompt string, images []Image) (string, error) {
	model := g.Model
	if model == "" {
		discMu.Lock()
		if discModel != "" && time.Since(discAt) < time.Hour {
			model = discModel
		}
		discMu.Unlock()
	}
	if model == "" {
		model = DefaultGeminiModel
	}
	out, status, err := g.generateRetrying(ctx, model, prompt, images)
	if err == nil {
		return out, nil
	}
	if g.Model != "" {
		return "", err // an explicitly chosen model: do not second-guess it
	}
	switch {
	case status == http.StatusNotFound:
		// A retired or unknown name: use the newest flash model this key can use.
		if found, derr := g.discover(ctx, flashVersion); derr == nil && found != "" && found != model {
			discMu.Lock()
			discModel, discAt = found, time.Now()
			discMu.Unlock()
			out, _, err2 := g.generateRetrying(ctx, found, prompt, images)
			return out, err2
		}
	case errors.Is(err, ErrBusy):
		// Overloaded: a lighter model on separate capacity often still answers.
		if lite, derr := g.discover(ctx, flashLiteVersion); derr == nil && lite != "" && lite != model {
			out, _, err2 := g.generateRetrying(ctx, lite, prompt, images)
			if err2 == nil {
				return out, nil
			}
		}
	}
	return "", err
}

func transient(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// generateRetrying retries transient failures with exponential backoff.
func (g Gemini) generateRetrying(ctx context.Context, model, prompt string, images []Image) (string, int, error) {
	tries, wait := g.Tries, g.Backoff
	if tries <= 0 {
		tries = 3
	}
	if wait <= 0 {
		wait = time.Second
	}
	for i := 1; ; i++ {
		out, status, err := g.generate(ctx, model, prompt, images)
		if err == nil || !transient(status) || i >= tries {
			return out, status, err
		}
		select {
		case <-ctx.Done():
			return "", status, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (g Gemini) base() string {
	if g.BaseURL != "" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	return "https://generativelanguage.googleapis.com"
}

func (g Gemini) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

var (
	flashVersion     = regexp.MustCompile(`^models/gemini-(\d+(?:\.\d+)?)-flash$`)
	flashLiteVersion = regexp.MustCompile(`^models/gemini-(\d+(?:\.\d+)?)-flash-lite$`)
)

// discover returns the newest model matching pattern that supports generateContent.
func (g Gemini) discover(ctx context.Context, pattern *regexp.Regexp) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base()+"/v1beta/models?pageSize=200", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-goog-api-key", g.Key)
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list models: http %d", resp.StatusCode)
	}
	var list struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", err
	}
	best, bestV := "", -1.0
	for _, m := range list.Models {
		sub := pattern.FindStringSubmatch(m.Name)
		if sub == nil || !contains(m.Methods, "generateContent") {
			continue
		}
		if v, err := strconv.ParseFloat(sub[1], 64); err == nil && v > bestV {
			best, bestV = strings.TrimPrefix(m.Name, "models/"), v
		}
	}
	if best == "" {
		return "", fmt.Errorf("no usable Gemini flash model found for this key")
	}
	return best, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// generate makes one generateContent call. It returns the HTTP status so the
// caller can decide whether discovery is worth trying.
func (g Gemini) generate(ctx context.Context, model, prompt string, images []Image) (string, int, error) {
	parts := []gPart{{Text: prompt}}
	for _, im := range images {
		raw, err := os.ReadFile(im.Path)
		if err != nil {
			return "", 0, err
		}
		parts = append(parts,
			gPart{Text: "Image " + im.Label + ":"},
			gPart{InlineData: &gInlineData{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(raw)}})
	}
	body, _ := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"parts": parts}},
		"generationConfig": map[string]any{"responseMimeType": "application/json", "temperature": 0.3},
	})

	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.base(), url.PathEscape(model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.Key) // header, not query string, so it never lands in logs
	resp, err := g.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("gemini: could not reach Google (%v)", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, geminiError(resp.StatusCode, raw)
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
		return "", resp.StatusCode, fmt.Errorf("gemini: bad response: %w", err)
	}
	if len(out.Candidates) == 0 {
		return "", resp.StatusCode, fmt.Errorf("gemini: no candidates in response")
	}
	var text strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	return text.String(), resp.StatusCode, nil
}

// geminiError turns an API failure into one short sentence safe to show a
// visitor (Google's JSON body can be long and technical).
func geminiError(status int, raw []byte) error {
	switch status {
	case http.StatusTooManyRequests:
		return fmt.Errorf("gemini: %w (rate limit or free-tier quota; try again shortly)", ErrBusy)
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("gemini: %w (Google's servers are overloaded)", ErrBusy)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("gemini: that API key was rejected")
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(raw, &e) == nil {
		msg = strings.TrimSpace(e.Error.Message)
	}
	if i := strings.IndexAny(msg, ".\n"); i > 0 {
		msg = msg[:i]
	}
	if msg == "" {
		msg = "request failed"
	}
	return fmt.Errorf("gemini: %s (http %d)", truncate(msg, 160), status)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
