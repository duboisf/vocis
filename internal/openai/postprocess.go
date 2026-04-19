package openai

import (
	"context"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// chatStream is the subset of ssestream.Stream methods used by PostProcess.
type chatStream interface {
	Next() bool
	Current() openaisdk.ChatCompletionChunk
	Err() error
}

// chatCompletionStreamer creates streaming chat completions.
type chatCompletionStreamer interface {
	NewStreaming(ctx context.Context, body openaisdk.ChatCompletionNewParams) chatStream
}

// sdkChatStreamer adapts the real OpenAI SDK to the chatCompletionStreamer interface.
type sdkChatStreamer struct {
	completions *openaisdk.ChatCompletionService
}

func (s *sdkChatStreamer) NewStreaming(ctx context.Context, body openaisdk.ChatCompletionNewParams) chatStream {
	return s.completions.NewStreaming(ctx, body)
}

// PostProcessResult holds the cleaned text and whether cleanup was skipped.
type PostProcessResult struct {
	Text    string
	Skipped bool
}

// WarmPostProcess fires a tiny chat completion to ensure `model` is
// resident in the backend before the real post-process request runs.
// On Lemonade this triggers the LLM-slot model swap eagerly so the
// real PP request doesn't pay the 5s+ load cost. Idempotent and cheap
// (~1 token) when the model is already loaded.
//
// Fire-and-forget: any error is logged and swallowed so warming never
// affects the dictation path.
func (c *Client) WarmPostProcess(ctx context.Context, model string) {
	if model == "" {
		return
	}
	maxTokens := int64(1)
	stream := c.chatStreamer.NewStreaming(ctx, openaisdk.ChatCompletionNewParams{
		Model: openaisdk.ChatModel(model),
		Messages: []openaisdk.ChatCompletionMessageParamUnion{
			openaisdk.UserMessage("ok"),
		},
		MaxCompletionTokens: openaisdk.Int(maxTokens),
	})
	for stream.Next() {
		_ = stream.Current()
	}
	if err := stream.Err(); err != nil {
		sessionlog.Warnf("postprocess warm %s: %v", model, err)
		return
	}
	sessionlog.Debugf("postprocess warm %s ok", model)
}

type streamResult struct {
	text string
	err  error
}

// PostProcess sends text to an LLM for cleanup using streaming with a two-phase timeout.
// onFirstToken is called (if non-nil) when the first token arrives from the model,
// allowing callers to extend visual countdowns.
func (c *Client) PostProcess(ctx context.Context, cfg config.PostProcessConfig, text string, onFirstToken func()) PostProcessResult {
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return PostProcessResult{Text: text}
	}

	if cfg.MinWordCount > 0 && len(strings.Fields(text)) < cfg.MinWordCount {
		sessionlog.Infof("postprocess skipped words=%d min=%d", len(strings.Fields(text)), cfg.MinWordCount)
		return PostProcessResult{Text: text}
	}

	totalTimeout := time.Duration(cfg.TotalTimeoutSec) * time.Second
	firstTokenTimeout := time.Duration(cfg.FirstTokenTimeoutSec) * time.Second

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("postprocess.first_token_timeout_sec", cfg.FirstTokenTimeoutSec),
		attribute.Int("postprocess.total_timeout_sec", cfg.TotalTimeoutSec),
	)

	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	prompt := cfg.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = config.DefaultPostProcessPrompt
	}

	sessionlog.Debugf("postprocess input=%q prompt=%q", text, prompt[:min(len(prompt), 80)])

	// Run streaming in a goroutine. Signal first token via channel
	// so we can enforce a tight first-token timeout without blocking.
	firstTokenCh := make(chan struct{}, 1)
	resultCh := make(chan streamResult, 1)

	start := time.Now()
	span.AddEvent("postprocess.streaming_request_sent")

	go func() {
		stream := c.chatStreamer.NewStreaming(ctx, openaisdk.ChatCompletionNewParams{
			Model: openaisdk.ChatModel(cfg.Model),
			Messages: []openaisdk.ChatCompletionMessageParamUnion{
				openaisdk.SystemMessage(prompt),
				openaisdk.UserMessage(text),
			},
		})

		var result strings.Builder
		gotFirst := false

		for stream.Next() {
			chunk := stream.Current()
			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta.Content
				if delta != "" && !gotFirst {
					gotFirst = true
					select {
					case firstTokenCh <- struct{}{}:
					default:
					}
					if onFirstToken != nil {
						onFirstToken()
					}
				}
				result.WriteString(delta)
			}
		}

		resultCh <- streamResult{text: result.String(), err: stream.Err()}
	}()

	// Wait for first token or timeout.
	select {
	case <-firstTokenCh:
		elapsed := time.Since(start).Round(time.Millisecond)
		span.AddEvent("postprocess.first_token_received",
			trace.WithAttributes(attribute.String("elapsed", elapsed.String())),
		)
		sessionlog.Debugf("postprocess: first token received after %s, waiting for completion", elapsed)
	case r := <-resultCh:
		// Completed before first token signal (very fast or empty).
		elapsed := time.Since(start).Round(time.Millisecond)
		span.AddEvent("postprocess.streaming_complete",
			trace.WithAttributes(attribute.String("elapsed", elapsed.String())),
		)
		return c.finishPostProcess(span, r, text, cfg.Model)
	case <-time.After(firstTokenTimeout):
		cancel()
		span.AddEvent("postprocess.first_token_timeout",
			trace.WithAttributes(attribute.String("timeout", firstTokenTimeout.String())),
		)
		sessionlog.Warnf("postprocess: no tokens within %s, giving up", firstTokenTimeout)
		return PostProcessResult{Text: text, Skipped: true}
	}

	// First token received — wait for full response (totalTimeout via context).
	r := <-resultCh
	elapsed := time.Since(start).Round(time.Millisecond)
	span.AddEvent("postprocess.streaming_complete",
		trace.WithAttributes(attribute.String("elapsed", elapsed.String())),
	)
	return c.finishPostProcess(span, r, text, cfg.Model)
}

func (c *Client) finishPostProcess(span trace.Span, r streamResult, rawText, model string) PostProcessResult {
	if r.err != nil {
		span.SetAttributes(attribute.String("postprocess.error", r.err.Error()))
		sessionlog.Warnf("postprocess failed, using raw transcription: %v", r.err)
		return PostProcessResult{Text: rawText, Skipped: true}
	}

	cleaned := strings.TrimSpace(r.text)
	if cleaned == "" {
		span.AddEvent("postprocess.empty_response")
		sessionlog.Warnf("postprocess returned empty text, using raw transcription")
		return PostProcessResult{Text: rawText, Skipped: true}
	}

	sessionlog.Debugf("postprocess result=%q", cleaned)
	sessionlog.Infof("postprocess cleaned=%d raw=%d model=%s", len(cleaned), len(rawText), model)
	return PostProcessResult{Text: cleaned}
}
