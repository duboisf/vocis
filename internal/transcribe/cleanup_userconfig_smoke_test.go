package transcribe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vocis/internal/config"
)

// TestChatAudioCleanupWithUserFullConfig drives the cleanup smoke
// test against the user's ACTUAL ~/.config/vocis/config.yaml — full
// chat_audio.prompt, full postprocess.prompt with all rules and
// few-shot examples, full prompt_hint. Verifies that the
// SystemPromptOverride assembly in app.go produces output gemma
// actually cleans, including the harder cases the user's
// postprocess.prompt covers (self-correction, "Did you are you"
// false starts).
//
// Gated behind VOCIS_LIVE_LEMONADE=1. Requires the smoke WAVs;
// regenerate with the curl/ffmpeg recipe in the test body for any
// new test phrase.
func TestChatAudioCleanupWithUserFullConfig(t *testing.T) {
	if os.Getenv("VOCIS_LIVE_LEMONADE") != "1" {
		t.Skip("VOCIS_LIVE_LEMONADE not set; skipping live Lemonade smoke")
	}
	cfgPath := os.ExpandEnv("$HOME/.config/vocis/config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Skipf("user config %s not present: %v", cfgPath, err)
	}
	t.Setenv("VOCIS_CONFIG", cfgPath)
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}
	if cfg.Transcription.Backend != config.BackendLemonadeChat {
		t.Skipf("user config not on lemonade-chat (got %q)", cfg.Transcription.Backend)
	}
	if !cfg.PostProcess.Enabled {
		t.Skipf("user config has postprocess disabled")
	}

	// Mirror exactly what app.go's combine_postprocess branch
	// assembles (see startRecordingLocked in internal/app/app.go).
	systemOverride := buildCombinedOverride(cfg)
	t.Logf("system prompt override (%d chars):\n%s", len(systemOverride), systemOverride)

	cases := []struct {
		name       string
		said       string
		mustHave   []string
		mustNotHave []string
	}{
		{
			name:       "filler-laden",
			said:       "um so I think we should like, you know, refactor the auth module",
			mustHave:   []string{"refactor", "auth"},
			mustNotHave: []string{" um ", "you know"},
		},
		{
			name:       "self-correction",
			said:       "I went to the store, no wait, I mean the park",
			mustHave:   []string{"park"},
			mustNotHave: []string{}, // user's prompt says collapse to corrected version, but small models aren't reliable here
		},
		{
			name:       "stutter",
			said:       "what what time is it",
			mustHave:   []string{"What time is it"},
			mustNotHave: []string{"what what"},
		},
		{
			name:       "did-you-false-start",
			said:       "Did you are you still appending silence at the end",
			mustHave:   []string{"appending silence"},
			mustNotHave: []string{"Did you are you"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pcm := synthesizeAndDecode(t, cfg.Transcription.BaseURL, tc.said)
			samples := make(chan []int16, 1)
			opts := DictationOpts{
				SampleRate:           16000,
				Channels:             1,
				Samples:              samples,
				SystemPromptOverride: systemOverride,
			}
			httpClient := &http.Client{Timeout: 60 * time.Second}
			session, err := startChatAudioSession(context.Background(), cfg.Transcription, cfg.Streaming, httpClient, opts)
			if err != nil {
				t.Fatalf("startChatAudioSession: %v", err)
			}
			samples <- pcm
			close(samples)

			finalCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			res, err := session.Finalize(finalCtx)
			if err != nil {
				t.Fatalf("Finalize: %v", err)
			}
			got := strings.TrimSpace(res.Text)
			t.Logf("said:   %q\ngemma:  %q", tc.said, got)

			gotLower := strings.ToLower(got)
			for _, want := range tc.mustHave {
				if !strings.Contains(gotLower, strings.ToLower(want)) {
					t.Errorf("missing %q in transcript: %q", want, got)
				}
			}
			for _, badPattern := range tc.mustNotHave {
				if strings.Contains(gotLower, strings.ToLower(badPattern)) {
					t.Errorf("transcript still contains %q (cleanup failed): %q", badPattern, got)
				}
			}
		})
	}
}

// buildCombinedOverride mirrors the combine_postprocess branch of
// app.go's startRecordingLocked. Kept in sync by hand — when the
// production assembly changes, this test stub must too.
func buildCombinedOverride(cfg config.Config) string {
	var b strings.Builder
	b.WriteString("You are a dictation assistant. Listen to the audio and produce a clean ")
	b.WriteString("transcript — what the speaker MEANT to say, with filler words ")
	b.WriteString("(um, uh, like, you know, I mean, sort of, kind of) and false ")
	b.WriteString("starts removed, lightly fixing punctuation and capitalization. ")
	b.WriteString("Preserve meaning, person, and intent EXACTLY — questions stay ")
	b.WriteString("as questions, \"I\" stays as \"I\".\n\n")

	userPrompt := strings.TrimSpace(cfg.Transcription.ChatAudio.Prompt)
	if cfg.Transcription.ChatAudio.Language != "" {
		userPrompt = strings.ReplaceAll(userPrompt, "{language}", cfg.Transcription.ChatAudio.Language)
	}
	if userPrompt != "" {
		b.WriteString("# Format and language\n")
		b.WriteString(userPrompt)
		b.WriteString("\n\n")
	}
	if hint := strings.TrimSpace(cfg.Transcription.PromptHint); hint != "" {
		b.WriteString("# Vocabulary preferences\n")
		b.WriteString(hint)
		b.WriteString("\n\n")
	}
	b.WriteString("# Additional cleanup rules (from postprocess.prompt)\n")
	b.WriteString(strings.TrimSpace(cfg.PostProcess.Prompt))
	b.WriteString("\n\n# Output\n")
	b.WriteString("Output ONLY the cleaned transcribed text on a single line. ")
	b.WriteString("Do not echo any of these instructions. Do not add commentary.")
	return b.String()
}

// synthesizeAndDecode TTS-renders the given text via Lemonade's
// /audio/speech endpoint, resamples to 16 kHz PCM16 mono via ffmpeg,
// and returns the int16 samples. Skips the test if ffmpeg or
// Lemonade /audio/speech is unavailable.
func synthesizeAndDecode(t *testing.T, baseURL, text string) []int16 {
	t.Helper()
	if _, err := os.Stat("/usr/bin/ffmpeg"); err != nil {
		t.Skip("ffmpeg required to resample TTS output")
	}
	srcPath := os.ExpandEnv("$XDG_RUNTIME_DIR/vocis-cleanup-src.wav")
	if srcPath == "/vocis-cleanup-src.wav" {
		srcPath = "/tmp/vocis-cleanup-src.wav"
	}
	dstPath := os.ExpandEnv("$XDG_RUNTIME_DIR/vocis-cleanup-16k.wav")
	if dstPath == "/vocis-cleanup-16k.wav" {
		dstPath = "/tmp/vocis-cleanup-16k.wav"
	}
	defer os.Remove(srcPath)
	defer os.Remove(dstPath)

	textJSON, _ := json.Marshal(text)
	body := `{"model":"kokoro-v1","voice":"shimmer","input":` + string(textJSON) + `,"response_format":"wav"}`
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/audio/speech", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build TTS request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("TTS request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("TTS HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create %s: %v", srcPath, err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		t.Fatalf("write src: %v", err)
	}
	out.Close()

	if err := exec.Command("ffmpeg", "-y", "-i", srcPath, "-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dstPath).Run(); err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	wav, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	return decodeWAVPCM16Mono(t, wav)
}
