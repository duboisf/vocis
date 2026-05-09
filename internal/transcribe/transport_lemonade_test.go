package transcribe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"vocis/internal/config"
)

func TestLemonadeTransportBuildsRealtimeURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		model     string
		wantHost  string
		wantPath  string
		wantModel string
	}{
		{"default", "", "Whisper-Tiny", "localhost:9000", "/realtime", "Whisper-Tiny"},
		{"http scheme upgraded", "http://localhost:9000", "Whisper-Base", "localhost:9000", "/realtime", "Whisper-Base"},
		{"explicit ws scheme", "ws://example.com:1234", "model-x", "example.com:1234", "/realtime", "model-x"},
		{"https upgraded to wss", "https://lemonade.example/", "x", "lemonade.example", "/realtime", "x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.TranscriptionConfig{
				Backend:     config.BackendLemonade,
				RealtimeURL: tc.raw,
				Model:       tc.model,
			}
			transport := newLemonadeTransport(cfg, config.StreamingConfig{}, time.Second)

			got, err := transport.buildURL()
			if err != nil {
				t.Fatalf("buildURL: %v", err)
			}

			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse %q: %v", got, err)
			}
			if u.Scheme != "ws" && u.Scheme != "wss" {
				t.Fatalf("scheme = %q, want ws or wss", u.Scheme)
			}
			if u.Host != tc.wantHost {
				t.Fatalf("host = %q, want %q", u.Host, tc.wantHost)
			}
			if u.Path != tc.wantPath {
				t.Fatalf("path = %q, want %q", u.Path, tc.wantPath)
			}
			if got := u.Query().Get("model"); got != tc.wantModel {
				t.Fatalf("model query = %q, want %q", got, tc.wantModel)
			}
		})
	}
}

func TestLemonadeTransportSessionUpdateShape(t *testing.T) {
	t.Parallel()

	cfg := config.TranscriptionConfig{
		Backend:  config.BackendLemonade,
		Model:    "Whisper-Tiny",
		Language: "en",
	}
	streaming := config.StreamingConfig{
		PrefixPaddingMS:   300,
		SilenceDurationMS: 500,
		Threshold:         0.02,
		ManualCommit:      false,
	}
	transport := newLemonadeTransport(cfg, streaming, time.Second)

	payload := transport.SessionUpdate()
	if got := payload["type"]; got != "session.update" {
		t.Fatalf("type = %v, want session.update", got)
	}
	session, ok := payload["session"].(map[string]any)
	if !ok {
		t.Fatalf("session is not a map: %T", payload["session"])
	}
	if got := session["model"]; got != "Whisper-Tiny" {
		t.Fatalf("session.model = %v", got)
	}
	if got := session["language"]; got != "en" {
		t.Fatalf("session.language = %v", got)
	}
	// Lemonade's payload is flat — must NOT contain OpenAI's nested audio/transcription wrappers.
	if _, exists := session["audio"]; exists {
		t.Fatalf("session.audio should not be present in lemonade payload, got %v", session["audio"])
	}

	td, ok := session["turn_detection"].(map[string]any)
	if !ok {
		t.Fatalf("session.turn_detection = %T, want map[string]any", session["turn_detection"])
	}
	if got := td["threshold"]; got != 0.02 {
		t.Fatalf("turn_detection.threshold = %v, want 0.02", got)
	}
	if got := td["silence_duration_ms"]; got != 500 {
		t.Fatalf("turn_detection.silence_duration_ms = %v, want 500", got)
	}
	if got := td["prefix_padding_ms"]; got != 300 {
		t.Fatalf("turn_detection.prefix_padding_ms = %v, want 300", got)
	}
	// Lemonade's turn_detection must NOT carry OpenAI's `type: server_vad` field.
	if _, exists := td["type"]; exists {
		t.Fatalf("turn_detection.type should not be present in lemonade payload")
	}
}

// TestLemonadeTransportSessionUpdateManualCommit verifies that
// streaming.manual_commit=true emits `turn_detection: null` on the wire,
// which is the signal Lemonade PR #1607 honors by disabling server-side
// VAD and interim transcription work.
func TestLemonadeTransportSessionUpdateManualCommit(t *testing.T) {
	t.Parallel()

	cfg := config.TranscriptionConfig{
		Backend: config.BackendLemonade,
		Model:   "Whisper-Tiny",
	}
	streaming := config.StreamingConfig{
		PrefixPaddingMS:   300,
		SilenceDurationMS: 500,
		Threshold:         0.02,
		ManualCommit:      true,
	}
	transport := newLemonadeTransport(cfg, streaming, time.Second)

	payload := transport.SessionUpdate()
	session, ok := payload["session"].(map[string]any)
	if !ok {
		t.Fatalf("session is not a map: %T", payload["session"])
	}

	// In-memory: the key is present and its value is nil.
	td, present := session["turn_detection"]
	if !present {
		t.Fatal("session.turn_detection key must be present (explicit null)")
	}
	if td != nil {
		t.Fatalf("turn_detection = %v, want nil", td)
	}

	// On the wire: encoding/json must emit JSON null, not omit the field.
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"turn_detection":null`) {
		t.Fatalf("wire payload missing turn_detection:null, got %s", string(raw))
	}
}

// TestLemonadeTransportSessionUpdateOmitsZeroVADFields verifies that zero
// values for the three turn_detection knobs cause vocis to omit them,
// letting Lemonade apply its own server-VAD defaults. When ALL three
// are zero, the turn_detection key is absent entirely.
func TestLemonadeTransportSessionUpdateOmitsZeroVADFields(t *testing.T) {
	t.Parallel()

	t.Run("all zero omits turn_detection", func(t *testing.T) {
		transport := newLemonadeTransport(
			config.TranscriptionConfig{Backend: config.BackendLemonade, Model: "m"},
			config.StreamingConfig{ManualCommit: false},
			time.Second,
		)
		session := transport.SessionUpdate()["session"].(map[string]any)
		if _, present := session["turn_detection"]; present {
			t.Fatalf("turn_detection key must be omitted when all knobs are zero, got %v", session["turn_detection"])
		}
	})

	t.Run("partial override keeps only set fields", func(t *testing.T) {
		transport := newLemonadeTransport(
			config.TranscriptionConfig{Backend: config.BackendLemonade, Model: "m"},
			config.StreamingConfig{ManualCommit: false, Threshold: 0.03},
			time.Second,
		)
		session := transport.SessionUpdate()["session"].(map[string]any)
		td, ok := session["turn_detection"].(map[string]any)
		if !ok {
			t.Fatalf("turn_detection should be a map when one field is set, got %T", session["turn_detection"])
		}
		if td["threshold"] != 0.03 {
			t.Fatalf("threshold = %v, want 0.03", td["threshold"])
		}
		if _, present := td["silence_duration_ms"]; present {
			t.Fatalf("silence_duration_ms must be omitted when zero")
		}
		if _, present := td["prefix_padding_ms"]; present {
			t.Fatalf("prefix_padding_ms must be omitted when zero")
		}
	})
}

// TestLemonadeTransportSessionUpdateCarriesPromptAndNoiseReduction pins
// that prompt_hint and streaming.noise_reduction land in the flat
// session.update payload at the right keys, so a future Lemonade schema
// addition for either field works without a client change.
func TestLemonadeTransportSessionUpdateCarriesPromptAndNoiseReduction(t *testing.T) {
	t.Parallel()

	transport := newLemonadeTransport(
		config.TranscriptionConfig{
			Backend:    config.BackendLemonade,
			Model:      "m",
			PromptHint: "Use technical spelling.",
		},
		config.StreamingConfig{NoiseReduction: "near_field"},
		time.Second,
	)
	session := transport.SessionUpdate()["session"].(map[string]any)

	if got := session["prompt"]; got != "Use technical spelling." {
		t.Fatalf("prompt = %v, want \"Use technical spelling.\"", got)
	}
	nr, ok := session["noise_reduction"].(map[string]any)
	if !ok {
		t.Fatalf("noise_reduction = %T, want map[string]any", session["noise_reduction"])
	}
	if nr["type"] != "near_field" {
		t.Fatalf("noise_reduction.type = %v, want near_field", nr["type"])
	}
}

func TestLemonadeTransportSampleRate(t *testing.T) {
	t.Parallel()
	transport := newLemonadeTransport(config.TranscriptionConfig{Backend: config.BackendLemonade}, config.StreamingConfig{}, time.Second)
	if got := transport.SampleRate(); got != 16000 {
		t.Fatalf("SampleRate = %d, want 16000", got)
	}
}

// TestClientLemonadeBackendDialsWithoutClientSecret stands up a fake Lemonade
// WS server (no /realtime/client_secrets endpoint, no auth header expected),
// constructs a Client with backend=lemonade, and verifies StartStream connects
// successfully and sends the lemonade-shaped session.update payload.
func TestClientLemonadeBackendDialsWithoutClientSecret(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}

	var (
		sawAuth      string
		sawModelArg  string
		gotSessionUp map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realtime" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		sawModelArg = r.URL.Query().Get("model")

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "session.created"}); err != nil {
			t.Fatalf("write created: %v", err)
		}
		if err := conn.ReadJSON(&gotSessionUp); err != nil {
			t.Fatalf("read session update: %v", err)
		}
	}))
	defer server.Close()

	// httptest.NewServer returns an http://127.0.0.1:PORT URL — feed it as
	// the lemonade RealtimeURL so the transport upgrades it to ws://.
	cfg := config.Default()
	cfg.Transcription.Backend = config.BackendLemonade
	cfg.Transcription.RealtimeURL = server.URL
	cfg.Transcription.Model = "Whisper-Tiny"
	cfg.Transcription.BaseURL = server.URL // not used for transcribe but keeps SDK happy

	client := New(cfg.Transcription, cfg.Streaming)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := client.StartStream(ctx, 16000, 1)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer stream.Close()

	// Wait for the server to read the session.update payload.
	deadline := time.Now().Add(2 * time.Second)
	for gotSessionUp == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if sawAuth != "" {
		t.Fatalf("Authorization header = %q, want empty (lemonade has no auth)", sawAuth)
	}
	if sawModelArg != "Whisper-Tiny" {
		t.Fatalf("model query arg = %q, want Whisper-Tiny", sawModelArg)
	}
	if gotSessionUp == nil {
		t.Fatal("server never received session.update")
	}
	if got := gotSessionUp["type"]; got != "session.update" {
		t.Fatalf("session update type = %v", got)
	}
	session, _ := gotSessionUp["session"].(map[string]any)
	if got := session["model"]; got != "Whisper-Tiny" {
		t.Fatalf("session.model = %v", got)
	}
	if _, hasAudio := session["audio"]; hasAudio {
		raw, _ := json.Marshal(gotSessionUp)
		t.Fatalf("lemonade session.update should be flat, got %s", string(raw))
	}
	if !strings.HasPrefix(client.transport.(*lemonadeTransport).rawURL, "http") {
		// sanity check on the test wiring
		t.Fatalf("rawURL = %q", client.transport.(*lemonadeTransport).rawURL)
	}
}
