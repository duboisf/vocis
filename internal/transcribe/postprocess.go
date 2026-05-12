package transcribe

import (
	"context"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
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

// Postprocess timing and floor constants. These used to live as
// YAML knobs under `postprocess.*` (min_word_count, first_token_
// timeout_seconds, total_timeout_seconds, temperature). The values
// were never tuned in practice, so they're pinned here at the
// consumer site to keep the YAML surface small.
//
// The timeout values are vars (not consts) so postprocess_test.go
// can shrink them to keep the timeout-branch tests fast — that's the
// one place a runtime override is justified, and there's no plan to
// expose them as YAML knobs again.
const (
	// defaultMinWordCount skips the postprocess call entirely when
	// the transcript has fewer words than this — short utterances
	// (single commands, "yes", "go ahead") don't benefit from
	// LLM cleanup and pay the round-trip cost for no gain.
	defaultMinWordCount = 10
	// defaultTemperature is the sampling temperature handed to the
	// LLM for cleanup. Low enough to keep the output faithful to
	// the input transcript without re-writing the user's words.
	defaultTemperature = 0.2
)

var (
	// defaultFirstTokenTimeoutSec is how long to wait for the LLM's
	// first SSE delta before declaring the postprocess attempt
	// failed and falling back to the raw transcript. Lemonade with
	// a cold model can legitimately take 5-10 s.
	defaultFirstTokenTimeoutSec = 10
	// defaultTotalTimeoutSec is the hard cap on the postprocess
	// request. If the full response hasn't arrived by then we give
	// up and paste the raw transcript with a `postprocess_skipped`
	// warning overlay.
	defaultTotalTimeoutSec = 15
)

// chatCompletionStreamer creates streaming chat completions.
type chatCompletionStreamer interface {
	NewStreaming(ctx context.Context, body openaisdk.ChatCompletionNewParams, opts ...option.RequestOption) chatStream
}

// sdkChatStreamer adapts the real OpenAI SDK to the chatCompletionStreamer interface.
type sdkChatStreamer struct {
	completions *openaisdk.ChatCompletionService
}

func (s *sdkChatStreamer) NewStreaming(ctx context.Context, body openaisdk.ChatCompletionNewParams, opts ...option.RequestOption) chatStream {
	return s.completions.NewStreaming(ctx, body, opts...)
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

// applySamplingParams populates body with the pinned sampling
// defaults. Temperature was the only sampler knob that ever made
// it out of `postprocess.*` config in the field; top_p / penalties
// / min_p / stop got dropped during the config-surface cull.
func applySamplingParams(body *openaisdk.ChatCompletionNewParams) {
	body.Temperature = param.NewOpt(defaultTemperature)
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

	if defaultMinWordCount > 0 && len(strings.Fields(text)) < defaultMinWordCount {
		sessionlog.Infof("postprocess skipped words=%d min=%d", len(strings.Fields(text)), defaultMinWordCount)
		return PostProcessResult{Text: text}
	}

	totalTimeout := time.Duration(defaultTotalTimeoutSec) * time.Second
	firstTokenTimeout := time.Duration(defaultFirstTokenTimeoutSec) * time.Second

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("postprocess.first_token_timeout_sec", defaultFirstTokenTimeoutSec),
		attribute.Int("postprocess.total_timeout_sec", defaultTotalTimeoutSec),
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
	span.AddEvent("postprocess.input",
		trace.WithAttributes(
			attribute.Int("input.text_len", len(text)),
			attribute.String("input.text", truncate(text, 500)),
		),
	)
	span.AddEvent("postprocess.streaming_request_sent")

	go func() {
		body := openaisdk.ChatCompletionNewParams{
			Model: openaisdk.ChatModel(cfg.Model),
			Messages: []openaisdk.ChatCompletionMessageParamUnion{
				openaisdk.SystemMessage(prompt),
				openaisdk.UserMessage(text),
			},
		}
		applySamplingParams(&body)
		stream := c.chatStreamer.NewStreaming(ctx, body)

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
		return c.finishPostProcess(span, r, text, cfg.Model, elapsed)
	case <-time.After(firstTokenTimeout):
		cancel()
		elapsed := time.Since(start).Round(time.Millisecond)
		span.AddEvent("postprocess.first_token_timeout",
			trace.WithAttributes(attribute.String("timeout", firstTokenTimeout.String())),
		)
		span.AddEvent("postprocess.output",
			trace.WithAttributes(
				attribute.Bool("skipped", true),
				attribute.String("reason", "first_token_timeout"),
				attribute.Int("output.text_len", len(text)),
				attribute.String("output.text", truncate(text, 500)),
			),
		)
		sessionlog.Warnf("postprocess: no tokens within %s elapsed=%s, giving up", firstTokenTimeout, elapsed)
		return PostProcessResult{Text: text, Skipped: true}
	}

	// First token received — wait for full response (totalTimeout via context).
	r := <-resultCh
	elapsed := time.Since(start).Round(time.Millisecond)
	span.AddEvent("postprocess.streaming_complete",
		trace.WithAttributes(attribute.String("elapsed", elapsed.String())),
	)
	return c.finishPostProcess(span, r, text, cfg.Model, elapsed)
}

func (c *Client) finishPostProcess(span trace.Span, r streamResult, rawText, model string, elapsed time.Duration) PostProcessResult {
	if r.err != nil {
		span.SetAttributes(attribute.String("postprocess.error", r.err.Error()))
		span.AddEvent("postprocess.output",
			trace.WithAttributes(
				attribute.Bool("skipped", true),
				attribute.String("reason", "stream_error"),
				attribute.Int("output.text_len", len(rawText)),
				attribute.String("output.text", truncate(rawText, 500)),
			),
		)
		sessionlog.Warnf("postprocess failed elapsed=%s, using raw transcription: %v", elapsed, r.err)
		return PostProcessResult{Text: rawText, Skipped: true}
	}

	cleaned := strings.TrimSpace(r.text)
	if cleaned == "" {
		span.AddEvent("postprocess.empty_response")
		span.AddEvent("postprocess.output",
			trace.WithAttributes(
				attribute.Bool("skipped", true),
				attribute.String("reason", "empty_response"),
				attribute.Int("output.text_len", len(rawText)),
				attribute.String("output.text", truncate(rawText, 500)),
			),
		)
		sessionlog.Warnf("postprocess returned empty text elapsed=%s, using raw transcription", elapsed)
		return PostProcessResult{Text: rawText, Skipped: true}
	}

	span.AddEvent("postprocess.output",
		trace.WithAttributes(
			attribute.Bool("skipped", false),
			attribute.Int("output.text_len", len(cleaned)),
			attribute.String("output.text", truncate(cleaned, 500)),
			attribute.Int("input.text_len", len(rawText)),
		),
	)
	sessionlog.Debugf("postprocess result=%q", cleaned)
	sessionlog.Infof("postprocess cleaned=%d raw=%d model=%s elapsed=%s", len(cleaned), len(rawText), model, elapsed)
	return PostProcessResult{Text: cleaned}
}
