package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// claudeCodeEngine shells out to the claude CLI (SPEC §2). It exists so an
// operator who already has Claude Code authenticated does not need a separate
// API key.
type claudeCodeEngine struct {
	bin string
}

func newClaudeCodeEngine() (Engine, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("ai: engine claude-code needs the `claude` binary on PATH: %w", err)
	}
	return &claudeCodeEngine{bin: bin}, nil
}

func (e *claudeCodeEngine) Name() string { return EngineClaudeCode }

// claudeCodeResult is the subset of `claude -p --output-format json` we read.
type claudeCodeResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Model        string  `json:"model"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Complete runs one non-interactive claude invocation.
func (e *claudeCodeEngine) Complete(ctx context.Context, req Request) (Response, error) {
	args := []string{"-p", "--output-format", "json"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := exec.CommandContext(ctx, e.bin, args...)

	prompt := req.Prompt
	if req.System != "" {
		// The CLI has no separate system channel here; put the contract first so it
		// still frames the request.
		prompt = req.System + "\n\n" + req.Prompt
	}
	cmd.Stdin = strings.NewReader(prompt)

	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Response{}, fmt.Errorf("ai: claude-code failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}

	return parseClaudeCodeOutput(out.Bytes(), req.Model)
}

// parseClaudeCodeOutput reads `claude -p --output-format json`. A response
// carrying total_cost_usd is measured (ADR-046: vendor-reported cost
// metadata); one without it is priced from our table, an estimate.
func parseClaudeCodeOutput(raw []byte, reqModel string) (Response, error) {
	var parsed claudeCodeResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Older or differently-configured CLIs may print bare text.
		return Response{Text: string(raw), Engine: EngineClaudeCode, Model: reqModel}, nil
	}
	if parsed.IsError {
		return Response{}, fmt.Errorf("ai: claude-code reported an error: %s", parsed.Result)
	}

	model := parsed.Model
	if model == "" {
		model = reqModel
	}
	res := Response{
		Text:             parsed.Result,
		Model:            model,
		Engine:           EngineClaudeCode,
		InputTokens:      parsed.Usage.InputTokens,
		OutputTokens:     parsed.Usage.OutputTokens,
		CacheReadTokens:  parsed.Usage.CacheReadInputTokens,
		CacheWriteTokens: parsed.Usage.CacheCreationInputTokens,
	}
	if parsed.TotalCostUSD > 0 {
		// The CLI reports what it actually cost; trust it over our own table.
		res.CostUSD, res.Priced, res.Measured = parsed.TotalCostUSD, true, true
	} else {
		res.CostUSD, res.Priced = Price(res.Model, res.InputTokens, res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens)
	}
	return res, nil
}
