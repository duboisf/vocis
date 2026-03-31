package openai

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/realtime"

	"vtt/internal/config"
	"vtt/internal/sessionlog"
)

const (
	defaultBaseURL   = "https://api.openai.com/v1"
	streamSampleRate = 24000
)

var ErrInputAudioBufferCommitEmpty = errors.New("input audio buffer commit empty")

type Client struct {
	cfg          config.OpenAIConfig
	streaming    config.StreamingConfig
	client       openaisdk.Client
	dialer       websocket.Dialer
	websocketURL string
	writeTimeout time.Duration
}

type StreamEventType string

const (
	StreamEventPartial StreamEventType = "partial"
	StreamEventFinal   StreamEventType = "final"
	StreamEventError   StreamEventType = "error"
)

type StreamEvent struct {
	Type StreamEventType
	Text string
	Err  error
}

type Stream struct {
	conn         *websocket.Conn
	encoder      *pcmEncoder
	writeTimeout time.Duration

	writeMu   sync.Mutex
	closeOnce sync.Once
	readyOnce sync.Once

	readyCh  chan error
	events   chan StreamEvent
	readDone chan struct{}

	partialMu sync.Mutex
	partial   string
	activeID  string
	partials  map[string]string
}

type realtimeEvent struct {
	Type string `json:"type"`
}

type realtimeReadyEvent struct {
	Type string `json:"type"`
}

type transcriptionDeltaEvent struct {
	Type   string `json:"type"`
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`
}

type transcriptionCompletedEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Transcript string `json:"transcript"`
}

type transcriptionFailedEvent struct {
	Type  string `json:"type"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
		Type    string `json:"type"`
	} `json:"error"`
}

type realtimeErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		EventID string `json:"event_id"`
		Type    string `json:"type"`
	} `json:"error"`
}

type pcmEncoder struct {
	inputRate int64
	channels  int
	accum     int64
}

const defaultProgrammerPrompt = "Transcribe for a programmer speaking naturally. " +
	"Prefer common software, terminal, API, Git, and GitHub terminology. Preserve " +
	"obvious technical terms, acronyms, and capitalization when the audio supports " +
	"them. Do not invent extra words that were not spoken."

func New(apiKey string, cfg config.OpenAIConfig, streaming config.StreamingConfig) *Client {
	timeout := time.Duration(cfg.RequestLimit) * time.Second

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if baseURL != defaultBaseURL {
		sessionlog.Warnf("openai base_url is non-default: %s", baseURL)
	}

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(timeout),
	}
	if org := organization(cfg); org != "" {
		opts = append(opts, option.WithOrganization(org))
	}
	if project := project(cfg); project != "" {
		opts = append(opts, option.WithProject(project))
	}

	return &Client{
		cfg:       cfg,
		streaming: streaming,
		client:    openaisdk.NewClient(opts...),
		dialer: websocket.Dialer{
			HandshakeTimeout: minDuration(timeout, 10*time.Second),
		},
		websocketURL: realtimeWSURL(baseURL),
		writeTimeout: minDuration(timeout, 15*time.Second),
	}
}

func (c *Client) StartStream(ctx context.Context, sampleRate, channels int) (*Stream, error) {
	if sampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}

	secret, err := c.createClientSecret(ctx)
	if err != nil {
		return nil, err
	}

	headers := http.Header{
		"Authorization": []string{"Bearer " + secret},
	}
	conn, resp, err := c.dialer.DialContext(ctx, c.websocketURL, headers)
	if err != nil {
		return nil, formatDialError(err, resp)
	}

	stream := &Stream{
		conn:         conn,
		encoder:      newPCMEncoder(sampleRate, channels),
		writeTimeout: c.writeTimeout,
		readyCh:      make(chan error, 1),
		events:       make(chan StreamEvent, 16),
		readDone:     make(chan struct{}),
	}
	go stream.readLoop()

	if err := stream.sendJSON(ctx, c.sessionUpdateEvent()); err != nil {
		stream.Close()
		return nil, err
	}

	return stream, nil
}

func (s *Stream) Append(ctx context.Context, samples []int16) error {
	payload := s.encoder.Encode(samples)
	if len(payload) == 0 {
		return nil
	}

	return s.sendJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(payload),
	})
}

func (s *Stream) Commit(ctx context.Context) error {
	if err := s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return err
	}
	return nil
}

func (s *Stream) Partial() string {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()
	return s.partial
}

func (s *Stream) Events() <-chan StreamEvent {
	return s.events
}

func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		_ = s.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(2*time.Second),
		)
		err = s.conn.Close()
	})
	return err
}

func (s *Stream) waitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.readyCh:
		return err
	case <-s.readDone:
		return errors.New("openai realtime stream closed before session became ready")
	}
}

func (s *Stream) sendJSON(ctx context.Context, payload any) error {
	deadline := time.Now().Add(s.writeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := s.conn.WriteJSON(payload); err != nil {
		return err
	}
	return nil
}

func (s *Stream) readLoop() {
	defer close(s.readDone)
	defer close(s.events)

	for {
		var raw jsonMessage
		if err := s.conn.ReadJSON(&raw); err != nil {
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		}

		switch raw.Type() {
		case "session.created", "session.updated":
			s.markReady(nil)
		case "conversation.item.input_audio_transcription.delta":
			var event transcriptionDeltaEvent
			if err := raw.decode(&event); err == nil && event.Delta != "" {
				partial := s.appendPartial(event.ItemID, event.Delta)
				s.emitPartial(StreamEvent{Type: StreamEventPartial, Text: partial})
			}
		case "conversation.item.input_audio_transcription.completed":
			var event transcriptionCompletedEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			final, clearPartial := s.completePartial(event.ItemID, event.Transcript)
			if clearPartial {
				s.emitPartial(StreamEvent{Type: StreamEventPartial, Text: ""})
			}
			s.emit(StreamEvent{Type: StreamEventFinal, Text: final})
		case "conversation.item.input_audio_transcription.failed":
			var event transcriptionFailedEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			s.emit(StreamEvent{
				Type: StreamEventError,
				Err:  formatRealtimeError(event.Error.Code, event.Error.Message),
			})
			return
		case "error":
			var event realtimeErrorEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			err := formatRealtimeError(event.Error.Code, event.Error.Message)
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		}
	}
}

func (s *Stream) markReady(err error) {
	s.readyOnce.Do(func() {
		s.readyCh <- err
	})
}

func (s *Stream) emit(event StreamEvent) {
	s.events <- event
}

func (s *Stream) emitPartial(event StreamEvent) {
	select {
	case s.events <- event:
	default:
	}
}

func (s *Stream) appendPartial(itemID, delta string) string {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()

	if itemID == "" {
		s.partial += delta
		return s.partial
	}
	if s.partials == nil {
		s.partials = make(map[string]string)
	}
	s.partials[itemID] += delta
	s.activeID = itemID
	s.partial = s.partials[itemID]
	return s.partial
}

func (s *Stream) completePartial(itemID, transcript string) (string, bool) {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()

	tracked := s.partial
	clearPartial := true
	if itemID != "" {
		if s.partials != nil {
			if value, ok := s.partials[itemID]; ok {
				tracked = value
				delete(s.partials, itemID)
			}
		}
		clearPartial = s.activeID == itemID || s.activeID == ""
		if clearPartial {
			s.activeID = ""
			s.partial = ""
		}
	} else {
		s.activeID = ""
		s.partial = ""
	}

	return reconcileCompletedTranscript(tracked, transcript), clearPartial
}

func reconcileCompletedTranscript(partial, transcript string) string {
	partial = strings.TrimSpace(partial)
	transcript = strings.TrimSpace(transcript)

	switch {
	case partial == "":
		return transcript
	case transcript == "":
		return partial
	}

	if normalizedHasSuffix(partial, transcript) && len([]rune(partial)) > len([]rune(transcript)) {
		return partial
	}
	return transcript
}

func normalizedHasSuffix(full, suffix string) bool {
	full = normalizeText(full)
	suffix = normalizeText(suffix)
	if full == "" || suffix == "" {
		return false
	}
	return strings.HasSuffix(full, suffix)
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

func (c *Client) sessionUpdateEvent() map[string]any {
	transcription := map[string]any{
		"model": c.cfg.Model,
	}
	if language := strings.TrimSpace(c.cfg.Language); language != "" {
		transcription["language"] = language
	}
	if prompt := c.prompt(); prompt != "" {
		transcription["prompt"] = prompt
	}

	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{
						"type": "audio/pcm",
						"rate": 24000,
					},
					"transcription":  transcription,
					"turn_detection": c.turnDetectionPayload(),
				},
			},
		},
	}
}

func (c *Client) prompt() string {
	if hint := strings.TrimSpace(c.cfg.PromptHint); hint != "" {
		return hint
	}
	return defaultProgrammerPrompt
}

func newPCMEncoder(sampleRate, channels int) *pcmEncoder {
	return &pcmEncoder{
		inputRate: int64(sampleRate),
		channels:  channels,
	}
}

func (e *pcmEncoder) Encode(samples []int16) []byte {
	if e == nil || e.inputRate <= 0 || e.channels <= 0 {
		return nil
	}

	frames := len(samples) / e.channels
	if frames == 0 {
		return nil
	}

	estimate := int((int64(frames)*streamSampleRate)/e.inputRate + 2)
	out := make([]byte, 0, estimate*2)

	for frame := 0; frame < frames; frame++ {
		sample := mixToMono(samples[frame*e.channels : frame*e.channels+e.channels])
		e.accum += streamSampleRate
		for e.accum >= e.inputRate {
			out = binary.LittleEndian.AppendUint16(out, uint16(sample))
			e.accum -= e.inputRate
		}
	}

	return out
}

func mixToMono(frame []int16) int16 {
	if len(frame) == 0 {
		return 0
	}
	if len(frame) == 1 {
		return frame[0]
	}

	var total int
	for _, sample := range frame {
		total += int(sample)
	}
	return int16(total / len(frame))
}

func realtimeWSURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}

	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}

	u.Path = strings.TrimRight(u.Path, "/") + "/realtime"
	u.RawQuery = ""
	return u.String()
}

func formatDialError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	requestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("openai realtime connect: status %d%s: %s",
		resp.StatusCode,
		requestIDSuffix(requestID),
		message,
	)
}

func formatRealtimeError(code, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "realtime transcription failed"
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New(message)
	}
	if code == "input_audio_buffer_commit_empty" {
		return fmt.Errorf("%w: %s", ErrInputAudioBufferCommitEmpty, message)
	}
	return fmt.Errorf("%s (%s)", message, code)
}

func organization(cfg config.OpenAIConfig) string {
	if value := strings.TrimSpace(cfg.Organization); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_ORGANIZATION")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OPENAI_ORG_ID"))
}

func project(cfg config.OpenAIConfig) string {
	if value := strings.TrimSpace(cfg.Project); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_PROJECT")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OPENAI_PROJECT_ID"))
}

func requestIDSuffix(requestID string) string {
	if requestID == "" {
		return ""
	}
	return fmt.Sprintf(" (request id %s)", requestID)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

func (c *Client) createClientSecret(ctx context.Context) (string, error) {
	resp, err := c.client.Realtime.ClientSecrets.New(ctx, c.clientSecretParams())
	if err != nil {
		var apiErr *openaisdk.Error
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf(
				"openai realtime session: status %d%s: %s",
				apiErr.StatusCode,
				requestIDSuffix(requestID(apiErr)),
				strings.TrimSpace(apiErr.Message),
			)
		}
		return "", err
	}
	if strings.TrimSpace(resp.Value) == "" {
		return "", errors.New("openai realtime session did not return a client secret")
	}
	return strings.TrimSpace(resp.Value), nil
}

func (c *Client) clientSecretParams() realtime.ClientSecretNewParams {
	input := realtime.RealtimeTranscriptionSessionAudioInputParam{
		Format: realtime.RealtimeAudioFormatsUnionParam{
			OfAudioPCM: &realtime.RealtimeAudioFormatsAudioPCMParam{
				Type: "audio/pcm",
				Rate: 24000,
			},
		},
		Transcription: c.audioTranscriptionParam(),
	}
	if turnDetection := c.turnDetectionParam(); turnDetection != nil {
		input.TurnDetection = *turnDetection
	}

	return realtime.ClientSecretNewParams{
		Session: realtime.ClientSecretNewParamsSessionUnion{
			OfTranscription: &realtime.RealtimeTranscriptionSessionCreateRequestParam{
				Audio: realtime.RealtimeTranscriptionSessionAudioParam{
					Input: input,
				},
			},
		},
	}
}

func (c *Client) audioTranscriptionParam() realtime.AudioTranscriptionParam {
	param := realtime.AudioTranscriptionParam{
		Model: realtime.AudioTranscriptionModel(c.cfg.Model),
	}
	if language := strings.TrimSpace(c.cfg.Language); language != "" {
		param.Language = openaisdk.String(language)
	}
	if prompt := c.prompt(); prompt != "" {
		param.Prompt = openaisdk.String(prompt)
	}
	return param
}

func (c *Client) turnDetectionPayload() any {
	if c.streaming.Mode != "segment" {
		return nil
	}

	return map[string]any{
		"type":                "server_vad",
		"prefix_padding_ms":   c.streaming.PrefixPaddingMS,
		"silence_duration_ms": c.streaming.SilenceDurationMS,
		"threshold":           c.streaming.Threshold,
	}
}

func (c *Client) turnDetectionParam() *realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionUnionParam {
	if c.streaming.Mode != "segment" {
		return nil
	}

	return &realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionUnionParam{
		OfServerVad: &realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionServerVadParam{
			Type:              "server_vad",
			PrefixPaddingMs:   openaisdk.Int(int64(c.streaming.PrefixPaddingMS)),
			SilenceDurationMs: openaisdk.Int(int64(c.streaming.SilenceDurationMS)),
			Threshold:         openaisdk.Float(c.streaming.Threshold),
		},
	}
}

func requestID(err *openaisdk.Error) string {
	if err == nil || err.Response == nil {
		return ""
	}
	return strings.TrimSpace(err.Response.Header.Get("x-request-id"))
}

type jsonMessage map[string]any

func (m jsonMessage) Type() string {
	value, _ := m["type"].(string)
	return value
}

func (m jsonMessage) decode(dst any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
