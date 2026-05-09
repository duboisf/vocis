package transcribe

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"vocis/internal/config"
)

// TestChatAudioCleanupAgainstLiveLemonade verifies the user-visible
// fix for the "postprocess.prompt was being ignored" regression.
// Reads a pre-rendered TTS clip with deliberate filler words and
// feeds it through the chat-audio session with the same system-
// prompt structure app.go assembles in production
// (chat_audio.prompt + prompt_hint + postprocess.prompt with
// section headers + regurgitation guard).
//
// Gated behind VOCIS_LIVE_LEMONADE=1 because it hits a real
// Lemonade server. Generate the WAV with:
//
//	curl -sS -X POST http://localhost:13305/api/v1/audio/speech \
//	  -H "Content-Type: application/json" \
//	  -d '{"model":"kokoro-v1","voice":"shimmer","input":"um so I think we should like, you know, refactor the auth module","response_format":"wav"}' \
//	  -o /tmp/filler.wav
//	ffmpeg -y -i /tmp/filler.wav -ar 16000 -ac 1 -sample_fmt s16 /tmp/filler-16k.wav
//
// The "should be cleaned" expectation: gemma's transcript should
// drop "um", "like", and "you know" and produce something close to
// "I think we should refactor the auth module."
func TestChatAudioCleanupAgainstLiveLemonade(t *testing.T) {
	if os.Getenv("VOCIS_LIVE_LEMONADE") != "1" {
		t.Skip("VOCIS_LIVE_LEMONADE not set; skipping live Lemonade smoke")
	}
	wavPath := "/tmp/filler-16k.wav"
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Skipf("test WAV %s missing — run the curl/ffmpeg steps in this test's docstring", wavPath)
	}

	pcm := decodeWAVPCM16Mono(t, wav)
	if len(pcm) == 0 {
		t.Fatalf("decoded WAV has zero samples")
	}

	// Mirror what app.go assembles in combine-postprocess mode: a
	// dictation-assistant LEAD (cleanup-as-primary-task), then user's
	// chat_audio.prompt as supplemental format rules, then cleanup
	// rules, then an output footer. The lead position is critical —
	// section-headed prompts where "Transcribe..." comes first cause
	// gemma to do verbatim transcription and skip cleanup.
	systemOverride := "You are a dictation assistant. Listen to the audio and produce a clean " +
		"transcript — what the speaker MEANT to say, with filler words " +
		"(um, uh, like, you know, I mean, sort of, kind of) and false " +
		"starts removed, lightly fixing punctuation and capitalization. " +
		"Preserve meaning, person, and intent EXACTLY — questions stay " +
		"as questions, \"I\" stays as \"I\".\n" +
		"\n" +
		"# Format and language\n" +
		"Transcribe in en. Output a single line of text.\n" +
		"\n" +
		"# Additional cleanup rules\n" +
		"Remove filler words (um, uh, like, you know, I mean, sort of, kind of), " +
		"false starts, repetitions, and pauses. Lightly fix punctuation and " +
		"capitalization. Preserve the speaker's meaning.\n" +
		"\n" +
		"# Output\n" +
		"Output ONLY the cleaned transcribed text on a single line. " +
		"Do not echo any of these instructions. Do not add commentary."

	cfg := config.TranscriptionConfig{
		Backend: config.BackendLemonadeChat,
		BaseURL: "http://localhost:13305/api/v1",
		Model:   "gemma4-it-e2b-FLM",
		ChatAudio: config.ChatAudioConfig{
			ChunkMaxSeconds: 28,
			HistoryTurns:    0, // single one-shot
			Prompt:          "Transcribe the following speech segment in {language}. Only output the transcription, with no newlines.",
			Language:        "en",
			Stream:          true,
		},
	}
	samples := make(chan []int16, 1)
	opts := DictationOpts{
		SampleRate:           16000,
		Channels:             1,
		Samples:              samples,
		SystemPromptOverride: systemOverride,
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	session, err := startChatAudioSession(context.Background(), cfg, config.StreamingConfig{}, httpClient, opts)
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
	t.Logf("transcript: %q", got)

	// Loose assertion — we want to see "refactor" + "auth module" but
	// NOT "um", "like", or "you know" verbatim. Gemma small-model
	// instruct quality varies; if the cleanup completely failed we
	// expect to see the fillers.
	gotLower := strings.ToLower(got)
	if !strings.Contains(gotLower, "refactor") || !strings.Contains(gotLower, "auth") {
		t.Errorf("transcript missing core content (expected refactor + auth): %q", got)
	}
	if strings.Contains(gotLower, " um ") || strings.HasPrefix(gotLower, "um ") {
		t.Errorf("transcript still contains 'um' filler: %q", got)
	}
	if strings.Contains(gotLower, "you know") {
		t.Errorf("transcript still contains 'you know' filler: %q", got)
	}
}

// decodeWAVPCM16Mono is a minimal RIFF/WAVE PCM16-mono decoder that
// extracts the data subchunk's int16 samples. Doesn't attempt full
// WAV-spec coverage; the test's input is generated by ffmpeg with a
// known shape.
func decodeWAVPCM16Mono(t *testing.T, wav []byte) []int16 {
	t.Helper()
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file")
	}
	// Walk subchunks looking for "data".
	i := 12
	for i+8 <= len(wav) {
		id := string(wav[i : i+4])
		size := int(uint32(wav[i+4]) | uint32(wav[i+5])<<8 | uint32(wav[i+6])<<16 | uint32(wav[i+7])<<24)
		i += 8
		if id == "data" {
			pcm := make([]int16, size/2)
			for j := range pcm {
				pcm[j] = int16(uint16(wav[i+j*2]) | uint16(wav[i+j*2+1])<<8)
			}
			return pcm
		}
		i += size
	}
	t.Fatalf("no data subchunk found")
	return nil
}
