package openai

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"vtt/internal/config"
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
			assertClientSecretRequest(t, body, defaultProgrammerPrompt)

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
			assertSessionUpdate(t, sessionUpdate, defaultProgrammerPrompt)

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
	cfg.OpenAI.BaseURL = server.URL
	cfg.OpenAI.Organization = "org_test"
	cfg.OpenAI.Project = "proj_test"
	cfg.OpenAI.Language = "en"

	client := New("test-key", cfg.OpenAI)
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
	got, err := stream.Commit(ctx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got != "hello world" {
		t.Fatalf("transcript = %q", got)
	}
	if partial := stream.Partial(); partial != "hello " {
		t.Fatalf("partial = %q", partial)
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
			assertClientSecretRequest(t, body, "Use technical spelling.")

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
			assertSessionUpdate(t, sessionUpdate, "Use technical spelling.")

			_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.OpenAI.BaseURL = server.URL
	cfg.OpenAI.PromptHint = "Use technical spelling."

	client := New("test-key", cfg.OpenAI)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()
}

func TestPCMEncoderUpsamplesAndDownmixes(t *testing.T) {
	t.Parallel()

	encoder := newPCMEncoder(16000, 2)
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

func TestDialErrorIncludesHTTPDetails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req_test")
		http.Error(w, "bad realtime request", http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.OpenAI.BaseURL = server.URL
	client := New("test-key", cfg.OpenAI)

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

func assertClientSecretRequest(t *testing.T, body map[string]any, wantPrompt string) {
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
}

func assertSessionUpdate(t *testing.T, event map[string]any, wantPrompt string) {
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
	if value, ok := input["turn_detection"]; !ok || value != nil {
		t.Fatalf("turn_detection = %v, want explicit null", value)
	}

	transcription := input["transcription"].(map[string]any)
	if got := transcription["model"]; got != "gpt-4o-mini-transcribe" {
		t.Fatalf("model = %v", got)
	}
	if got := transcription["prompt"]; got != wantPrompt {
		t.Fatalf("prompt = %v, want %q", got, wantPrompt)
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

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
