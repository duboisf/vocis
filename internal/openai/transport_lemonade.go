package openai

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"vocis/internal/config"
)

const (
	lemonadeSampleRate        = 16000
	lemonadeDefaultRealtimeURL = "ws://localhost:9000"
)

type lemonadeTransport struct {
	cfg       config.OpenAIConfig
	streaming config.StreamingConfig
	dialer    websocket.Dialer
	rawURL    string
}

func newLemonadeTransport(
	cfg config.OpenAIConfig,
	streaming config.StreamingConfig,
	timeout time.Duration,
) *lemonadeTransport {
	raw := strings.TrimRight(cfg.RealtimeURL, "/")
	if raw == "" {
		raw = lemonadeDefaultRealtimeURL
	}
	return &lemonadeTransport{
		cfg:       cfg,
		streaming: streaming,
		dialer:    websocket.Dialer{HandshakeTimeout: minDuration(timeout, 5*time.Second)},
		rawURL:    raw,
	}
}

func (t *lemonadeTransport) SampleRate() int { return lemonadeSampleRate }

func (t *lemonadeTransport) Dial(ctx context.Context) (*websocket.Conn, error) {
	wsURL, err := t.buildURL()
	if err != nil {
		return nil, err
	}
	conn, resp, err := t.dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, formatDialError(err, resp)
	}
	return conn, nil
}

// buildURL converts the configured base into a Lemonade realtime WS URL of the
// form ws://host:port/realtime?model=<model>. Accepts http(s)/ws(s) schemes
// and adds /realtime + ?model= when missing.
func (t *lemonadeTransport) buildURL() (string, error) {
	u, err := url.Parse(t.rawURL)
	if err != nil {
		return "", fmt.Errorf("parse lemonade realtime_url %q: %w", t.rawURL, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("lemonade realtime_url must use ws, wss, http, or https: %q", t.rawURL)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/realtime"
	}
	q := u.Query()
	if q.Get("model") == "" && strings.TrimSpace(t.cfg.Model) != "" {
		q.Set("model", t.cfg.Model)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (t *lemonadeTransport) SessionUpdate() map[string]any {
	session := map[string]any{
		"model": t.cfg.Model,
	}
	if language := strings.TrimSpace(t.cfg.Language); language != "" {
		session["language"] = language
	}
	return map[string]any{
		"type":    "session.update",
		"session": session,
	}
}
