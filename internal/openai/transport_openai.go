package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/realtime"

	"vocis/internal/config"
)

const openaiSampleRate = 24000

type openaiTransport struct {
	cfg          config.OpenAIConfig
	streaming    config.StreamingConfig
	sdkClient    openaisdk.Client
	dialer       websocket.Dialer
	websocketURL string
}

func newOpenAITransport(
	cfg config.OpenAIConfig,
	streaming config.StreamingConfig,
	sdk openaisdk.Client,
	baseURL string,
	timeout time.Duration,
) *openaiTransport {
	return &openaiTransport{
		cfg:          cfg,
		streaming:    streaming,
		sdkClient:    sdk,
		dialer:       websocket.Dialer{HandshakeTimeout: minDuration(timeout, 5*time.Second)},
		websocketURL: openaiRealtimeWSURL(baseURL),
	}
}

func (t *openaiTransport) SampleRate() int { return openaiSampleRate }

func (t *openaiTransport) Dial(ctx context.Context) (*websocket.Conn, error) {
	secret, err := t.createClientSecret(ctx)
	if err != nil {
		return nil, err
	}
	headers := http.Header{
		"Authorization": []string{"Bearer " + secret},
	}
	conn, resp, err := t.dialer.DialContext(ctx, t.websocketURL, headers)
	if err != nil {
		return nil, formatDialError(err, resp)
	}
	return conn, nil
}

func (t *openaiTransport) SessionUpdate() map[string]any {
	transcription := map[string]any{
		"model": t.cfg.Model,
	}
	if language := strings.TrimSpace(t.cfg.Language); language != "" {
		transcription["language"] = language
	}
	if prompt := t.prompt(); prompt != "" {
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
						"rate": openaiSampleRate,
					},
					"transcription":  transcription,
					"turn_detection": t.turnDetectionPayload(),
				},
			},
		},
	}
}

func (t *openaiTransport) prompt() string {
	if hint := strings.TrimSpace(t.cfg.PromptHint); hint != "" {
		return hint
	}
	return config.DefaultPromptHint
}

func (t *openaiTransport) turnDetectionPayload() any {
	return map[string]any{
		"type":                "server_vad",
		"prefix_padding_ms":   t.streaming.PrefixPaddingMS,
		"silence_duration_ms": t.streaming.SilenceDurationMS,
		"threshold":           t.streaming.Threshold,
	}
}

func (t *openaiTransport) createClientSecret(ctx context.Context) (string, error) {
	resp, err := t.sdkClient.Realtime.ClientSecrets.New(ctx, t.clientSecretParams())
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

func (t *openaiTransport) clientSecretParams() realtime.ClientSecretNewParams {
	input := realtime.RealtimeTranscriptionSessionAudioInputParam{
		Format: realtime.RealtimeAudioFormatsUnionParam{
			OfAudioPCM: &realtime.RealtimeAudioFormatsAudioPCMParam{
				Type: "audio/pcm",
				Rate: openaiSampleRate,
			},
		},
		Transcription: t.audioTranscriptionParam(),
	}
	if td := t.turnDetectionParam(); td != nil {
		input.TurnDetection = *td
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

func (t *openaiTransport) audioTranscriptionParam() realtime.AudioTranscriptionParam {
	param := realtime.AudioTranscriptionParam{
		Model: realtime.AudioTranscriptionModel(t.cfg.Model),
	}
	if language := strings.TrimSpace(t.cfg.Language); language != "" {
		param.Language = openaisdk.String(language)
	}
	if prompt := t.prompt(); prompt != "" {
		param.Prompt = openaisdk.String(prompt)
	}
	return param
}

func (t *openaiTransport) turnDetectionParam() *realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionUnionParam {
	return &realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionUnionParam{
		OfServerVad: &realtime.RealtimeTranscriptionSessionAudioInputTurnDetectionServerVadParam{
			Type:              "server_vad",
			PrefixPaddingMs:   openaisdk.Int(int64(t.streaming.PrefixPaddingMS)),
			SilenceDurationMs: openaisdk.Int(int64(t.streaming.SilenceDurationMS)),
			Threshold:         openaisdk.Float(t.streaming.Threshold),
		},
	}
}

func openaiRealtimeWSURL(baseURL string) string {
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
