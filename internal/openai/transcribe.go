package openai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"vtt/internal/config"
)

type Client struct {
	cfg    config.OpenAIConfig
	client openaisdk.Client
}

const defaultProgrammerPrompt = "Transcribe for a programmer speaking naturally. " +
	"Prefer common software, terminal, API, Git, and GitHub terminology. Preserve " +
	"obvious technical terms, acronyms, and capitalization when the audio supports " +
	"them. Do not invent extra words that were not spoken."

func New(apiKey string, cfg config.OpenAIConfig) *Client {
	timeout := time.Duration(cfg.RequestLimit) * time.Second

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")),
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
	}
}

func (c *Client) Transcribe(ctx context.Context, filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	params := openaisdk.AudioTranscriptionNewParams{
		File:           file,
		Model:          openaisdk.AudioModel(c.cfg.Model),
		ResponseFormat: openaisdk.AudioResponseFormatJSON,
	}
	if language := strings.TrimSpace(c.cfg.Language); language != "" {
		params.Language = openaisdk.String(language)
	}
	if prompt := c.prompt(); prompt != "" {
		params.Prompt = openaisdk.String(prompt)
	}

	resp, err := c.client.Audio.Transcriptions.New(ctx, params)
	if err != nil {
		var apiErr *openaisdk.Error
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf(
				"openai transcription: status %d%s: %s",
				apiErr.StatusCode,
				requestIDSuffix(requestID(apiErr)),
				strings.TrimSpace(apiErr.Message),
			)
		}
		return "", err
	}

	return strings.TrimSpace(resp.Text), nil
}

func (c *Client) prompt() string {
	if hint := strings.TrimSpace(c.cfg.PromptHint); hint != "" {
		return hint
	}
	return defaultProgrammerPrompt
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

func requestID(err *openaisdk.Error) string {
	if err == nil || err.Response == nil {
		return ""
	}
	return strings.TrimSpace(err.Response.Header.Get("x-request-id"))
}
