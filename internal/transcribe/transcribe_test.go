package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"vocis/internal/config"
)

func TestStartStreamAppendsPCMAndReturnsTranscript(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	var sawAuth string
	var sawOrg string
	var sawProject string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			sawAuth = r.Header.Get("Authorization")
			sawOrg = r.Header.Get("OpenAI-Organization")
			sawProject = r.Header.Get("OpenAI-Project")

			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode client secret request: %v", err)
			}
			assertClientSecretRequest(t, body, config.DefaultPromptHint, true)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session": map[string]any{
					"type": "transcription",
				},
			})
		case "/realtime":
			if got := r.Header.Get("Authorization"); got != "Bearer ek_test" {
				t.Fatalf("websocket authorization = %q", got)
			}

			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			if err := conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			}); err != nil {
				t.Fatalf("write created event: %v", err)
			}

			var sessionUpdate map[string]any
			if err := conn.ReadJSON(&sessionUpdate); err != nil {
				t.Fatalf("read session update: %v", err)
			}
			assertSessionUpdate(t, sessionUpdate, config.DefaultPromptHint, true)

			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})

			var appendEvent map[string]any
			if err := conn.ReadJSON(&appendEvent); err != nil {
				t.Fatalf("read append event: %v", err)
			}
			if got := appendEvent["type"]; got != "input_audio_buffer.append" {
				t.Fatalf("append type = %v", got)
			}
			gotBytes := decodeAudioPayload(t, appendEvent["audio"].(string))
			wantBytes := []byte{}
			for _, sample := range []int16{1000, -1000, 2000, -2000} {
				wantBytes = binary.LittleEndian.AppendUint16(wantBytes, uint16(sample))
			}
			if string(gotBytes) != string(wantBytes) {
				t.Fatalf("audio payload = %v, want %v", gotBytes, wantBytes)
			}

			var commitEvent map[string]any
			if err := conn.ReadJSON(&commitEvent); err != nil {
				t.Fatalf("read commit event: %v", err)
			}
			if got := commitEvent["type"]; got != "input_audio_buffer.commit" {
				t.Fatalf("commit type = %v", got)
			}

			if err := conn.WriteJSON(map[string]any{
				"type":  "conversation.item.input_audio_transcription.delta",
				"delta": "hello ",
			}); err != nil {
				t.Fatalf("write delta event: %v", err)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"transcript": "hello world",
			}); err != nil {
				t.Fatalf("write completed event: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL
	cfg.Transcription.Organization = "org_test"
	cfg.Transcription.Project = "proj_test"
	cfg.Transcription.Language = "en"

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()

	if err := stream.Append(ctx, []int16{1000, -1000, 2000, -2000}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := stream.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gotPartial string
	var gotFinal string
	for gotFinal == "" {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case event := <-stream.Events():
			switch event.Type {
			case StreamEventPartial:
				if event.Text != "" {
					gotPartial = event.Text
				}
			case StreamEventFinal:
				gotFinal = event.Text
			case StreamEventError:
				t.Fatalf("stream error: %v", event.Err)
			}
		}
	}

	if gotFinal != "hello world" {
		t.Fatalf("transcript = %q", gotFinal)
	}
	if gotPartial != "hello " {
		t.Fatalf("partial event = %q", gotPartial)
	}
	if partial := stream.Partial(); partial != "" {
		t.Fatalf("partial = %q, want empty after completion", partial)
	}
	if sawAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", sawAuth)
	}
	if sawOrg != "org_test" {
		t.Fatalf("organization = %q", sawOrg)
	}
	if sawProject != "proj_test" {
		t.Fatalf("project = %q", sawProject)
	}
}

func TestStartStreamUsesPromptHintWhenConfigured(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode client secret request: %v", err)
			}
			assertClientSecretRequest(t, body, "Use technical spelling.", true)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session": map[string]any{
					"type": "transcription",
				},
			})
		case "/realtime":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			if err := conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			}); err != nil {
				t.Fatalf("write created event: %v", err)
			}

			var sessionUpdate map[string]any
			if err := conn.ReadJSON(&sessionUpdate); err != nil {
				t.Fatalf("read session update: %v", err)
			}
			assertSessionUpdate(t, sessionUpdate, "Use technical spelling.", true)

			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL
	cfg.Transcription.PromptHint = "Use technical spelling."

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()
}

func TestStartStreamKeepsFirstWordWhenCompletedTranscriptIsSuffix(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session": map[string]any{
					"type": "transcription",
				},
			})
		case "/realtime":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			if err := conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			}); err != nil {
				t.Fatalf("write created event: %v", err)
			}

			var sessionUpdate map[string]any
			if err := conn.ReadJSON(&sessionUpdate); err != nil {
				t.Fatalf("read session update: %v", err)
			}

			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})

			var appendEvent map[string]any
			if err := conn.ReadJSON(&appendEvent); err != nil {
				t.Fatalf("read append event: %v", err)
			}
			if got := appendEvent["type"]; got != "input_audio_buffer.append" {
				t.Fatalf("append type = %v", got)
			}

			var commitEvent map[string]any
			if err := conn.ReadJSON(&commitEvent); err != nil {
				t.Fatalf("read commit event: %v", err)
			}
			if got := commitEvent["type"]; got != "input_audio_buffer.commit" {
				t.Fatalf("commit type = %v", got)
			}

			if err := conn.WriteJSON(map[string]any{
				"type":    "conversation.item.input_audio_transcription.delta",
				"item_id": "item_123",
				"delta":   "hello world",
			}); err != nil {
				t.Fatalf("write delta event: %v", err)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"item_id":    "item_123",
				"transcript": "world",
			}); err != nil {
				t.Fatalf("write completed event: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()

	if err := stream.Append(ctx, []int16{1000, -1000, 2000, -2000}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := stream.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gotFinal string
	for gotFinal == "" {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case event := <-stream.Events():
			switch event.Type {
			case StreamEventFinal:
				gotFinal = event.Text
			case StreamEventError:
				t.Fatalf("stream error: %v", event.Err)
			}
		}
	}

	if gotFinal != "hello world" {
		t.Fatalf("final transcript = %q, want hello world", gotFinal)
	}
}

func TestCanSkipTrailingTimeout(t *testing.T) {
	t.Parallel()

	session := &DictationSession{}
	session.segmentCount.Store(1)
	stream := &Stream{}

	if !session.canSkipTrailingTimeout(context.DeadlineExceeded, stream) {
		t.Fatal("expected trailing timeout to be skippable for segmented dictation")
	}
}

func TestCannotSkipTrailingTimeoutWithoutSegments(t *testing.T) {
	t.Parallel()

	session := &DictationSession{}
	stream := &Stream{}

	if session.canSkipTrailingTimeout(context.DeadlineExceeded, stream) {
		t.Fatal("expected trailing timeout to fail when no live segments were inserted")
	}
}

func TestPCMEncoderUpsamplesAndDownmixes(t *testing.T) {
	t.Parallel()

	encoder := newPCMEncoder(16000, 24000, 2)
	got := encoder.Encode([]int16{
		1000, 3000,
		-1000, -3000,
		2000, 4000,
		-2000, -4000,
	})

	if gotSamples := decodePCM16(got); len(gotSamples) != 6 {
		t.Fatalf("encoded sample count = %d, want 6", len(gotSamples))
	} else {
		want := []int16{2000, -2000, -2000, 3000, -3000, -3000}
		for i := range want {
			if gotSamples[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d", i, gotSamples[i], want[i])
			}
		}
	}
}

func TestFormatRealtimeErrorMarksEmptyCommit(t *testing.T) {
	t.Parallel()

	err := formatRealtimeError("input_audio_buffer_commit_empty", "buffer too small")
	if !errors.Is(err, ErrInputAudioBufferCommitEmpty) {
		t.Fatalf("errors.Is(err, ErrInputAudioBufferCommitEmpty) = false, err=%v", err)
	}
}

func TestReconcileCompletedTranscriptKeepsFullTurnWhenCompletedIsSuffix(t *testing.T) {
	t.Parallel()

	got := reconcileCompletedTranscript("hello world", "world")
	if got != "hello world" {
		t.Fatalf("reconcileCompletedTranscript = %q, want hello world", got)
	}
}

func TestReconcileCompletedTranscriptUsesCompletedWhenItIsIndependent(t *testing.T) {
	t.Parallel()

	got := reconcileCompletedTranscript("hello world", "goodbye world")
	if got != "goodbye world" {
		t.Fatalf("reconcileCompletedTranscript = %q, want goodbye world", got)
	}
}

func TestDictationSessionHandleStreamEventEmitsLiveSegment(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events: make(chan DictationEvent, 1),
		finals: make(chan finalResult, 1),
	}
	session.liveSegments.Store(true)

	err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventFinal,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	select {
	case event := <-session.events:
		if event.Type != DictationEventSegment {
			t.Fatalf("event.Type = %q, want segment", event.Type)
		}
		if event.Text != "hello world" {
			t.Fatalf("event.Text = %q, want hello world", event.Text)
		}
	default:
		t.Fatal("expected live segment event")
	}

	select {
	case result := <-session.finals:
		t.Fatalf("unexpected queued final: %+v", result)
	default:
	}
}

func TestDictationSessionHandleStreamEventQueuesFinalAfterLiveDisabled(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events: make(chan DictationEvent, 1),
		finals: make(chan finalResult, 1),
	}
	// liveSegments left false (zero value)

	err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventFinal,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	select {
	case result := <-session.finals:
		if result.text != "hello world" {
			t.Fatalf("result.text = %q, want hello world", result.text)
		}
		if result.err != nil {
			t.Fatalf("result.err = %v, want nil", result.err)
		}
	default:
		t.Fatal("expected queued final result")
	}
}

func TestAppendSegmentTextAddsSpaceBetweenChunks(t *testing.T) {
	t.Parallel()

	got := appendSegmentText("hello", "world")
	if got != "hello world" {
		t.Fatalf("appendSegmentText = %q, want hello world", got)
	}
}

// TestStreamAppendPartialIncrementalDeltas exercises the incremental
// delta semantics used by gpt-4o-transcribe et al.: each event carries
// only the new text to append.
func TestStreamAppendPartialIncrementalDeltas(t *testing.T) {
	t.Parallel()

	s := &Stream{mergeDelta: mergeIncrementalDelta}
	s.appendPartial("item_1", "Ok")
	s.appendPartial("item_1", " I")
	got := s.appendPartial("item_1", " see")
	if got != "Ok I see" {
		t.Fatalf("incremental partial = %q, want %q", got, "Ok I see")
	}
}

// TestStreamAppendPartialCumulativeDeltas exercises the cumulative
// delta semantics used by Whisper models: each event carries the full
// transcript so far. Naïve concatenation would produce
// "OkOK IOK I see" — the merge strategy must replace rather than append.
func TestStreamAppendPartialCumulativeDeltas(t *testing.T) {
	t.Parallel()

	s := &Stream{mergeDelta: mergeCumulativeDelta}
	s.appendPartial("item_1", "Ok")
	s.appendPartial("item_1", "OK I")
	got := s.appendPartial("item_1", "OK I see")
	if got != "OK I see" {
		t.Fatalf("cumulative partial = %q, want %q", got, "OK I see")
	}
}

// TestStreamAppendPartialDefaultsToIncremental documents that a zero-value
// Stream (constructed without a transport in tests) falls back to the
// OpenAI-style append. Protects helpers that build a Stream directly.
func TestStreamAppendPartialDefaultsToIncremental(t *testing.T) {
	t.Parallel()

	s := &Stream{}
	got := s.appendPartial("", "hello") + s.appendPartial("", " world")
	if got != "hellohello world" {
		// first call returns "hello" (partial=""), second returns "hello world".
		// sum is "hello" + "hello world" = "hellohello world".
		t.Fatalf("default-merge partials = %q", got)
	}
}

func TestDialErrorIncludesHTTPDetails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req_test")
		http.Error(w, "bad realtime request", http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL
	client := New("test-key", cfg.Transcription, cfg.Streaming)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.StartStream(ctx, 24000, 1)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("error = %q", err)
	}
	if !strings.Contains(err.Error(), "req_test") {
		t.Fatalf("error = %q", err)
	}
}

func TestStartStreamEnablesServerVADForSegmentMode(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode client secret request: %v", err)
			}
			assertClientSecretRequest(t, body, config.DefaultPromptHint, true)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session": map[string]any{
					"type": "transcription",
				},
			})
		case "/realtime":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			if err := conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			}); err != nil {
				t.Fatalf("write created event: %v", err)
			}

			var sessionUpdate map[string]any
			if err := conn.ReadJSON(&sessionUpdate); err != nil {
				t.Fatalf("read session update: %v", err)
			}
			assertSessionUpdate(t, sessionUpdate, config.DefaultPromptHint, true)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()
}

func assertClientSecretRequest(t *testing.T, body map[string]any, wantPrompt string, wantSegment bool) {
	t.Helper()

	session := body["session"].(map[string]any)
	if got := session["type"]; got != "transcription" {
		t.Fatalf("session type = %v", got)
	}

	audio := session["audio"].(map[string]any)
	input := audio["input"].(map[string]any)
	format := input["format"].(map[string]any)
	if got := format["type"]; got != "audio/pcm" {
		t.Fatalf("audio format = %v", got)
	}
	if got := int(format["rate"].(float64)); got != 24000 {
		t.Fatalf("audio rate = %d", got)
	}

	transcription := input["transcription"].(map[string]any)
	if got := transcription["model"]; got != "gpt-4o-mini-transcribe" {
		t.Fatalf("model = %v", got)
	}
	if got := transcription["prompt"]; got != wantPrompt {
		t.Fatalf("prompt = %v, want %q", got, wantPrompt)
	}
	assertTurnDetection(t, input["turn_detection"], wantSegment)
}

func assertSessionUpdate(t *testing.T, event map[string]any, wantPrompt string, wantSegment bool) {
	t.Helper()

	if got := event["type"]; got != "session.update" {
		t.Fatalf("event type = %v", got)
	}

	session := event["session"].(map[string]any)
	if got := session["type"]; got != "transcription" {
		t.Fatalf("session type = %v", got)
	}
	audio := session["audio"].(map[string]any)
	input := audio["input"].(map[string]any)
	format := input["format"].(map[string]any)
	if got := format["type"]; got != "audio/pcm" {
		t.Fatalf("audio format = %v", got)
	}
	if got := int(format["rate"].(float64)); got != 24000 {
		t.Fatalf("audio rate = %d", got)
	}
	transcription := input["transcription"].(map[string]any)
	if got := transcription["model"]; got != "gpt-4o-mini-transcribe" {
		t.Fatalf("model = %v", got)
	}
	if got := transcription["prompt"]; got != wantPrompt {
		t.Fatalf("prompt = %v, want %q", got, wantPrompt)
	}
	assertTurnDetection(t, input["turn_detection"], wantSegment)
}

func assertTurnDetection(t *testing.T, value any, wantSegment bool) {
	t.Helper()

	if !wantSegment {
		if value != nil {
			t.Fatalf("turn_detection = %v, want nil", value)
		}
		return
	}

	turnDetection, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("turn_detection type = %T, want map[string]any", value)
	}
	if got := turnDetection["type"]; got != "server_vad" {
		t.Fatalf("turn_detection.type = %v", got)
	}
	if got := int(turnDetection["prefix_padding_ms"].(float64)); got != 300 {
		t.Fatalf("turn_detection.prefix_padding_ms = %d", got)
	}
	if got := int(turnDetection["silence_duration_ms"].(float64)); got != 500 {
		t.Fatalf("turn_detection.silence_duration_ms = %d", got)
	}
	if got := turnDetection["threshold"].(float64); got != 0.02 {
		t.Fatalf("turn_detection.threshold = %v", got)
	}
}

func decodeAudioPayload(t *testing.T, value string) []byte {
	t.Helper()

	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode audio payload: %v", err)
	}
	return data
}

func decodePCM16(data []byte) []int16 {
	samples := make([]int16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		samples = append(samples, int16(binary.LittleEndian.Uint16(data[i:])))
	}
	return samples
}

func TestFinalizeReceivesTrailingSegmentAfterSamplesClose(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session":    map[string]any{"type": "transcription"},
			})
		case "/realtime":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			_ = conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			})
			var sessionUpdate map[string]any
			_ = conn.ReadJSON(&sessionUpdate)
			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})

			// Read the audio append.
			var appendEvent map[string]any
			_ = conn.ReadJSON(&appendEvent)

			// Wait for the commit (sent by Finalize after samples close).
			var commitEvent map[string]any
			_ = conn.ReadJSON(&commitEvent)

			// Simulate a delay before the final transcript arrives.
			// This is the critical part: the final arrives AFTER the pump
			// has exited (samples closed). The stream event consumer must
			// still be running to receive it.
			time.Sleep(100 * time.Millisecond)
			_ = conn.WriteJSON(map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"transcript": "trailing words",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	samples := make(chan []int16, 1)
	samples <- []int16{1000, -1000, 2000, -2000}

	dictation, err := client.StartDictation(ctx, DictationOpts{SampleRate: 24000, Channels: 1, Samples: samples})
	if err != nil {
		t.Fatalf("start dictation: %v", err)
	}

	// Drain events to avoid blocking the session.
	go func() {
		for range dictation.Events() {
		}
	}()

	// Let the stream connect and audio flush.
	time.Sleep(200 * time.Millisecond)

	// Close samples — simulates the user releasing the hotkey.
	close(samples)

	result, err := dictation.Finalize(ctx)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if !strings.Contains(result.Text, "trailing") {
		t.Fatalf("result.Text = %q, want it to contain 'trailing'", result.Text)
	}
}

func TestFinalizeSucceedsWhenCommitEmptyArrivesViaFinals(t *testing.T) {
	t.Parallel()

	// Simulates: all audio consumed by live segments, then Commit succeeds
	// (WebSocket write ok) but the server responds with "buffer empty" error
	// via stream events. Finalize should return the accumulated segment text,
	// not an error.

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realtime/client_secrets":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": 123,
				"value":      "ek_test",
				"session":    map[string]any{"type": "transcription"},
			})
		case "/realtime":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()

			_ = conn.WriteJSON(map[string]any{
				"type":    "session.created",
				"session": map[string]any{"type": "transcription"},
			})
			var sessionUpdate map[string]any
			_ = conn.ReadJSON(&sessionUpdate)
			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})

			// Read audio appends and the final commit.
			// Emit a segment on the first append only (simulating server VAD
			// consuming that audio). Later appends are trailing audio that
			// the server hasn't processed into a segment yet.
			segmentSent := false
			var gotCommit bool
			for !gotCommit {
				var msg map[string]any
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
				switch msg["type"] {
				case "input_audio_buffer.append":
					if !segmentSent {
						segmentSent = true
						_ = conn.WriteJSON(map[string]any{
							"type":       "conversation.item.input_audio_transcription.completed",
							"transcript": "all segments delivered",
						})
					}
				case "input_audio_buffer.commit":
					gotCommit = true
				}
			}

			// Server responds: buffer empty (all audio already consumed).
			_ = conn.WriteJSON(map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "input_audio_buffer_commit_empty",
					"message": "buffer too small",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendOpenAI
	cfg.Transcription.Model = "gpt-4o-mini-transcribe"
	cfg.Transcription.BaseURL = server.URL

	client := New("test-key", cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	samples := make(chan []int16, 4)
	samples <- []int16{1000, -1000, 2000, -2000}

	dictation, err := client.StartDictation(ctx, DictationOpts{SampleRate: 24000, Channels: 1, Samples: samples})
	if err != nil {
		t.Fatalf("start dictation: %v", err)
	}

	// Drain live segment events.
	go func() {
		for range dictation.Events() {
		}
	}()

	// Let stream connect and segment arrive.
	time.Sleep(300 * time.Millisecond)

	// Send more audio AFTER the segment cleared the trailing flag.
	// This keeps hasTrailing=true, matching the production scenario
	// where audio chunks continue arriving after the last segment.
	samples <- []int16{500, -500}
	time.Sleep(50 * time.Millisecond)

	// Close samples — simulates user releasing hotkey.
	close(samples)

	_, err = dictation.Finalize(ctx)
	if err != nil {
		t.Fatalf("finalize should succeed (empty commit is expected), got error: %v", err)
	}
	// Live segment text is accumulated by the app layer via Events(),
	// not returned by Finalize. Finalize just needs to not error out.
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
