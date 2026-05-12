package recorder

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfreymuth/pulse"
	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/config"
	"vocis/internal/telemetry"
)

const (
	minRecordingDuration = 100 * time.Millisecond
	stopFlushDelay       = 120 * time.Millisecond

	// SampleRate is the only sample rate the rest of the pipeline
	// understands: Silero VAD is hard-wired to 16 kHz, and Lemonade's
	// chat-audio path expects 16 kHz PCM. Used to be a YAML knob
	// (`recording.sample_rate`); pinned here once the value never
	// successfully diverged from this in practice.
	SampleRate = 16000
	// Channels is fixed at mono. Lemonade's chat-audio errors on
	// non-mono input.
	Channels = 1
	// DefaultMaxDurationSeconds is the per-session ceiling. Used by
	// app.go's force-stop timer; was `recording.max_duration_seconds`.
	DefaultMaxDurationSeconds = 120
)

// The recording backend used to be a YAML knob (`recording.backend`,
// "auto" | "pulse") but auto-detection already covered every case in
// practice. The only backend implemented in this package is the
// PulseAudio client below.

var ErrRecordingTooShort = errors.New("recording too short")

type Recorder struct{}

type Session struct {
	client     *pulse.Client
	stream     *pulse.RecordStream
	pipe       *samplePipe
	meter      *levelMeter
	startedAt  time.Time
	sampleRate int
	channels   int

	frames atomic.Int64
	once   sync.Once
	err    error
}

type samplePipe struct {
	mu     sync.Mutex
	ch     chan []int16
	closed bool
}

type meteredWriter struct {
	dst      *samplePipe
	meter    *levelMeter
	channels int
	frames   *atomic.Int64
}

type levelMeter struct {
	mu        sync.Mutex
	level     float64
	updatedAt time.Time
}

func New() *Recorder {
	return &Recorder{}
}

func Check() error {
	client, err := newPulseClient()
	if err != nil {
		return err
	}
	defer client.Close()

	_, err = client.DefaultSource()
	return err
}

func (r *Recorder) Start(ctx context.Context, cfg config.RecordingConfig) (*Session, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.recorder.start",
		attribute.Int("audio.sample_rate", SampleRate),
		attribute.Int("audio.channels", Channels),
		attribute.String("audio.device", cfg.Device),
	)
	defer func() { telemetry.EndSpan(span, nil) }()

	client, err := newPulseClient()
	if err != nil {
		telemetry.EndSpan(span, err)
		return nil, err
	}

	options, err := recordOptions(client, cfg)
	if err != nil {
		client.Close()
		telemetry.EndSpan(span, err)
		return nil, err
	}

	session := &Session{
		client:     client,
		pipe:       newSamplePipe(),
		meter:      &levelMeter{},
		startedAt:  time.Now(),
		sampleRate: SampleRate,
		channels:   Channels,
	}

	stream, err := client.NewRecord(
		pulse.Int16Writer((&meteredWriter{
			dst:      session.pipe,
			meter:    session.meter,
			channels: Channels,
			frames:   &session.frames,
		}).Write),
		options...,
	)
	if err != nil {
		client.Close()
		session.pipe.Close()
		telemetry.EndSpan(span, err)
		return nil, err
	}

	session.stream = stream
	stream.Start()
	select {
	case <-ctx.Done():
		session.closeResources()
		telemetry.EndSpan(span, ctx.Err())
		return nil, ctx.Err()
	default:
	}

	return session, nil
}

func (s *Session) Samples() <-chan []int16 {
	if s == nil || s.pipe == nil {
		return nil
	}
	return s.pipe.Chan()
}

func (s *Session) SampleRate() int {
	if s == nil {
		return 0
	}
	return s.sampleRate
}

func (s *Session) Channels() int {
	if s == nil {
		return 0
	}
	return s.channels
}

func (s *Session) Duration() time.Duration {
	if s == nil || s.sampleRate <= 0 {
		return 0
	}
	frames := s.frames.Load()
	if frames <= 0 {
		return 0
	}
	return time.Duration(frames) * time.Second / time.Duration(s.sampleRate)
}

func (s *Session) BytesCaptured() int64 {
	if s == nil || s.channels <= 0 {
		return 0
	}
	return s.frames.Load() * int64(s.channels) * 2
}

func (s *Session) Stop(ctx context.Context) error {
	s.once.Do(func() {
		_, span := telemetry.StartSpan(ctx, "vocis.recorder.stop")
		s.err = s.stop(ctx)
		span.SetAttributes(
			attribute.Int64("recording.bytes", s.BytesCaptured()),
			attribute.String("recording.duration", s.Duration().Round(10*time.Millisecond).String()),
		)
		telemetry.EndSpan(span, s.err)
	})
	return s.err
}

func (s *Session) Level() float64 {
	if s == nil || s.meter == nil {
		return 0
	}
	return s.meter.Level()
}

func (s *Session) stop(ctx context.Context) error {
	if s.stream != nil {
		done := make(chan struct{})
		go func() {
			s.stream.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			s.closeResources()
			return ctx.Err()
		}
	}

	timer := time.NewTimer(stopFlushDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		s.closeResources()
		return ctx.Err()
	case <-timer.C:
	}

	streamErr := error(nil)
	if s.stream != nil {
		streamErr = s.stream.Error()
	}
	s.closeResources()
	if streamErr != nil {
		return fmt.Errorf("record audio: %w", streamErr)
	}

	return validRecordingDuration(s.Duration())
}

func (s *Session) closeResources() {
	if s.stream != nil {
		s.stream.Close()
		s.stream = nil
	}
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	if s.pipe != nil {
		s.pipe.Close()
	}
}

func recordOptions(client *pulse.Client, cfg config.RecordingConfig) ([]pulse.RecordOption, error) {
	options := []pulse.RecordOption{
		pulse.RecordSampleRate(SampleRate),
		pulse.RecordMediaName("vocis dictation"),
		pulse.RecordLatency(0.05),
	}

	// Channels is pinned to 1 (mono) at the package level; no switch
	// needed but kept here as a guard for any future override.
	if Channels == 1 {
		options = append(options, pulse.RecordMono)
	} else if Channels == 2 {
		options = append(options, pulse.RecordStereo)
	}

	source, err := resolveSource(client, cfg.Device)
	if err != nil {
		return nil, fmt.Errorf("recording device %q: %w", strings.TrimSpace(cfg.Device), err)
	}
	if source != nil {
		options = append(options, pulse.RecordSource(source))
	}

	return options, nil
}

func resolveSource(client *pulse.Client, device string) (*pulse.Source, error) {
	switch strings.TrimSpace(device) {
	case "", "default":
		return nil, nil
	default:
		return client.SourceByID(device)
	}
}

func newPulseClient() (*pulse.Client, error) {
	return pulse.NewClient(
		pulse.ClientApplicationName("vocis"),
		pulse.ClientApplicationIconName("audio-input-microphone"),
	)
}

func validRecordingDuration(duration time.Duration) error {
	if duration < minRecordingDuration {
		return fmt.Errorf("%w: %s", ErrRecordingTooShort, duration.Round(10*time.Millisecond))
	}
	return nil
}

func newSamplePipe() *samplePipe {
	return &samplePipe{
		ch: make(chan []int16, 16),
	}
}

func (p *samplePipe) Write(samples []int16) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, errors.New("sample pipe closed")
	}

	chunk := append([]int16(nil), samples...)
	p.ch <- chunk
	return len(samples), nil
}

func (p *samplePipe) Chan() <-chan []int16 {
	return p.ch
}

func (p *samplePipe) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	close(p.ch)
}

func (w *meteredWriter) Write(samples []int16) (int, error) {
	if w.meter != nil {
		w.meter.Update(samples)
	}
	if w.frames != nil && w.channels > 0 {
		w.frames.Add(int64(len(samples) / w.channels))
	}
	return w.dst.Write(samples)
}

func (m *levelMeter) Update(samples []int16) {
	if len(samples) == 0 {
		return
	}

	var peak float64
	for _, sample := range samples {
		value := math.Abs(float64(sample)) / 32768.0
		if value > peak {
			peak = value
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if peak > m.level {
		m.level = peak
	} else {
		m.level = m.level*0.65 + peak*0.35
	}
	m.updatedAt = time.Now()
}

func (m *levelMeter) Level() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.updatedAt.IsZero() {
		return 0
	}

	age := time.Since(m.updatedAt)
	if age >= 220*time.Millisecond {
		return 0
	}

	level := m.level * (1 - float64(age)/(220*float64(time.Millisecond)))
	if level < 0 {
		return 0
	}
	if level > 1 {
		return 1
	}
	return level
}
