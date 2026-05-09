package transcribe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"vocis/internal/config"
)

// fakeChatStream implements chatStream for testing.
type fakeChatStream struct {
	mu      sync.Mutex
	chunks  []openaisdk.ChatCompletionChunk
	delays  []time.Duration // per-chunk delay; len may be shorter than chunks
	pos     int
	err     error
	ctx     context.Context
}

func (f *fakeChatStream) Next() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pos >= len(f.chunks) {
		return false
	}

	if f.pos < len(f.delays) && f.delays[f.pos] > 0 {
		delay := f.delays[f.pos]
		f.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-f.ctx.Done():
			f.mu.Lock()
			f.err = f.ctx.Err()
			return false
		}
		f.mu.Lock()
	}

	f.pos++
	return true
}

func (f *fakeChatStream) Current() openaisdk.ChatCompletionChunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pos <= 0 || f.pos > len(f.chunks) {
		return openaisdk.ChatCompletionChunk{}
	}
	return f.chunks[f.pos-1]
}

func (f *fakeChatStream) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func makeChunk(content string) openaisdk.ChatCompletionChunk {
	return openaisdk.ChatCompletionChunk{
		Choices: []openaisdk.ChatCompletionChunkChoice{
			{Delta: openaisdk.ChatCompletionChunkChoiceDelta{Content: content}},
		},
	}
}

// fakeStreamer implements chatCompletionStreamer for testing.
type fakeStreamer struct {
	stream   chatStream
	lastBody openaisdk.ChatCompletionNewParams
}

func (f *fakeStreamer) NewStreaming(_ context.Context, body openaisdk.ChatCompletionNewParams, _ ...option.RequestOption) chatStream {
	f.lastBody = body
	return f.stream
}

func newTestClient(streamer chatCompletionStreamer) *Client {
	return &Client{
		chatStreamer: streamer,
	}
}

func enabledCfg() config.PostProcessConfig {
	return config.PostProcessConfig{
		Enabled:              true,
		Model:                "test-model",
		Prompt:               "clean up",
		MinWordCount:         0,
		FirstTokenTimeoutSec: 1,
		TotalTimeoutSec:      5,
	}
}

func TestPostProcessHappyPath(t *testing.T) {
	t.Parallel()

	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{
			makeChunk("Hello"),
			makeChunk(", "),
			makeChunk("world!"),
		},
	}

	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), enabledCfg(), "uh hello um world", nil)

	if result.Text != "Hello, world!" {
		t.Fatalf("text = %q, want %q", result.Text, "Hello, world!")
	}
	if result.Skipped {
		t.Fatal("expected Skipped=false")
	}
}

func TestPostProcessFirstTokenTimeout(t *testing.T) {
	t.Parallel()

	cfg := enabledCfg()
	cfg.FirstTokenTimeoutSec = 1

	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{makeChunk("late")},
		delays: []time.Duration{3 * time.Second},
	}

	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), cfg, "some text here", nil)

	if !result.Skipped {
		t.Fatal("expected Skipped=true on first-token timeout")
	}
	if result.Text != "some text here" {
		t.Fatalf("text = %q, want raw text", result.Text)
	}
}

func TestPostProcessStreamError(t *testing.T) {
	t.Parallel()

	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: nil,
		err:    errors.New("connection reset"),
	}

	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), enabledCfg(), "some input text", nil)

	if !result.Skipped {
		t.Fatal("expected Skipped=true on stream error")
	}
	if result.Text != "some input text" {
		t.Fatalf("text = %q, want raw text", result.Text)
	}
}

func TestPostProcessEmptyResponse(t *testing.T) {
	t.Parallel()

	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{makeChunk(""), makeChunk("  ")},
	}

	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), enabledCfg(), "raw input", nil)

	if !result.Skipped {
		t.Fatal("expected Skipped=true for empty response")
	}
	if result.Text != "raw input" {
		t.Fatalf("text = %q, want raw text", result.Text)
	}
}

func TestPostProcessDisabled(t *testing.T) {
	t.Parallel()

	cfg := enabledCfg()
	cfg.Enabled = false

	client := newTestClient(nil)
	result := client.PostProcess(context.Background(), cfg, "some text", nil)

	if result.Text != "some text" {
		t.Fatalf("text = %q, want passthrough", result.Text)
	}
	if result.Skipped {
		t.Fatal("expected Skipped=false for disabled (not a skip, just passthrough)")
	}
}

func TestPostProcessEmptyInput(t *testing.T) {
	t.Parallel()

	client := newTestClient(nil)
	result := client.PostProcess(context.Background(), enabledCfg(), "   ", nil)

	if result.Text != "   " {
		t.Fatalf("text = %q, want passthrough for whitespace", result.Text)
	}
}

func TestPostProcessMinWordCount(t *testing.T) {
	t.Parallel()

	cfg := enabledCfg()
	cfg.MinWordCount = 5

	client := newTestClient(nil)
	result := client.PostProcess(context.Background(), cfg, "only three words", nil)

	if result.Text != "only three words" {
		t.Fatalf("text = %q, want passthrough", result.Text)
	}
}

func TestPostProcessFastCompletionBeforeFirstTokenSignal(t *testing.T) {
	t.Parallel()

	// Stream completes so fast the result arrives before firstTokenCh is checked.
	// This exercises the "completed before first token signal" select branch.
	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{makeChunk("cleaned")},
	}

	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), enabledCfg(), "raw text", nil)

	if result.Text != "cleaned" {
		t.Fatalf("text = %q, want %q", result.Text, "cleaned")
	}
	if result.Skipped {
		t.Fatal("expected Skipped=false")
	}
}

func TestPostProcessCallsOnFirstToken(t *testing.T) {
	t.Parallel()

	stream := &fakeChatStream{
		ctx: context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{
			makeChunk("Hello"),
			makeChunk(" world"),
		},
	}

	var called atomic.Bool
	client := newTestClient(&fakeStreamer{stream: stream})
	result := client.PostProcess(context.Background(), enabledCfg(), "raw text", func() {
		called.Store(true)
	})

	if !called.Load() {
		t.Fatal("expected onFirstToken callback to be called")
	}
	if result.Text != "Hello world" {
		t.Fatalf("text = %q, want %q", result.Text, "Hello world")
	}
}

func TestPostProcessAppliesSamplingParams(t *testing.T) {
	t.Parallel()

	temp, topP := 0.25, 0.9
	cfg := enabledCfg()
	cfg.Temperature = &temp
	cfg.TopP = &topP

	streamer := &fakeStreamer{stream: &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{makeChunk("ok")},
	}}
	client := newTestClient(streamer)
	client.PostProcess(context.Background(), cfg, "some input text", nil)

	got := streamer.lastBody
	if got.Temperature.Value != temp {
		t.Errorf("temperature = %v, want %v", got.Temperature.Value, temp)
	}
	if got.TopP.Value != topP {
		t.Errorf("top_p = %v, want %v", got.TopP.Value, topP)
	}
}

func TestPostProcessNoCallbackOnTimeout(t *testing.T) {
	t.Parallel()

	cfg := enabledCfg()
	cfg.FirstTokenTimeoutSec = 1

	stream := &fakeChatStream{
		ctx:    context.Background(),
		chunks: []openaisdk.ChatCompletionChunk{makeChunk("late")},
		delays: []time.Duration{3 * time.Second},
	}

	var called atomic.Bool
	client := newTestClient(&fakeStreamer{stream: stream})
	client.PostProcess(context.Background(), cfg, "some text here", func() {
		called.Store(true)
	})

	if called.Load() {
		t.Fatal("onFirstToken should not be called when first-token times out")
	}
}
