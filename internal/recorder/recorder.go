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

	"vtt/internal/config"
)

const (
	minRecordingDuration = 100 * time.Millisecond
	stopFlushDelay       = 120 * time.Millisecond
)

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

// CleanupStale is kept for call-site compatibility. Streaming capture no longer
// creates temp audio files on disk.
func CleanupStale() {}

func (r *Recorder) Start(ctx context.Context, cfg config.RecordingConfig) (*Session, error) {
	client, err := newPulseClient()
	if err != nil {
		return nil, err
	}

	options, err := recordOptions(client, cfg)
	if err != nil {
		client.Close()
		return nil, err
	}

	session := &Session{
		client:     client,
		pipe:       newSamplePipe(),
		meter:      &levelMeter{},
		startedAt:  time.Now(),
		sampleRate: cfg.SampleRate,
		channels:   cfg.Channels,
	}

	stream, err := client.NewRecord(
		pulse.Int16Writer((&meteredWriter{
			dst:      session.pipe,
			meter:    session.meter,
			channels: cfg.Channels,
			frames:   &session.frames,
		}).Write),
		options...,
	)
	if err != nil {
		client.Close()
		session.pipe.Close()
		return nil, err
	}

	session.stream = stream
	stream.Start()

	select {
	case <-ctx.Done():
		session.closeResources()
		return nil, ctx.Err()
	case <-time.After(80 * time.Millisecond):
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
		s.err = s.stop(ctx)
	})
	return s.err
}

func (s *Session) Level() float64 {
	if s == nil || s.meter == nil {
		return 0
	}
	return s.meter.Level()
}

func (s *Session) Cleanup() {}

func (s *Session) stop(ctx context.Context) error {
	if s.stream != nil {
		s.stream.Stop()
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
	closeErr := s.closeResources()
	if streamErr != nil {
		return fmt.Errorf("record audio: %w", streamErr)
	}
	if closeErr != nil {
		return closeErr
	}

	return validRecordingDuration(s.Duration())
}

func (s *Session) closeResources() error {
	var errs []error

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

	return errors.Join(errs...)
}

func recordOptions(client *pulse.Client, cfg config.RecordingConfig) ([]pulse.RecordOption, error) {
	options := []pulse.RecordOption{
		pulse.RecordSampleRate(cfg.SampleRate),
		pulse.RecordMediaName("vtt dictation"),
		pulse.RecordLatency(0.05),
	}

	switch cfg.Channels {
	case 1:
		options = append(options, pulse.RecordMono)
	case 2:
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
		pulse.ClientApplicationName("vtt"),
		pulse.ClientApplicationIconName("audio-input-microphone"),
	)
}

func validRecordingDuration(duration time.Duration) error {
	if duration < minRecordingDuration {
		return fmt.Errorf("recording duration %s is too short", duration.Round(10*time.Millisecond))
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
