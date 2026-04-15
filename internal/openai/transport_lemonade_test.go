package openai

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
			cfg := config.OpenAIConfig{
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

	cfg := config.OpenAIConfig{
		Backend:  config.BackendLemonade,
		Model:    "Whisper-Tiny",
		Language: "en",
	}
	transport := newLemonadeTransport(cfg, config.StreamingConfig{}, time.Second)

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
}

func TestLemonadeTransportSampleRate(t *testing.T) {
	t.Parallel()
	transport := newLemonadeTransport(config.OpenAIConfig{Backend: config.BackendLemonade}, config.StreamingConfig{}, time.Second)
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
	cfg.OpenAI.Backend = config.BackendLemonade
	cfg.OpenAI.RealtimeURL = server.URL
	cfg.OpenAI.Model = "Whisper-Tiny"
	cfg.OpenAI.BaseURL = server.URL // not used for transcribe but keeps SDK happy

	client := New("", cfg.OpenAI, cfg.Streaming)
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
