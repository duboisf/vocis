package openai

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vtt/internal/config"
)

func TestTranscribeUsesSDKAndReturnsText(t *testing.T) {
	t.Parallel()

	var sawAuth string
	var sawOrg string
	var sawProject string
	var sawContentType string
	var sawPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawOrg = r.Header.Get("OpenAI-Organization")
		sawProject = r.Header.Get("OpenAI-Project")
		sawContentType = r.Header.Get("Content-Type")
		sawPath = r.URL.Path

		if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil {
			t.Fatalf("parse content type: %v", err)
		} else {
			reader, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}

			fields := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("next part: %v", err)
				}
				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("read part %s: %v", part.FormName(), err)
				}
				fields[part.FormName()] = string(body)
				_ = params
			}

			if got := fields["model"]; got != "gpt-4o-mini-transcribe" {
				t.Fatalf("model = %q", got)
			}
			if got := fields["language"]; got != "en" {
				t.Fatalf("language = %q", got)
			}
			if got := fields["response_format"]; got != "json" {
				t.Fatalf("response_format = %q", got)
			}
			if got := fields["prompt"]; got != defaultProgrammerPrompt {
				t.Fatalf("prompt = %q, want %q", got, defaultProgrammerPrompt)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "hello from sdk"})
	}))
	defer server.Close()

	filePath := writeTempAudio(t)
	defer os.Remove(filePath)

	cfg := config.Default()
	cfg.OpenAI.BaseURL = server.URL
	cfg.OpenAI.Organization = "org_test"
	cfg.OpenAI.Project = "proj_test"
	cfg.OpenAI.Language = "en"

	client := New("test-key", cfg.OpenAI)
	got, err := client.Transcribe(context.Background(), filePath)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}

	if got != "hello from sdk" {
		t.Fatalf("transcript = %q", got)
	}
	if sawAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", sawAuth)
	}
	if sawOrg != "org_test" {
		t.Fatalf("organization = %q", sawOrg)
	}
	if sawProject != "proj_test" {
		t.Fatalf("project = %q", sawProject)
	}
	if sawPath != "/audio/transcriptions" {
		t.Fatalf("path = %q", sawPath)
	}
	if !strings.HasPrefix(sawContentType, "multipart/form-data;") {
		t.Fatalf("content type = %q", sawContentType)
	}
}

func TestTranscribeIncludesPromptHintWhenConfigured(t *testing.T) {
	t.Parallel()

	var sawPrompt string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("multipart reader: %v", err)
		}

		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("next part: %v", err)
			}
			body, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("read part %s: %v", part.FormName(), err)
			}
			if part.FormName() == "prompt" {
				sawPrompt = string(body)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "prompt hint ok"})
	}))
	defer server.Close()

	filePath := writeTempAudio(t)
	defer os.Remove(filePath)

	cfg := config.Default()
	cfg.OpenAI.BaseURL = server.URL
	cfg.OpenAI.PromptHint = "Use technical spelling."
	cfg.OpenAI.Vocabulary = []string{"OpenAI", "GPT", "VTT"}

	client := New("test-key", cfg.OpenAI)
	got, err := client.Transcribe(context.Background(), filePath)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}

	if got != "prompt hint ok" {
		t.Fatalf("transcript = %q", got)
	}
	if sawPrompt != "Use technical spelling." {
		t.Fatalf("prompt = %q", sawPrompt)
	}
}

func TestPromptUsesBuiltInProgrammerHintByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.OpenAI.PromptHint = ""

	client := New("test-key", cfg.OpenAI)
	if got := client.prompt(); got != defaultProgrammerPrompt {
		t.Fatalf("prompt = %q, want %q", got, defaultProgrammerPrompt)
	}
}

func writeTempAudio(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.wav")
	if err := os.WriteFile(path, []byte("RIFFfakewavdata"), 0o600); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	return path
}
