package ai

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// apiEngine talks to the Anthropic Messages API.
type apiEngine struct {
	client anthropic.Client
}

func newAPIEngine(getenv func(string) string) (Engine, error) {
	// The key comes through the caller's env view — for a built-in adapter
	// that is the runner-injected session env (SPEC §6: OS env first, then
	// ~/.gtm/secrets), NOT the process env. Reading os.Getenv here silently
	// ignored a key stored with `gtm secret set`; found by the first live
	// compose run.
	key := getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ai: ANTHROPIC_API_KEY is not set (run `gtm secret set ANTHROPIC_API_KEY`)")
	}
	return &apiEngine{client: anthropic.NewClient(option.WithAPIKey(key))}, nil
}

func (e *apiEngine) Name() string { return EngineAPI }

// Complete sends one message and returns its text.
func (e *apiEngine) Complete(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	msg, err := e.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, fmt.Errorf("ai: anthropic request failed: %w", err)
	}

	var text string
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text += t.Text
		}
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		return Response{}, fmt.Errorf("ai: the model declined this request (%s)", msg.StopDetails.Category)
	}

	res := Response{
		Text:             text,
		Model:            string(msg.Model),
		Engine:           EngineAPI,
		InputTokens:      int(msg.Usage.InputTokens),
		OutputTokens:     int(msg.Usage.OutputTokens),
		CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
		CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
	}
	res.CostUSD, res.Priced = Price(res.Model, res.InputTokens, res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens)

	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return res, fmt.Errorf("ai: response hit max_tokens (%d) before finishing; raise max_tokens or lower batch_size", maxTokens)
	}
	return res, nil
}
