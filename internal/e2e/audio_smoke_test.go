//go:build integration

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vtt/internal/config"
	"vtt/internal/openai"
	"vtt/internal/recorder"
	"vtt/internal/securestore"
	"vtt/internal/sessionlog"
)

func TestPiperAudioToRealtimeSmoke(t *testing.T) {
	if os.Getenv("VTT_AUDIO_SMOKE") != "1" {
		t.Skip("set VTT_AUDIO_SMOKE=1 to run the audio smoke test")
	}

	requireCommand(t, "pactl")
	requireCommand(t, "paplay")
	requireCommand(t, "piper")

	voiceModel := findVoiceModel(t)
	apiKey := lookupAPIKey(t)

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logSession, err := sessionlog.Start()
	if err != nil {
		t.Fatalf("start session log: %v", err)
	}
	defer logSession.Close()

	sinkName := fmt.Sprintf("vtt_smoke_%d", time.Now().UnixNano())
	moduleID := loadNullSink(t, sinkName)
	defer unloadModule(t, moduleID)

	sourceName := sinkName + ".monitor"
	waitForSource(t, sourceName)

	cfg := config.Default()
	cfg.Recording.Device = sourceName
	cfg.Streaming.Mode = "release"
	cfg.OpenAI.RequestLimit = 60

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	rec := recorder.New()
	recording, err := rec.Start(ctx, cfg.Recording)
	if err != nil {
		t.Fatalf("start recorder: %v", err)
	}

	client := openai.New(apiKey, cfg.OpenAI, cfg.Streaming)
	dictation, err := client.StartDictation(
		ctx,
		cfg.Recording.SampleRate,
		cfg.Recording.Channels,
		recording.Samples(),
	)
	if err != nil {
		t.Fatalf("start dictation: %v", err)
	}

	go func() {
		for event := range dictation.Events() {
			switch event.Type {
			case openai.DictationEventPartial:
				if strings.TrimSpace(event.Text) != "" {
					sessionlog.Infof("smoke partial: %s", event.Text)
				}
			case openai.DictationEventSegment:
				sessionlog.Infof("smoke segment: %s", event.Text)
			}
		}
	}()

	wavPath := synthesizeSpeech(t, voiceModel, "hello smoke test this should become text")

	time.Sleep(250 * time.Millisecond)
	playAudio(t, sinkName, wavPath)
	time.Sleep(300 * time.Millisecond)

	if err := recording.Stop(ctx); err != nil {
		t.Fatalf("stop recorder: %v", err)
	}

	result, err := dictation.Finalize(ctx)
	if err != nil {
		t.Fatalf("finalize dictation: %v", err)
	}

	sessionlog.Infof("smoke transcription complete: %s", result.Text)

	if got := normalizeText(result.Text); !strings.Contains(got, "smoke") || !strings.Contains(got, "test") {
		t.Fatalf("transcript = %q, want it to contain smoke and test", result.Text)
	}

	logBytes, err := os.ReadFile(logSession.Path())
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "realtime transcription stream ready") {
		t.Fatalf("session log missing realtime readiness line:\n%s", logText)
	}
	if !strings.Contains(logText, "smoke transcription complete:") {
		t.Fatalf("session log missing smoke completion line:\n%s", logText)
	}
}

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is required: %v", name, err)
	}
}

func lookupAPIKey(t *testing.T) string {
	t.Helper()

	key, err := securestore.New().APIKey()
	if err != nil {
		t.Skipf("OpenAI API key unavailable: %v", err)
	}
	return key
}

func findVoiceModel(t *testing.T) string {
	t.Helper()

	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".local", "share", "piper-voices", "en_US-lessac-medium.onnx"),
		filepath.Join(os.Getenv("HOME"), ".local", "share", "piper-voices", "en_GB-alan-medium.onnx"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			if _, err := os.Stat(candidate + ".json"); err == nil {
				return candidate
			}
		}
	}
	t.Skip("no suitable local Piper voice model found")
	return ""
}

func loadNullSink(t *testing.T, sinkName string) string {
	t.Helper()

	out, err := runOutput(
		context.Background(),
		"pactl",
		"load-module",
		"module-null-sink",
		fmt.Sprintf("sink_name=%s", sinkName),
		fmt.Sprintf("sink_properties=device.description=%s", sinkName),
	)
	if err != nil {
		t.Fatalf("load null sink: %v", err)
	}
	return strings.TrimSpace(out)
}

func unloadModule(t *testing.T, moduleID string) {
	t.Helper()
	if strings.TrimSpace(moduleID) == "" {
		return
	}
	_, _ = runOutput(context.Background(), "pactl", "unload-module", strings.TrimSpace(moduleID))
}

func waitForSource(t *testing.T, sourceName string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runOutput(context.Background(), "pactl", "list", "short", "sources")
		if err == nil && strings.Contains(out, sourceName) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for source %q", sourceName)
}

func synthesizeSpeech(t *testing.T, voiceModel, text string) string {
	t.Helper()

	wavPath := filepath.Join(t.TempDir(), "smoke.wav")
	configPath := voiceModel + ".json"
	cmd := exec.Command("piper", "-m", voiceModel, "-c", configPath, "-f", wavPath)
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthesize speech: %v\n%s", err, out)
	}
	return wavPath
}

func playAudio(t *testing.T, sinkName, wavPath string) {
	t.Helper()

	if _, err := runOutput(context.Background(), "paplay", "--device", sinkName, wavPath); err != nil {
		t.Fatalf("play audio: %v", err)
	}
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}
