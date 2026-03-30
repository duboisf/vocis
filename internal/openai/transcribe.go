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

type Client struct {
	cfg          config.OpenAIConfig
	client       openaisdk.Client
	dialer       websocket.Dialer
	websocketURL string
	writeTimeout time.Duration
}

type Stream struct {
	conn         *websocket.Conn
	encoder      *pcmEncoder
	writeTimeout time.Duration

	writeMu    sync.Mutex
	closeOnce  sync.Once
	readyOnce  sync.Once
	resultOnce sync.Once

	readyCh  chan error
	resultCh chan transcriptResult
	readDone chan struct{}

	partialMu sync.Mutex
	partial   string
}

type transcriptResult struct {
	text string
	err  error
}

type realtimeEvent struct {
	Type string `json:"type"`
}

type realtimeReadyEvent struct {
	Type string `json:"type"`
}

type transcriptionDeltaEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type transcriptionCompletedEvent struct {
	Type       string `json:"type"`
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

func New(apiKey string, cfg config.OpenAIConfig) *Client {
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
		cfg:    cfg,
		client: openaisdk.NewClient(opts...),
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
		resultCh:     make(chan transcriptResult, 1),
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

func (s *Stream) Commit(ctx context.Context) (string, error) {
	if err := s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-s.resultCh:
		return strings.TrimSpace(result.text), result.err
	case <-s.readDone:
		return "", errors.New("openai realtime stream closed before transcription completed")
	}
}

func (s *Stream) Partial() string {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()
	return s.partial
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

	for {
		var raw jsonMessage
		if err := s.conn.ReadJSON(&raw); err != nil {
			s.markReady(err)
			s.finish("", err)
			return
		}

		switch raw.Type() {
		case "session.created", "session.updated":
			s.markReady(nil)
		case "conversation.item.input_audio_transcription.delta":
			var event transcriptionDeltaEvent
			if err := raw.decode(&event); err == nil && event.Delta != "" {
				s.partialMu.Lock()
				s.partial += event.Delta
				s.partialMu.Unlock()
			}
		case "conversation.item.input_audio_transcription.completed":
			var event transcriptionCompletedEvent
			if err := raw.decode(&event); err != nil {
				s.finish("", err)
				return
			}
			s.finish(strings.TrimSpace(event.Transcript), nil)
			return
		case "conversation.item.input_audio_transcription.failed":
			var event transcriptionFailedEvent
			if err := raw.decode(&event); err != nil {
				s.finish("", err)
				return
			}
			s.finish("", formatRealtimeError(event.Error.Code, event.Error.Message))
			return
		case "error":
			var event realtimeErrorEvent
			if err := raw.decode(&event); err != nil {
				s.finish("", err)
				return
			}
			err := formatRealtimeError(event.Error.Code, event.Error.Message)
			s.markReady(err)
			s.finish("", err)
			return
		}
	}
}

func (s *Stream) markReady(err error) {
	s.readyOnce.Do(func() {
		s.readyCh <- err
	})
}

func (s *Stream) finish(text string, err error) {
	s.resultOnce.Do(func() {
		s.resultCh <- transcriptResult{text: text, err: err}
	})
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
					"turn_detection": nil,
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
	return realtime.ClientSecretNewParams{
		Session: realtime.ClientSecretNewParamsSessionUnion{
			OfTranscription: &realtime.RealtimeTranscriptionSessionCreateRequestParam{
				Audio: realtime.RealtimeTranscriptionSessionAudioParam{
					Input: realtime.RealtimeTranscriptionSessionAudioInputParam{
						Format: realtime.RealtimeAudioFormatsUnionParam{
							OfAudioPCM: &realtime.RealtimeAudioFormatsAudioPCMParam{
								Type: "audio/pcm",
								Rate: 24000,
							},
						},
						Transcription: c.audioTranscriptionParam(),
					},
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
