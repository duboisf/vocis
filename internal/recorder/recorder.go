package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jfreymuth/pulse"

	"vtt/internal/config"
)

const (
	minRecordingDuration = 100 * time.Millisecond
	stopFlushDelay       = 120 * time.Millisecond
	wavHeaderSize        = 44
)

type Recorder struct{}

type Session struct {
	path      string
	client    *pulse.Client
	stream    *pulse.RecordStream
	file      *wavFile
	meter     *levelMeter
	startedAt time.Time
	once      sync.Once
	err       error
}

type wavFile struct {
	mu         sync.Mutex
	file       *os.File
	sampleRate int
	channels   int
	dataBytes  uint32
	closed     bool
}

type meteredWriter struct {
	dst   *wavFile
	meter *levelMeter
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
	path, err := tempRecordingPath()
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}

	wav, err := newWAVFile(file, cfg.SampleRate, cfg.Channels)
	if err != nil {
		file.Close()
		return nil, err
	}

	client, err := newPulseClient()
	if err != nil {
		wav.Close()
		os.Remove(path)
		return nil, err
	}

	options, err := recordOptions(client, cfg)
	if err != nil {
		client.Close()
		wav.Close()
		os.Remove(path)
		return nil, err
	}

	meter := &levelMeter{}
	stream, err := client.NewRecord(
		pulse.Int16Writer((&meteredWriter{dst: wav, meter: meter}).Write),
		options...,
	)
	if err != nil {
		client.Close()
		wav.Close()
		os.Remove(path)
		return nil, err
	}

	session := &Session{
		path:      path,
		client:    client,
		stream:    stream,
		file:      wav,
		meter:     meter,
		startedAt: time.Now(),
	}

	stream.Start()

	select {
	case <-ctx.Done():
		session.closeResources()
		session.Cleanup()
		return nil, ctx.Err()
	case <-time.After(80 * time.Millisecond):
	}

	return session, nil
}

func (s *Session) Path() string {
	return s.path
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

func (s *Session) Cleanup() {
	_ = os.Remove(s.path)
}

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

	return validRecording(s.path)
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
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			errs = append(errs, err)
		}
		s.file = nil
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

func tempRecordingPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "vtt")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}

	file, err := os.CreateTemp(path, "recording-*.wav")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	return name, nil
}

func validRecording(path string) error {
	duration, err := wavDuration(path)
	if err != nil {
		return err
	}
	if duration < minRecordingDuration {
		return fmt.Errorf("recording duration %s is too short", duration.Round(10*time.Millisecond))
	}
	return nil
}

func wavDuration(path string) (time.Duration, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	header := make([]byte, wavHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, errors.New("recording is empty")
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, errors.New("recording is not a WAV file")
	}
	if string(header[12:16]) != "fmt " || string(header[36:40]) != "data" {
		return 0, errors.New("recording header is invalid")
	}

	audioFormat := binary.LittleEndian.Uint16(header[20:22])
	channels := binary.LittleEndian.Uint16(header[22:24])
	sampleRate := binary.LittleEndian.Uint32(header[24:28])
	bitsPerSample := binary.LittleEndian.Uint16(header[34:36])
	dataBytes := binary.LittleEndian.Uint32(header[40:44])

	if audioFormat != 1 {
		return 0, fmt.Errorf("recording format %d is unsupported", audioFormat)
	}
	if channels == 0 || sampleRate == 0 || bitsPerSample == 0 {
		return 0, errors.New("recording header is invalid")
	}
	if dataBytes == 0 {
		return 0, errors.New("recording is empty")
	}

	bytesPerFrame := uint32(channels) * uint32(bitsPerSample/8)
	if bytesPerFrame == 0 {
		return 0, errors.New("recording header is invalid")
	}

	return time.Duration(dataBytes) * time.Second / time.Duration(bytesPerFrame*sampleRate), nil
}

func newWAVFile(file *os.File, sampleRate, channels int) (*wavFile, error) {
	if sampleRate <= 0 {
		return nil, errors.New("sample rate must be greater than zero")
	}
	if channels != 1 && channels != 2 {
		return nil, errors.New("recording.channels must be 1 or 2 for pulse capture")
	}

	wav := &wavFile{
		file:       file,
		sampleRate: sampleRate,
		channels:   channels,
	}
	if _, err := file.Write(wav.header()); err != nil {
		return nil, err
	}
	return wav, nil
}

func (f *wavFile) Write(samples []int16) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}

	buf := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}
	if _, err := f.file.Write(buf); err != nil {
		return 0, err
	}

	f.dataBytes += uint32(len(buf))
	return len(samples), nil
}

func (f *wavFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true

	if _, err := f.file.WriteAt(f.header(), 0); err != nil {
		_ = f.file.Close()
		return err
	}
	return f.file.Close()
}

func (w *meteredWriter) Write(samples []int16) (int, error) {
	if w.meter != nil {
		w.meter.Update(samples)
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

func (f *wavFile) header() []byte {
	header := make([]byte, wavHeaderSize)

	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+f.dataBytes)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(f.channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(f.sampleRate))

	blockAlign := uint16(f.channels * 2)
	byteRate := uint32(f.sampleRate) * uint32(blockAlign)
	binary.LittleEndian.PutUint32(header[28:32], byteRate)
	binary.LittleEndian.PutUint16(header[32:34], blockAlign)
	binary.LittleEndian.PutUint16(header[34:36], 16)

	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], f.dataBytes)
	return header
}
