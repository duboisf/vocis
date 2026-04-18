package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event is a single WS frame crossing the session boundary, used for
// human-readable logging. Direction is "→" for outbound, "←" for
// inbound.
type Event struct {
	At        time.Time
	Direction string
	Type      string
	Payload   map[string]any
	Note      string
}

// Config configures a Session at construction time. Required field is
// URL; everything else has a sensible zero value.
type Config struct {
	URL        string    // realtime WS endpoint (ws://host:port/realtime?model=...)
	Model      string    // model id passed in session.update; empty omits the field
	VADms      int       // turn_detection.silence_duration_ms; -1 disables turn_detection
	Log        io.Writer // event log destination; nil → silent
	Transcript io.Writer // receives each turn's text the moment `completed` arrives (one line per turn); nil → silent
	Debug      bool      // dump raw JSON for every WS frame
}

// StreamOpts controls a single Stream() call. Samples is mono PCM16
// at the given Rate; the session resamples to 16 kHz before sending.
type StreamOpts struct {
	Samples    []int16
	Rate       int
	PadMs      int  // ms of silence to append after the audio (helps Lemonade VAD trigger)
	PauseMs    int  // sleep before sending input_audio_buffer.commit
	SkipCommit bool // omit commit, rely entirely on server VAD auto-commit
}

// Session runs one realtime probe end-to-end.
//
//	s := New(Config{URL: ws, Model: m, VADms: 500, Log: os.Stdout})
//	if err := s.Start(ctx); err != nil { ... }
//	defer s.Close()
//	if err := s.Stream(StreamOpts{Samples: pcm, Rate: 16000}); err != nil { ... }
//	result, err := s.Collect(ctx, 3*time.Second)
type Session struct {
	cfg Config

	start time.Time

	conn       *websocket.Conn
	eventsDone chan struct{}

	mu     sync.Mutex
	result Result // accumulated state — snapshot() returns a copy
}

// Result is the post-Collect summary across the whole session.
type Result struct {
	Turns        []string
	FirstDelta   string
	CommitAt     time.Time
	FirstDeltaAt time.Time
	LastEventAt  time.Time

	// TurnsStarted / CommitAcked drive Collect's exit condition.
	// Collect returns once CommitAcked is true AND len(Turns) >=
	// TurnsStarted — i.e. every speech_started has a matching
	// completed. No more arbitrary idle polling.
	TurnsStarted int
	CommitAcked  bool
}

func (r Result) FullTranscript() string {
	return joinTurns(r.Turns)
}

func (r Result) Summary() string {
	if r.CommitAt.IsZero() {
		return fmt.Sprintf("turns=%d/%d  total_chars=%d", len(r.Turns), r.TurnsStarted, len(r.FullTranscript()))
	}
	toFirstDelta := "-"
	if !r.FirstDeltaAt.IsZero() {
		toFirstDelta = fmt.Sprintf("%dms", r.FirstDeltaAt.Sub(r.CommitAt).Milliseconds())
	}
	toLastEvent := "-"
	if !r.LastEventAt.IsZero() {
		toLastEvent = fmt.Sprintf("%dms", r.LastEventAt.Sub(r.CommitAt).Milliseconds())
	}
	return fmt.Sprintf("turns=%d/%d  total_chars=%d  commit→first_delta=%s  commit→last_event=%s",
		len(r.Turns), r.TurnsStarted, len(r.FullTranscript()), toFirstDelta, toLastEvent)
}

// New returns a configured session. Nothing opens until Start().
func New(cfg Config) *Session { return &Session{cfg: cfg} }

// Start dials the WS, sends session.update, and spins up the reader
// goroutine. Must be called before Stream/Collect.
func (s *Session) Start(ctx context.Context) error {
	s.start = time.Now()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, s.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", s.cfg.URL, err)
	}
	s.conn = conn
	s.eventsDone = make(chan struct{})
	go s.readEvents()

	session := map[string]any{}
	if s.cfg.Model != "" {
		session["model"] = s.cfg.Model
	}
	if s.cfg.VADms >= 0 {
		session["turn_detection"] = map[string]any{
			"threshold":           0.01,
			"silence_duration_ms": s.cfg.VADms,
			"prefix_padding_ms":   300,
		}
	}
	return s.sendOutbound("session.update", map[string]any{
		"type":    "session.update",
		"session": session,
	})
}

// Stream sends the audio to Lemonade: resamples to 16 kHz, appends
// optional silence padding, sleeps, then sends input_audio_buffer.commit
// (unless SkipCommit is true).
func (s *Session) Stream(opts StreamOpts) error {
	chunked := resample(opts.Samples, opts.Rate, 16000)
	if err := s.streamChunks(chunked, "audio"); err != nil {
		return err
	}
	if opts.PadMs > 0 {
		pad := make([]int16, 16000*opts.PadMs/1000)
		if err := s.streamChunks(pad, "pad_silence"); err != nil {
			return err
		}
	}
	if opts.PauseMs > 0 {
		time.Sleep(time.Duration(opts.PauseMs) * time.Millisecond)
	}
	if opts.SkipCommit {
		s.emit(Event{At: time.Now(), Direction: "→", Type: "commit.skipped"})
		return nil
	}
	s.mu.Lock()
	s.result.CommitAt = time.Now()
	s.mu.Unlock()
	return s.sendOutbound("input_audio_buffer.commit", map[string]any{
		"type": "input_audio_buffer.commit",
	})
}

// Collect blocks until Lemonade has finished transcribing every turn
// in the audio we sent: waits for the `committed` ack, then for as
// many `completed` events as we saw `speech_started` events. Returns
// early if `maxWait` elapses (safety — a silent turn would otherwise
// hang the probe).
func (s *Session) Collect(ctx context.Context, maxWait time.Duration) (Result, error) {
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return s.snapshot(), ctx.Err()
		case <-s.eventsDone:
			return s.snapshot(), nil
		case <-ticker.C:
			snap := s.snapshot()
			if snap.CommitAcked && len(snap.Turns) >= snap.TurnsStarted {
				return snap, nil
			}
			if time.Now().After(deadline) {
				return snap, fmt.Errorf("collect: timed out after %s (turns=%d/%d, committed=%v)",
					maxWait, len(snap.Turns), snap.TurnsStarted, snap.CommitAcked)
			}
		}
	}
}

// Close tears down the WS. Safe to call multiple times.
func (s *Session) Close() error {
	if s.conn != nil {
		_ = s.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// internals — keep below here to keep the public API section readable
// ---------------------------------------------------------------------------

// snapshot returns a copy of the accumulated result. The Turns slice
// is cloned so the caller can hold it past further mutations.
func (s *Session) snapshot() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.result
	out.Turns = append([]string(nil), s.result.Turns...)
	return out
}

func (s *Session) streamChunks(samples []int16, label string) error {
	const chunkMs = 50
	chunkSamples := 16000 * chunkMs / 1000
	total := len(samples) * 2
	s.emit(Event{
		At:        time.Now(),
		Direction: "→",
		Type:      "input_audio_buffer.append." + label,
		Note:      fmt.Sprintf("%d bytes in %dms chunks", total, chunkMs),
	})
	for offset := 0; offset < len(samples); offset += chunkSamples {
		end := offset + chunkSamples
		if end > len(samples) {
			end = len(samples)
		}
		chunk := samplesToBytes(samples[offset:end])
		b64 := base64.StdEncoding.EncodeToString(chunk)
		if err := s.conn.WriteJSON(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": b64,
		}); err != nil {
			return fmt.Errorf("append %s: %w", label, err)
		}
		time.Sleep(chunkMs * time.Millisecond)
	}
	return nil
}

func (s *Session) sendOutbound(kind string, payload any) error {
	raw, _ := json.Marshal(payload)
	note := ""
	if s.cfg.Debug {
		note = string(raw)
	}
	s.emit(Event{
		At:        time.Now(),
		Direction: "→",
		Type:      kind,
		Note:      note,
	})
	return s.conn.WriteJSON(payload)
}

func (s *Session) readEvents() {
	defer close(s.eventsDone)
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		now := time.Now()
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.emit(Event{At: now, Direction: "←", Type: "decode_error", Note: err.Error()})
			continue
		}
		t, _ := msg["type"].(string)
		s.noteInbound(now, t, msg)
		note := inboundExtra(t, msg)
		if s.cfg.Debug {
			// Dump the raw JSON exactly as it landed off the wire —
			// useful for spec-vs-reality diffs (Lemonade re-sends the
			// full partial in every delta event, etc.).
			note = string(raw)
		}
		s.emit(Event{At: now, Direction: "←", Type: t, Payload: msg, Note: note})
	}
}

func (s *Session) noteInbound(now time.Time, kind string, msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.LastEventAt = now

	switch kind {
	case "input_audio_buffer.speech_started":
		s.result.TurnsStarted++
	case "input_audio_buffer.committed":
		s.result.CommitAcked = true
	case "conversation.item.input_audio_transcription.delta":
		if d, _ := msg["delta"].(string); d != "" && s.result.FirstDelta == "" {
			s.result.FirstDelta = d
			s.result.FirstDeltaAt = now
		}
	case "conversation.item.input_audio_transcription.completed":
		if tr, _ := msg["transcript"].(string); tr != "" {
			s.result.Turns = append(s.result.Turns, tr)
			// Stream the turn out as it lands — caller can pipe stdout
			// to a file/tee while the session is still running.
			if s.cfg.Transcript != nil {
				fmt.Fprintln(s.cfg.Transcript, strings.TrimSpace(tr))
			}
		}
	}
}

// emit writes one human-readable line to the session's log destination.
// Format: "NNNNms  → type  extra". Matches the pre-rework probe so
// existing eyeball-parsing habits still work.
func (s *Session) emit(ev Event) {
	if s.cfg.Log == nil {
		return
	}
	elapsed := ev.At.Sub(s.start).Milliseconds()
	if ev.Note == "" {
		fmt.Fprintf(s.cfg.Log, "%7dms  %s  %s\n", elapsed, ev.Direction, ev.Type)
		return
	}
	fmt.Fprintf(s.cfg.Log, "%7dms  %s  %s  %s\n", elapsed, ev.Direction, ev.Type, ev.Note)
}

func inboundExtra(kind string, msg map[string]any) string {
	switch kind {
	case "conversation.item.input_audio_transcription.delta":
		if d, _ := msg["delta"].(string); d != "" {
			return fmt.Sprintf("delta=%q", d)
		}
	case "conversation.item.input_audio_transcription.completed":
		if tr, _ := msg["transcript"].(string); tr != "" {
			return fmt.Sprintf("transcript=%q", tr)
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func joinTurns(turns []string) string {
	out := ""
	for _, t := range turns {
		if out != "" && needsSpace(out, t) {
			out += " "
		}
		out += t
	}
	return out
}

func needsSpace(prev, next string) bool {
	if prev == "" || next == "" {
		return false
	}
	last := prev[len(prev)-1]
	first := next[0]
	if last == ' ' || first == ' ' {
		return false
	}
	return true
}
