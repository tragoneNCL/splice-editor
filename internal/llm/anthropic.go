package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultClaudeModel is used unless ANTHROPIC_MODEL or Model overrides it.
const DefaultClaudeModel = "claude-opus-5-5"

// Anthropic calls the Claude API with the official SDK. The key is the
// caller's own (bring-your-own-key); it is never logged or stored.
type Anthropic struct {
	Key     string
	Model   string
	BaseURL string // tests point this at a fake server
}

func (a Anthropic) Ask(ctx context.Context, prompt string, images []Image) (string, error) {
	model := a.Model
	if model == "" {
		model = os.Getenv("ANTHROPIC_MODEL")
	}
	if model == "" {
		model = DefaultClaudeModel
	}
	opts := []option.RequestOption{option.WithAPIKey(a.Key)}
	if a.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(a.BaseURL))
	}
	client := anthropic.NewClient(opts...)

	var blocks []anthropic.ContentBlockParamUnion
	for _, im := range images {
		raw, err := os.ReadFile(im.Path)
		if err != nil {
			return "", err
		}
		blocks = append(blocks,
			anthropic.NewTextBlock("Image "+im.Label+":"),
			anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(raw)))
	}
	blocks = append(blocks, anthropic.NewTextBlock(prompt))

	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 8000,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
	})
	if err != nil {
		return "", friendly(err)
	}
	var out strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("claude: empty reply (stop reason %s)", resp.StopReason)
	}
	return out.String(), nil
}

// friendly turns API errors into messages safe to show a visitor (no key,
// no request body).
func friendly(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		switch apierr.StatusCode {
		case 401:
			return fmt.Errorf("claude: that API key was rejected")
		case 403:
			return fmt.Errorf("claude: this key is not allowed to use that model")
		case 429:
			return fmt.Errorf("claude: rate limit reached, try again shortly")
		case 402:
			return fmt.Errorf("claude: the account is out of credit")
		}
		return fmt.Errorf("claude: request failed (http %d)", apierr.StatusCode)
	}
	return fmt.Errorf("claude: %w", err)
}
