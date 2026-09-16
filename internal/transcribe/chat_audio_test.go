package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vocis/internal/config"
)

func TestParseSSEDeltaContentString(t *testing.T) {
	t.Parallel()
	payload := `{"choices":[{"delta":{"content":"Hello"},"finish_reason":""}]}`
	delta, finish, err := parseSSEDelta(payload)
	if err != nil {
		t.Fatalf("parseSSEDelta: %v", err)
	}
	if delta != "Hello" {
		t.Fatalf("delta=%q want %q", delta, "Hello")
	}
	if finish != "" {
		t.Fatalf("finish=%q want empty", finish)
	}
}

func TestParseSSEDeltaContentParts(t *testing.T) {
	t.Parallel()
	payload := `{"choices":[{"delta":{"content":[{"type":"text","text":"foo "},{"type":"text","text":"bar"}]}}]}`
	delta, _, err := parseSSEDelta(payload)
	if err != nil {
		t.Fatalf("parseSSEDelta: %v", err)
	}
	if delta != "foo bar" {
		t.Fatalf("delta=%q want %q", delta, "foo bar")
	}
}

func TestParseSSEDeltaFinishReason(t *testing.T) {
	t.Parallel()
	payload := `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	delta, finish, err := parseSSEDelta(payload)
	if err != nil {
		t.Fatalf("parseSSEDelta: %v", err)
	}
	if delta != "" {
		t.Fatalf("delta=%q want empty", delta)
	}
	if finish != "stop" {
		t.Fatalf("finish=%q want stop", finish)
	}
}

func TestReadSSEAccumulatesAndStopsOnDone(t *testing.T) {
	t.Parallel()
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n" +
		"data: [DONE]\n\n"
	s := &chatAudioSession{events: make(chan DictationEvent, 16)}
	got, err := s.readSSE(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if got != "Hello world" {
		t.Fatalf("text=%q want %q", got, "Hello world")
	}
	// Two partials (cumulative) should have been emitted.
	close(s.events)
	var partials []string
	for ev := range s.events {
		if ev.Type == DictationEventPartial {
			partials = append(partials, ev.Text)
		}
	}
	if len(partials) != 2 || partials[0] != "Hello " || partials[1] != "Hello world" {
		t.Fatalf("partials=%v", partials)
	}
}

func TestBuildMessagesAppendsPromptHint(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe in {language}.",
		language:       "en",
		promptHint:     "Also clean up filler words.",
	}
	msgs := s.buildMessages([][]byte{[]byte("wav")})
	if msgs[0]["role"] != "system" {
		t.Fatalf("msg[0].role=%v", msgs[0]["role"])
	}
	got := msgs[0]["content"].(string)
	if !strings.Contains(got, "Transcribe in en.") {
		t.Fatalf("system prompt missing transcribe text: %q", got)
	}
	if !strings.Contains(got, "Also clean up filler words.") {
		t.Fatalf("system prompt missing prompt hint: %q", got)
	}
}

func TestBuildMessagesSingleClipIsSystemPlusOneUserTurn(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{promptTemplate: "Transcribe.", language: "en"}
	msgs := s.buildMessages([][]byte{[]byte("now")})
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("msgs=%v want [system, user]", msgs)
	}
	parts := msgs[1]["content"].([]map[string]any)
	if len(parts) != 1 || parts[0]["type"] != "input_audio" {
		t.Fatalf("user content=%v want one input_audio part", parts)
	}
}

func TestBuildChatCompletionsURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"appends path", "http://localhost:13305/api/v1", "http://localhost:13305/api/v1/chat/completions", false},
		{"strips trailing slash", "http://localhost:13305/api/v1/", "http://localhost:13305/api/v1/chat/completions", false},
		{"keeps existing path", "http://localhost:13305/api/v1/chat/completions", "http://localhost:13305/api/v1/chat/completions", false},
		{"empty", "", "", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildChatCompletionsURL(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestRedactedRequestJSONStripsAudio(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"model":  "gemma-test",
		"stream": true,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Transcribe the speech."},
					{
						"type": "input_audio",
						"input_audio": map[string]any{
							"data":   base64.StdEncoding.EncodeToString([]byte("fake-wav-bytes")),
							"format": "wav",
						},
					},
				},
			},
		},
	}
	out, err := redactedRequestJSON(body)
	if err != nil {
		t.Fatalf("redactedRequestJSON: %v", err)
	}
	if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte("fake-wav-bytes"))) {
		t.Fatalf("redacted output still contains base64 data: %s", out)
	}
	// JSON encodes < as <, so check for the JSON-escaped form.
	if !strings.Contains(out, `<wav`) || !strings.Contains(out, "base64=") {
		t.Fatalf("redacted output missing placeholder: %s", out)
	}
	if !strings.Contains(out, "Transcribe the speech.") {
		t.Fatalf("redacted output dropped the prompt text: %s", out)
	}
	if !strings.Contains(out, `"model": "gemma-test"`) {
		t.Fatalf("redacted output dropped the model field: %s", out)
	}
}

func TestEncodePCM16WAVHeader(t *testing.T) {
	t.Parallel()
	samples := []int16{0, 1, -1, 2}
	wav := encodePCM16WAV(samples, 16000)
	if string(wav[0:4]) != "RIFF" {
		t.Fatalf("RIFF marker missing")
	}
	if string(wav[8:12]) != "WAVE" {
		t.Fatalf("WAVE marker missing")
	}
	if string(wav[12:16]) != "fmt " {
		t.Fatalf("fmt  marker missing")
	}
	if string(wav[36:40]) != "data" {
		t.Fatalf("data marker missing")
	}
	if len(wav) != 44+len(samples)*2 {
		t.Fatalf("len=%d want %d", len(wav), 44+len(samples)*2)
	}
}

// TestChatAudioSessionMultiClipForceCutBatching verifies that two
// force-cut clips accumulate into a SINGLE /chat/completions POST
// with two input_audio parts, instead of two separate POSTs. The
// system message must carry the multi-clip framing that asks gemma
// to transcribe all clips as one continuous text, and history must
// be skipped (the audio itself is the cross-clip context).
func TestChatAudioSessionMultiClipForceCutBatching(t *testing.T) {
	type seenRequest struct {
		body []byte
	}
	var (
		mu      sync.Mutex
		seen    []seenRequest
		replies = []string{"hello world"}
		callIdx int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{body: body})
		idx := callIdx
		callIdx++
		mu.Unlock()
		if idx >= len(replies) {
			http.Error(w, "out of replies", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", replies[idx])
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:  server.URL + "/api/v1",
		Model:    "gemma-test",
		Prompt:   "Transcribe in {language}.",
		Language: "en",
	}
	samples := make(chan []int16, 2)
	opts := DictationOpts{
		SampleRate: 8000, // deliberately != sileroSampleRate so VAD is skipped (Silero unused)
		Channels:   1,
		Samples:    samples,
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}

	// Push two force-cut chunks. The chunk cap is pinned at
	// defaultChunkMaxSeconds*sampleRate = 28*8000 = 224000 samples.
	// Send that twice so each push lands as its own force-cut clip.
	chunk := make([]int16, defaultChunkMaxSeconds*opts.SampleRate)
	samples <- chunk
	samples <- chunk
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.Finalize(finalCtx)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(res.Text, "hello world") {
		t.Fatalf("joined text=%q want \"hello world\"", res.Text)
	}

	mu.Lock()
	calls := len(seen)
	body := seen[0].body
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls=%d want 1 (force-cuts must batch into a single multi-clip POST)", calls)
	}

	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Expect: system message with multi-clip framing + one user
	// message with TWO input_audio parts (one per force-cut clip).
	if len(req.Messages) != 2 {
		t.Fatalf("messages=%d want 2 (system + user)", len(req.Messages))
	}
	if req.Messages[0]["role"] != "system" {
		t.Fatalf("messages[0].role=%v want system", req.Messages[0]["role"])
	}
	sys := req.Messages[0]["content"].(string)
	if !strings.Contains(sys, "ALL clips") {
		t.Fatalf("system message missing multi-clip framing: %q", sys)
	}
	if req.Messages[1]["role"] != "user" {
		t.Fatalf("messages[1].role=%v want user", req.Messages[1]["role"])
	}
	parts := req.Messages[1]["content"].([]any)
	audioCount := 0
	for _, p := range parts {
		m := p.(map[string]any)
		if m["type"] == "input_audio" {
			audioCount++
		}
	}
	if audioCount != 2 {
		t.Fatalf("user content audio parts=%d want 2", audioCount)
	}
	// No assistant turn anywhere: nothing prior is ever re-sent.
	for _, m := range req.Messages {
		if m["role"] == "assistant" {
			t.Fatalf("multi-clip request unexpectedly has assistant turn: %v", m)
		}
	}
}

// TestChatAudioSessionWritesAudioCaptureWAV verifies that the chat-
// audio session natively mirrors each POSTed chunk to disk under
// $XDG_STATE_HOME/vocis/audio/ — the audio_capture writer is opened
// by startChatAudioSession itself from cfg.AudioCapture, no external
// plumbing needed.
func TestChatAudioSessionWritesAudioCaptureWAV(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi.\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:  server.URL + "/api/v1",
		Model:    "gemma-test",
		Prompt:   "Transcribe in {language}.",
		Language: "en",
		AudioCapture: config.AudioCaptureConfig{
			Enabled:           true,
			TTLSeconds:        3600,
			GCIntervalSeconds: 600,
		},
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 8000, // != sileroSampleRate so VAD is skipped
		Channels:   1,
		Samples:    samples,
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}

	samples <- make([]int16, opts.SampleRate) // 1s of audio
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := session.Finalize(finalCtx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	audioDir := filepath.Join(tmp, "vocis", "audio")
	entries, err := os.ReadDir(audioDir)
	if err != nil {
		t.Fatalf("read audio dir %s: %v", audioDir, err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("audio dir entries=%d want 1; got %v", len(entries), names)
	}
	name := entries[0].Name()
	if !strings.Contains(name, "release") {
		t.Errorf("filename %q should encode the release reason", name)
	}
	if !strings.HasSuffix(name, ".wav") {
		t.Errorf("filename %q should end in .wav", name)
	}
	data, err := os.ReadFile(filepath.Join(audioDir, name))
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if len(data) < 44 || string(data[:4]) != "RIFF" {
		t.Errorf("file %s is not a valid WAV (len=%d head=%q)", name, len(data), data[:min(8, len(data))])
	}
}

// TestChatAudioSessionDropsSilentChunk verifies the energy gate
// prevents a chunk of all-zero PCM from ever reaching the model. This
// is the defense against Gemma hallucinating a long "I cannot
// transcribe..." response when there's no real speech in the buffer
// (the typical case when Silero VAD isn't installed and a hold
// captures pure silence).
func TestChatAudioSessionDropsSilentChunk(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not have been called", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:      server.URL,
		Model:        "gemma-test",
		Prompt:       "Transcribe.",
		Language:     "en",
		MinChunkPeak: 0.02,
		MinChunkRMS:  0.005,
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 8000,
		Channels:   1,
		Samples:    samples,
	}
	session, err := startChatAudioSession(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Pure zeros — peak=0, rms=0, both below the gate.
	samples <- make([]int16, 80000)
	close(samples)
	res, err := session.Finalize(context.Background())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if called {
		t.Fatalf("server was called for an all-silence chunk; energy gate failed")
	}
	if strings.TrimSpace(res.Text) != "" {
		t.Fatalf("text=%q want empty (silence dropped)", res.Text)
	}
}

// TestChatAudioSessionDropsHallucination wires a server that returns
// a stock Whisper-hallucination phrase and verifies the filter drops
// it on the trailing path.
func TestChatAudioSessionDropsHallucination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Thank you.\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:              server.URL,
		Model:                "gemma-test",
		HallucinationFilters: []string{"Thank you."},
		Prompt:               "Transcribe.",
		Language:             "en",
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 8000,
		Channels:   1,
		Samples:    samples,
	}
	session, err := startChatAudioSession(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	samples <- make([]int16, 80000)
	close(samples)
	res, err := session.Finalize(context.Background())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if strings.TrimSpace(res.Text) != "" {
		t.Fatalf("text=%q want empty (hallucination dropped)", res.Text)
	}
}

// TestEnergyGateTrustsVADOverRMS reproduces the "pause, resume, release"
// loss: after a VAD pause the buffer keeps filling with room silence
// until the user speaks again, so the trailing clip is mostly silence
// with a short quiet phrase at the end. Its RMS averages below
// min_chunk_rms even though Silero saw speech, and the whole phrase was
// dropped as "silent". When VAD vouched for speech the RMS arm must not
// apply; the peak arm still catches true silence.
func TestEnergyGateTrustsVADOverRMS(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{minChunkPeak: 0.02, minChunkRMS: 0.005}
	const rate = 16000
	clip := make([]int16, 10*rate)
	for i := 0; i < rate/5; i++ { // 200 ms of quiet speech at the tail
		clip[len(clip)-1-i] = 983 // 0.03 of full scale
	}
	if _, _, ok := s.passEnergyGate(clip, false); ok {
		t.Fatal("without VAD the diluted clip must fail the RMS arm")
	}
	if _, _, ok := s.passEnergyGate(clip, true); !ok {
		t.Fatal("with VAD speech the diluted clip must pass")
	}
	if _, _, ok := s.passEnergyGate(make([]int16, rate), true); ok {
		t.Fatal("pure silence must still fail on peak even when VAD vouched")
	}
}
