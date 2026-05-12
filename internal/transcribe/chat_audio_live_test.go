package transcribe

import (
	"context"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"vocis/internal/config"
)

// TestChatAudioLiveLemonade hits a real Lemonade server with a
// synthesized audio chunk to prove the wire shape end-to-end. Gated
// behind VOCIS_LIVE_LEMONADE=1 so CI / regular `go test ./...` don't
// require a running Lemonade. Run with:
//
//	VOCIS_LIVE_LEMONADE=1 go test ./internal/transcribe/ \
//	  -run TestChatAudioLiveLemonade -v
//
// The test only asserts that the request was accepted and that the
// session returned without error. The exact transcript depends on
// what Lemonade's gemma4 hears in the synthesized tone (often empty
// or the "Thank you." hallucination), so we don't pin its content.
func TestChatAudioLiveLemonade(t *testing.T) {
	if os.Getenv("VOCIS_LIVE_LEMONADE") != "1" {
		t.Skip("VOCIS_LIVE_LEMONADE not set; skipping live Lemonade integration test")
	}

	cfg := config.TranscriptionConfig{
		BaseURL: "http://localhost:13305/api/v1",
		Model:   "gemma4-it-e2b-FLM",
		Prompt: "Transcribe the following speech segment in {language}. " +
			"Only output the transcription, with no newlines.",
		Language: "en",
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 16000,
		Channels:   1,
		Samples:    samples,
	}

	// 2 seconds of a 440 Hz sine at -20 dBFS. Speech-like enough to
	// avoid Lemonade rejecting the buffer; doesn't meaningfully matter
	// what the model decodes.
	const seconds = 2
	audio := make([]int16, 16000*seconds)
	amp := int16(math.MaxInt16 / 10)
	for i := range audio {
		audio[i] = int16(float64(amp) * math.Sin(2*math.Pi*440*float64(i)/16000))
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}
	samples <- audio
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := session.Finalize(finalCtx)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	t.Logf("live transcript: %q", strings.TrimSpace(res.Text))
}

// TestChatAudioLiveLemonadeForceCutBatch sends more audio than the
// pinned chunk-max so a force-cut fires and the session ends up
// posting a multi-clip request. Verifies Lemonade accepts the shape.
// Same gating as the basic live test.
func TestChatAudioLiveLemonadeForceCutBatch(t *testing.T) {
	if os.Getenv("VOCIS_LIVE_LEMONADE") != "1" {
		t.Skip("VOCIS_LIVE_LEMONADE not set; skipping live Lemonade integration test")
	}

	cfg := config.TranscriptionConfig{
		BaseURL: "http://localhost:13305/api/v1",
		Model:   "gemma4-it-e2b-FLM",
		Prompt: "Transcribe the following speech segment in {language}. " +
			"Only output the transcription, with no newlines.",
		Language: "en",
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 16000,
		Channels:   1,
		Samples:    samples,
	}

	// Send slightly more audio than the pinned chunk-max so a
	// force-cut fires and we end up with two clips on the wire.
	const seconds = defaultChunkMaxSeconds + 2
	audio := make([]int16, 16000*seconds)
	amp := int16(math.MaxInt16 / 10)
	for i := range audio {
		audio[i] = int16(float64(amp) * math.Sin(2*math.Pi*440*float64(i)/16000))
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}
	samples <- audio
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := session.Finalize(finalCtx)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	t.Logf("force-cut batch live transcript: %q", strings.TrimSpace(res.Text))
}
