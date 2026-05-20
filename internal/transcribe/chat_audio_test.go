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
	s := &chatAudioSession{
		streamSSE: true,
		events:    make(chan DictationEvent, 16),
	}
	s.liveSegments.Store(true)
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

func TestBuildMessagesIncludesHistoryThenCurrent(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe in {language}.",
		language:       "en",
		historyTurns:   2,
	}
	s.history = []chatTurn{
		{wav: []byte("wav1"), transcript: "first"},
		{wav: []byte("wav2"), transcript: "second"},
	}
	current := []byte("wavCurrent")
	msgs := s.buildMessages([][]byte{current})
	// system + 2*(user/assistant) + final user = 6
	if len(msgs) != 6 {
		t.Fatalf("len=%d want 6 (system + 2 user/assistant pairs + 1 user)", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "Transcribe in en." {
		t.Fatalf("msg[0]=%v want system with rendered prompt", msgs[0])
	}
	if msgs[1]["role"] != "user" {
		t.Fatalf("msg[1].role=%v want user", msgs[1]["role"])
	}
	if msgs[2]["role"] != "assistant" || msgs[2]["content"] != "first" {
		t.Fatalf("msg[2]=%v", msgs[2])
	}
	if msgs[3]["role"] != "user" {
		t.Fatalf("msg[3].role=%v want user", msgs[3]["role"])
	}
	if msgs[4]["role"] != "assistant" || msgs[4]["content"] != "second" {
		t.Fatalf("msg[4]=%v", msgs[4])
	}
	if msgs[5]["role"] != "user" {
		t.Fatalf("msg[5].role=%v want user", msgs[5]["role"])
	}

	// Final user content is just an audio part — no text prompt
	// (the prompt now lives in the system message).
	parts, ok := msgs[5]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("user content type=%T", msgs[5]["content"])
	}
	if len(parts) != 1 {
		t.Fatalf("user content parts=%d want 1 (audio only)", len(parts))
	}
	if parts[0]["type"] != "input_audio" {
		t.Fatalf("audio part type=%v", parts[0]["type"])
	}
	audio := parts[0]["input_audio"].(map[string]any)
	if audio["format"] != "wav" {
		t.Fatalf("format=%v", audio["format"])
	}
	decoded, err := base64.StdEncoding.DecodeString(audio["data"].(string))
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if string(decoded) != "wavCurrent" {
		t.Fatalf("audio bytes=%q", decoded)
	}
}

func TestBuildMessagesAppendsExtraSystemPrompt(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate:    "Transcribe in {language}.",
		language:          "en",
		historyTurns:      0,
		extraSystemPrompt: "Also clean up filler words.",
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
		t.Fatalf("system prompt missing extra prompt: %q", got)
	}
}

func TestBuildMessagesRespectsHistoryTurnsCap(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe.",
		language:       "en",
		historyTurns:   1, // only most recent prior turn allowed
	}
	s.history = []chatTurn{
		{wav: []byte("a"), transcript: "alpha"},
		{wav: []byte("b"), transcript: "bravo"},
		{wav: []byte("c"), transcript: "charlie"},
	}
	msgs := s.buildMessages([][]byte{[]byte("now")})
	// system + user (charlie audio) + assistant "charlie" + user (now audio) = 4
	if len(msgs) != 4 {
		t.Fatalf("len=%d want 4", len(msgs))
	}
	if msgs[2]["content"] != "charlie" {
		t.Fatalf("kept turn=%v want charlie", msgs[2]["content"])
	}
}

func TestBuildMessagesInlineClipsSplitsSystemAndUser(t *testing.T) {
	t.Parallel()
	// inline_clips is no longer a YAML knob but the code path is
	// still exercised here in case we ever resurrect it as a
	// build-time override.
	s := &chatAudioSession{
		promptTemplate: "Transcribe in {language}.",
		language:       "en",
		historyTurns:   2,
		contextMode:    config.ChatAudioContextInlineClips,
	}
	s.history = []chatTurn{
		{wav: []byte("wavA"), transcript: "alpha"},
		{wav: []byte("wavB"), transcript: "bravo"},
	}
	msgs := s.buildMessages([][]byte{[]byte("wavCurrent")})
	if len(msgs) != 2 {
		t.Fatalf("len=%d want 2 (system + user)", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("role=%v want system", msgs[0]["role"])
	}
	sys := msgs[0]["content"].(string)
	if !strings.Contains(sys, "FINAL clip") {
		t.Fatalf("system missing FINAL-clip directive: %q", sys)
	}
	if !strings.Contains(sys, "Transcribe in en.") {
		t.Fatalf("system missing transcribe prompt: %q", sys)
	}
	parts := msgs[1]["content"].([]map[string]any)
	// 2 prior (label+audio) + 1 current (label+audio) = 6 parts
	if len(parts) != 6 {
		t.Fatalf("parts=%d want 6", len(parts))
	}
	if parts[0]["text"] != "[prior clip 1]:" || parts[2]["text"] != "[prior clip 2]:" || parts[4]["text"] != "[current clip]:" {
		t.Fatalf("clip labels wrong: %v %v %v", parts[0], parts[2], parts[4])
	}
	if parts[1]["type"] != "input_audio" || parts[3]["type"] != "input_audio" || parts[5]["type"] != "input_audio" {
		t.Fatalf("audio part types wrong: %v %v %v", parts[1]["type"], parts[3]["type"], parts[5]["type"])
	}
	audio := parts[5]["input_audio"].(map[string]any)
	decoded, err := base64.StdEncoding.DecodeString(audio["data"].(string))
	if err != nil || string(decoded) != "wavCurrent" {
		t.Fatalf("current audio decode mismatch err=%v got=%q", err, decoded)
	}
}

func TestBuildMessagesInlineClipsNoHistoryOmitsLastClipFraming(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe in {language}.",
		language:       "en",
		historyTurns:   2,
		contextMode:    config.ChatAudioContextInlineClips,
	}
	msgs := s.buildMessages([][]byte{[]byte("wavCurrent")})
	sys := msgs[0]["content"].(string)
	if strings.Contains(sys, "FINAL clip") {
		t.Fatalf("expected plain system prompt without FINAL-clip directive when no history; got %q", sys)
	}
	if sys != "Transcribe in en." {
		t.Fatalf("plain prompt mismatch: %q", sys)
	}
}

func TestBuildMessagesNoHistoryWhenZeroTurns(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe.",
		language:       "en",
		historyTurns:   0,
	}
	s.history = []chatTurn{
		{wav: []byte("a"), transcript: "alpha"},
	}
	msgs := s.buildMessages([][]byte{[]byte("now")})
	// system + user(audio) = 2
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("msgs=%v", msgs)
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
	// Multi-clip mode skips history, so no assistant turn anywhere.
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
	if !strings.Contains(name, "samples_closed") || !strings.Contains(name, "trailing") {
		t.Errorf("filename %q should encode samples_closed-trailing reason", name)
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

func TestEndsWithTerminalPunctuation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"hello.", true},
		{"hello?", true},
		{"hello!", true},
		{"trailed off…", true},
		{"hello.\n", true},
		{"hello.  ", true},
		{"and it", false},
		{"and it,", false},
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := endsWithTerminalPunctuation(c.in); got != c.want {
			t.Errorf("endsWithTerminalPunctuation(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestFormatReplacement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		priorFmt string
		newText  string
		want     string
	}{
		{"first segment no leading space", "first segment", "unified", "unified"},
		{"prior had leading space", " second", "unified", " unified"},
		{"new starts with punctuation no space added", " second", ", actually unified", ", actually unified"},
		{"new already has leading space kept", " second", " unified", " unified"},
		{"empty new is empty", " second", "  ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &chatAudioSession{lastEmittedFormatted: c.priorFmt}
			if got := s.formatReplacement(c.newText); got != c.want {
				t.Fatalf("formatReplacement(%q) with prior %q = %q, want %q",
					c.newText, c.priorFmt, got, c.want)
			}
		})
	}
}

// TestChatAudioSessionContinuationRebatch verifies that when
// continuation_rebatch is on and history's last entry's transcript
// lacks terminal punctuation, the next chunk is sent as a multi-clip
// POST that prepends the prior audio — the model sees both clips as
// one continuous utterance and the request must skip few-shot history
// (the audio itself is the cross-clip context).
func TestChatAudioSessionContinuationRebatch(t *testing.T) {
	type seenRequest struct{ body []byte }
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Sometimes I talk and it stops here.\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:             server.URL + "/api/v1",
		Model:               "gemma-test",
		Prompt:              "Transcribe in {language}.",
		Language:            "en",
		ContinuationRebatch: true,
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

	// Seed history with a prior turn whose transcript lacks terminal
	// punctuation. Pre-seed before any samples arrive so the worker's
	// happens-before is established via the chunksCh send.
	priorPCM := make([]int16, 8000) // 1s of audio @ 8 kHz
	for i := range priorPCM {
		priorPCM[i] = 1000
	}
	session.history = []chatTurn{{
		pcm:        priorPCM,
		wav:        encodePCM16WAV(priorPCM, 8000),
		transcript: "Sometimes I talk and it",
	}}
	session.lastEmittedFormatted = " Sometimes I talk and it"

	// Push one chunk of speech, then close so the trailing flush fires.
	chunk := make([]int16, 8000)
	for i := range chunk {
		chunk[i] = 2000
	}
	samples <- chunk
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := session.Finalize(finalCtx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	mu.Lock()
	calls := len(seen)
	var body []byte
	if calls > 0 {
		body = seen[0].body
	}
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}

	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages=%d want 2 (system + user) — rebatch must skip few-shot history", len(req.Messages))
	}
	if req.Messages[0]["role"] != "system" {
		t.Fatalf("messages[0].role=%v", req.Messages[0]["role"])
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
		if p.(map[string]any)["type"] == "input_audio" {
			audioCount++
		}
	}
	if audioCount != 2 {
		t.Fatalf("user content audio parts=%d want 2 (prior + current)", audioCount)
	}
	for _, m := range req.Messages {
		if m["role"] == "assistant" {
			t.Fatalf("rebatched request unexpectedly has assistant history turn: %v", m)
		}
	}

	// History should have been REPLACED, not appended.
	if got := len(session.history); got != 1 {
		t.Fatalf("history len=%d want 1 (rebatch replaces, not appends)", got)
	}
	if got := session.history[0].transcript; got != "Sometimes I talk and it stops here." {
		t.Fatalf("history[0].transcript=%q want the unified transcript", got)
	}
}

// TestChatAudioSessionContinuationRebatchSkippedWhenTooLong is a
// regression test: an unbounded rebatch chain on a long pause-free
// monologue grows the prepended prior audio until the combined POST
// exceeds Gemma's 30 s window, which silently drops the freshly-spoken
// tail. When prepending the prior clip would breach RebatchMaxSeconds,
// the rebatch must be SKIPPED — the current chunk posts as its own
// fresh segment (few-shot history, NOT a multi-clip merge) so the new
// audio is fully transcribed instead of being dropped past the cap.
func TestChatAudioSessionContinuationRebatchSkippedWhenTooLong(t *testing.T) {
	type seenRequest struct{ body []byte }
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"and the tail of what I said.\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:             server.URL + "/api/v1",
		Model:               "gemma-test",
		Prompt:              "Transcribe in {language}.",
		Language:            "en",
		ContinuationRebatch: true,
		// 1 s cap: prior (1 s) + current (1 s) = 2 s combined, which
		// breaches the bound and must force the rebatch to be skipped.
		RebatchMaxSeconds: 1,
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

	// Seed history with a prior turn whose transcript lacks terminal
	// punctuation — the rebatch trigger condition — but whose audio is
	// long enough that prepending it would breach RebatchMaxSeconds.
	priorPCM := make([]int16, 8000) // 1s of audio @ 8 kHz
	for i := range priorPCM {
		priorPCM[i] = 1000
	}
	session.history = []chatTurn{{
		pcm:        priorPCM,
		wav:        encodePCM16WAV(priorPCM, 8000),
		transcript: "I was saying something",
	}}
	session.lastEmittedFormatted = " I was saying something"

	chunk := make([]int16, 8000) // 1s — combined with prior = 2s > 1s cap
	for i := range chunk {
		chunk[i] = 2000
	}
	samples <- chunk
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := session.Finalize(finalCtx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	mu.Lock()
	calls := len(seen)
	var body []byte
	if calls > 0 {
		body = seen[0].body
	}
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}

	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// A skipped rebatch posts as a normal chunk: the prior turn rides
	// along as few-shot history, so the request carries an assistant
	// message. A rebatch would instead be system+user only.
	hasAssistant := false
	for _, m := range req.Messages {
		if m["role"] == "assistant" {
			hasAssistant = true
		}
	}
	if !hasAssistant {
		t.Fatalf("rebatch was not skipped — request has no assistant few-shot turn (messages=%d), the oversized prior audio was merged into a multi-clip POST", len(req.Messages))
	}

	// History must be APPENDED to (fresh segment), not REPLACED — a
	// rebatch overwrites history's last entry, a skip adds a new one.
	if got := len(session.history); got != 2 {
		t.Fatalf("history len=%d want 2 (skipped rebatch appends a fresh segment, not replaces)", got)
	}
	if got := session.history[0].transcript; got != "I was saying something" {
		t.Fatalf("history[0].transcript=%q want the prior turn left intact", got)
	}
	if got := session.history[1].transcript; got != "and the tail of what I said." {
		t.Fatalf("history[1].transcript=%q want the new segment transcribed in full", got)
	}
}

// TestChatAudioSessionContinuationRebatchPostFinalizeRetractsFromLive
// regression: when the prior segment was emitted on the events channel
// (i.e. before liveSegments flipped to false) and the trailing flush
// triggers a rebatch, the worker emits the result via the finals queue
// because liveSegments is now false. The trailing-collector can't
// absorb a retraction against its empty buffer, so the unapplied
// retraction must be forwarded up via FinalizeResult.RetractFromLivePrevLen
// for the caller to strip from its already-emitted live text. Without
// this plumbing, the prior broken segment stays in liveText and the
// unified rebatch text is appended ON TOP, double-pasting the prefix.
func TestChatAudioSessionContinuationRebatchPostFinalizeRetractsFromLive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"alpha beta gamma.\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.TranscriptionConfig{
		BaseURL:             server.URL + "/api/v1",
		Model:               "gemma-test",
		Prompt:              "Transcribe.",
		Language:            "en",
		ContinuationRebatch: true,
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 8000,
		Channels:   1,
		Samples:    samples,
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}

	// Seed history with an unfinished prior segment as if it had been
	// emitted live earlier in the session. lastEmittedFormatted
	// reflects the runes the caller (app.go) already added to its
	// liveText: " alpha beta" (12 runes, including leading space).
	priorPCM := make([]int16, 8000)
	for i := range priorPCM {
		priorPCM[i] = 1000
	}
	session.history = []chatTurn{{
		pcm:        priorPCM,
		wav:        encodePCM16WAV(priorPCM, 8000),
		transcript: "alpha beta",
	}}
	session.lastEmittedFormatted = " alpha beta"

	// Push the chunk THEN immediately close samples so the trailing
	// flush fires before any non-trailing chunk is processed. Worker
	// will see liveSegments=false at processing time (Finalize flipped
	// it), so the rebatch result goes through the finals queue.
	chunk := make([]int16, 8000)
	for i := range chunk {
		chunk[i] = 2000
	}
	samples <- chunk
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.Finalize(finalCtx)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	const priorEmittedRunes = 11 // " alpha beta"
	if result.RetractFromLivePrevLen != priorEmittedRunes {
		t.Fatalf("RetractFromLivePrevLen=%d want %d (the prior segment's rune count)",
			result.RetractFromLivePrevLen, priorEmittedRunes)
	}
	if !strings.Contains(result.Text, "alpha beta gamma.") {
		t.Fatalf("trailing text=%q want unified transcript", result.Text)
	}
	if strings.Count(result.Text, "alpha") != 1 {
		t.Fatalf("trailing text=%q has %d occurrences of 'alpha' want 1 (no duplication)",
			result.Text, strings.Count(result.Text, "alpha"))
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
