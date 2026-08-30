package ai

import (
	"context"
	"fmt"
	"os"

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
	// ~/.gtme/secrets), NOT the process env. Reading os.Getenv here silently
	// ignored a key stored with `gtme secret set`; found by the first live
	// compose run.
	key := getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ai: ANTHROPIC_API_KEY is not set (run `gtme secret set ANTHROPIC_API_KEY`)")
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	// An identity-linked (workspace-scoped) key is refused by the Messages
	// API without its workspace header; the round-trip agent hit this live
	// (VALIDATION.md 2026-08-30). Optional credential, declared by the AI
	// manifests, injected by the runner like any other.
	if ws := getenv("ANTHROPIC_WORKSPACE_ID"); ws != "" {
		opts = append(opts, option.WithHeader("anthropic-workspace-id", ws))
	}
	// GTME_ANTHROPIC_BASE_URL points the engine at a stub (tests only).
	if base := getenv("GTME_ANTHROPIC_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	} else if base := os.Getenv("GTME_ANTHROPIC_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	return &apiEngine{client: anthropic.NewClient(opts...)}, nil
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
			anthropic.NewUserMessage(userBlocks(req)...),
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

// userBlocks renders the user turn. When the adapter exposed the
// shared/payload split (ADR-035) the two halves go as separate text blocks
// with a cache breakpoint on the shared one — every batch of a step then
// re-reads the operator's prompt from cache and pays only for its records.
func userBlocks(req Request) []anthropic.ContentBlockParamUnion {
	if req.Shared == "" || req.Payload == "" {
		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(req.Prompt)}
	}
	shared := anthropic.NewTextBlock(req.Shared)
	shared.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
	return []anthropic.ContentBlockParamUnion{shared, anthropic.NewTextBlock(req.Payload)}
}

// messageParams builds one request's Messages parameters.
func messageParams(req Request) anthropic.MessageNewParams {
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
			anthropic.NewUserMessage(userBlocks(req)...),
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	return params
}

// Submit sends every request as one Message Batch (ADR-038): half the
// per-token price, answered later under the batch id.
func (e *apiEngine) Submit(ctx context.Context, reqs []BatchRequest) (string, error) {
	items := make([]anthropic.MessageBatchNewParamsRequest, 0, len(reqs))
	for _, r := range reqs {
		p := messageParams(r.Request)
		items = append(items, anthropic.MessageBatchNewParamsRequest{
			CustomID: r.CustomID,
			Params: anthropic.MessageBatchNewParamsRequestParams{
				Model:     p.Model,
				MaxTokens: p.MaxTokens,
				Messages:  p.Messages,
				System:    p.System,
			},
		})
	}
	batch, err := e.client.Messages.Batches.New(ctx, anthropic.MessageBatchNewParams{Requests: items})
	if err != nil {
		return "", fmt.Errorf("ai: anthropic batch submit failed: %w", err)
	}
	return batch.ID, nil
}

// Collect reads a batch's results once the provider has ended it.
func (e *apiEngine) Collect(ctx context.Context, token string) (map[string]BatchResult, bool, error) {
	batch, err := e.client.Messages.Batches.Get(ctx, token)
	if err != nil {
		return nil, false, fmt.Errorf("ai: anthropic batch %s: %w", token, err)
	}
	if batch.ProcessingStatus != anthropic.MessageBatchProcessingStatusEnded {
		return nil, false, nil
	}
	stream := e.client.Messages.Batches.ResultsStreaming(ctx, token)
	out := map[string]BatchResult{}
	for stream.Next() {
		item := stream.Current()
		switch item.Result.Type {
		case "succeeded":
			msg := item.Result.Message
			var text string
			for _, block := range msg.Content {
				if t, ok := block.AsAny().(anthropic.TextBlock); ok {
					text += t.Text
				}
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
			// Batches bill at half the per-token price.
			cost, priced := Price(res.Model, res.InputTokens, res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens)
			res.CostUSD, res.Priced = cost/2, priced
			out[item.CustomID] = BatchResult{Response: res}
		case "errored":
			out[item.CustomID] = BatchResult{Err: fmt.Errorf("ai: batch request errored: %s", item.Result.Error.Error.Message)}
		default:
			out[item.CustomID] = BatchResult{Err: fmt.Errorf("ai: batch request %s", item.Result.Type)}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, false, fmt.Errorf("ai: reading batch %s results: %w", token, err)
	}
	return out, true, nil
}
