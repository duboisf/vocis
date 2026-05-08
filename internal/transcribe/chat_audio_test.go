package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	msgs := s.buildMessages(current)
	if len(msgs) != 5 {
		t.Fatalf("len=%d want 5 (2 user/assistant pairs + 1 user)", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Fatalf("msg[0].role=%v want user", msgs[0]["role"])
	}
	if msgs[1]["role"] != "assistant" || msgs[1]["content"] != "first" {
		t.Fatalf("msg[1]=%v", msgs[1])
	}
	if msgs[2]["role"] != "user" {
		t.Fatalf("msg[2].role=%v want user", msgs[2]["role"])
	}
	if msgs[3]["role"] != "assistant" || msgs[3]["content"] != "second" {
		t.Fatalf("msg[3]=%v", msgs[3])
	}
	if msgs[4]["role"] != "user" {
		t.Fatalf("msg[4].role=%v want user", msgs[4]["role"])
	}

	// User content must be the [text, input_audio] pair with the
	// language-substituted prompt.
	parts, ok := msgs[4]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("user content type=%T", msgs[4]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("user content parts=%d", len(parts))
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "Transcribe in en." {
		t.Fatalf("text part=%v", parts[0])
	}
	if parts[1]["type"] != "input_audio" {
		t.Fatalf("audio part type=%v", parts[1]["type"])
	}
	audio := parts[1]["input_audio"].(map[string]any)
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
	msgs := s.buildMessages([]byte("now"))
	if len(msgs) != 3 {
		t.Fatalf("len=%d want 3", len(msgs))
	}
	if msgs[1]["content"] != "charlie" {
		t.Fatalf("kept turn=%v want charlie", msgs[1]["content"])
	}
}

func TestBuildMessagesInlineClipsSingleUserTurn(t *testing.T) {
	t.Parallel()
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
	msgs := s.buildMessages([]byte("wavCurrent"))
	if len(msgs) != 1 {
		t.Fatalf("len=%d want 1 (single user message)", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Fatalf("role=%v want user", msgs[0]["role"])
	}
	parts, ok := msgs[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content type=%T", msgs[0]["content"])
	}
	// 1 leading text + 2*(prior count) + 2 (current label + audio) =
	// 1 + 4 + 2 = 7 parts
	if len(parts) != 7 {
		t.Fatalf("parts=%d want 7", len(parts))
	}
	// Leading instruction must scope to the FINAL clip when context
	// is non-empty.
	leading := parts[0]
	if leading["type"] != "text" {
		t.Fatalf("leading type=%v", leading["type"])
	}
	if !strings.Contains(leading["text"].(string), "FINAL clip") {
		t.Fatalf("leading instruction missing FINAL-clip directive: %q", leading["text"])
	}
	// Parts 1,3 are prior labels; parts 2,4 are prior audio.
	if parts[1]["text"] != "[prior clip 1]:" {
		t.Fatalf("parts[1]=%v", parts[1])
	}
	if parts[3]["text"] != "[prior clip 2]:" {
		t.Fatalf("parts[3]=%v", parts[3])
	}
	if parts[2]["type"] != "input_audio" || parts[4]["type"] != "input_audio" {
		t.Fatalf("prior audio parts wrong types: %v %v", parts[2]["type"], parts[4]["type"])
	}
	// Last two parts: current label + current audio.
	if parts[5]["text"] != "[current clip]:" {
		t.Fatalf("parts[5]=%v", parts[5])
	}
	if parts[6]["type"] != "input_audio" {
		t.Fatalf("parts[6]=%v", parts[6])
	}
	audio := parts[6]["input_audio"].(map[string]any)
	decoded, err := base64.StdEncoding.DecodeString(audio["data"].(string))
	if err != nil || string(decoded) != "wavCurrent" {
		t.Fatalf("current audio decode mismatch err=%v got=%q", err, decoded)
	}
}

func TestBuildMessagesInlineClipsNoHistoryUsesPlainPrompt(t *testing.T) {
	t.Parallel()
	s := &chatAudioSession{
		promptTemplate: "Transcribe in {language}.",
		language:       "en",
		historyTurns:   2,
		contextMode:    config.ChatAudioContextInlineClips,
	}
	msgs := s.buildMessages([]byte("wavCurrent"))
	parts := msgs[0]["content"].([]map[string]any)
	leading := parts[0]["text"].(string)
	if strings.Contains(leading, "FINAL clip") {
		t.Fatalf("expected plain prompt without FINAL-clip directive when no history; got %q", leading)
	}
	if leading != "Transcribe in en." {
		t.Fatalf("plain prompt mismatch: %q", leading)
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
	msgs := s.buildMessages([]byte("now"))
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
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

// TestChatAudioSessionEndToEnd drives a full session against an
// httptest server that mimics Lemonade's SSE chat-completions response.
// Verifies: VAD-bounded chunking, per-chunk POST, history accumulation
// across two chunks, and Finalize collecting the trailing transcript
// when liveSegments is off at chunk arrival time.
func TestChatAudioSessionEndToEnd(t *testing.T) {
	type seenRequest struct {
		body []byte
	}
	var (
		mu       sync.Mutex
		seen     []seenRequest
		replies  = []string{"hello", "world"}
		callIdx  int
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
		Backend: config.BackendLemonadeChat,
		BaseURL: server.URL + "/api/v1",
		Model:   "gemma-test",
		ChatAudio: config.ChatAudioConfig{
			ChunkMaxSeconds: 10,
			HistoryTurns:    2,
			Prompt:          "Transcribe in {language}.",
			Language:        "en",
			Stream:          true,
		},
	}
	streaming := config.StreamingConfig{
		// Disable Silero in the test path — sileroSampleRate guard
		// triggers when sampleRate doesn't match, falling back to
		// chunk_max_seconds-only chunking, which is what we want here.
	}
	samples := make(chan []int16, 2)
	opts := DictationOpts{
		SampleRate: 8000, // deliberately != sileroSampleRate so VAD is skipped
		Channels:   1,
		Samples:    samples,
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, streaming, httpClient, opts)
	if err != nil {
		t.Fatalf("startChatAudioSession: %v", err)
	}

	// Push two force-cut chunks. With chunk_max_seconds=10 at 8 kHz,
	// the cap is 80000 samples per chunk. Send exactly that twice.
	chunk := make([]int16, 80000)
	samples <- chunk
	samples <- chunk
	close(samples)

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.Finalize(finalCtx)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(res.Text, "hello") || !strings.Contains(res.Text, "world") {
		t.Fatalf("joined text=%q want hello+world", res.Text)
	}

	mu.Lock()
	calls := len(seen)
	first := seen[0].body
	second := seen[1].body
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}

	// Second request must include the first response as an assistant
	// turn (few-shot history). Decode and inspect.
	var req2 struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(second, &req2); err != nil {
		t.Fatalf("decode req2: %v", err)
	}
	foundHistory := false
	for _, m := range req2.Messages {
		if m["role"] == "assistant" && m["content"] == "hello" {
			foundHistory = true
			break
		}
	}
	if !foundHistory {
		t.Fatalf("second request missing assistant=hello in history; messages=%+v", req2.Messages)
	}

	// First request must NOT contain any assistant turn.
	var req1 struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(first, &req1); err != nil {
		t.Fatalf("decode req1: %v", err)
	}
	for _, m := range req1.Messages {
		if m["role"] == "assistant" {
			t.Fatalf("first request unexpectedly has assistant turn: %v", m)
		}
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
		Backend: config.BackendLemonadeChat,
		BaseURL: server.URL,
		Model:   "gemma-test",
		HallucinationFilters: []string{"Thank you."},
		ChatAudio: config.ChatAudioConfig{
			ChunkMaxSeconds: 10,
			HistoryTurns:    1,
			Prompt:          "Transcribe.",
			Language:        "en",
			Stream:          true,
		},
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate: 8000,
		Channels:   1,
		Samples:    samples,
	}
	session, err := startChatAudioSession(context.Background(), cfg, config.StreamingConfig{}, &http.Client{Timeout: 5 * time.Second}, opts)
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

