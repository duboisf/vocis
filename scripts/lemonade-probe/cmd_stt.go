package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

// runSTT handles `lemonade-probe stt <wav|mic|text> <arg> [flags]`.
// Dispatches to a source-specific capture, then runs the shared
// realtime-WS probe.
func runSTT(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("stt: missing source subcommand (wav|mic|text)")
	}
	source, tail := args[0], args[1:]

	fs := flag.NewFlagSet("stt "+source, flag.ContinueOnError)
	url := fs.String("url", "ws://localhost:9000/realtime?model=whisper-v3-turbo-FLM", "realtime WS url")
	model := fs.String("model", "whisper-v3-turbo-FLM", "STT model id for session.update")
	silenceMs := fs.Int("silence_ms", 500, "VAD silence_duration_ms; -1 disables turn_detection")
	padMs := fs.Int("pad_ms", 0, "append this many ms of silence after audio before committing")
	pauseMs := fs.Int("pause_ms", 200, "sleep after audio (and pad) before commit")
	skipCommit := fs.Bool("skip_commit", false, "omit input_audio_buffer.commit and rely entirely on VAD auto-commit")
	timeoutMs := fs.Int("timeout_ms", 30000, "safety ceiling for Collect; real exit is when every speech_started has a matching completed")
	debug := fs.Bool("debug", false, "dump raw JSON for every WS frame")

	// TTS-only flags (ignored for wav/mic) — kept here so `stt text` is
	// a one-liner without a second level of subcommand parsing.
	ttsURL := fs.String("tts_url", "http://localhost:13305/api/v1/audio/speech", "TTS endpoint (stt text only)")
	ttsModel := fs.String("tts_model", "kokoro-v1", "TTS model id (stt text only)")
	ttsVoice := fs.String("tts_voice", "fable", "TTS voice id (stt text only)")

	// parse remaining args AFTER the positional (path/seconds/text)
	if len(tail) == 0 {
		return fmt.Errorf("stt %s: missing argument", source)
	}
	positional, flagArgs := tail[0], tail[1:]
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	samples, rate, describe, err := captureForSTT(ctx, source, positional,
		*ttsURL, *ttsModel, *ttsVoice)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "source: %s → %d samples @ %d Hz (%dms audio)\n",
		describe, len(samples), rate, len(samples)*1000/rate)

	session := New(Config{
		URL:        *url,
		Model:      *model,
		VADms:      *silenceMs,
		Log:        os.Stderr, // protocol events → stderr
		Transcript: os.Stdout, // each completed turn → stdout, immediately
		Debug:      *debug,
	})
	if err := session.Start(ctx); err != nil {
		return err
	}
	defer session.Close()

	if err := session.Stream(StreamOpts{
		Samples:    samples,
		Rate:       rate,
		PadMs:      *padMs,
		PauseMs:    *pauseMs,
		SkipCommit: *skipCommit,
	}); err != nil {
		return err
	}

	result, err := session.Collect(ctx, time.Duration(*timeoutMs)*time.Millisecond)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "SUMMARY  %s\n", result.Summary())
	return nil
}

// captureForSTT picks the right source function based on subcommand.
// Returns samples + rate + a human-readable describe string used for
// one-line logging before the session spins up.
func captureForSTT(ctx context.Context, source, positional, ttsURL, ttsModel, ttsVoice string) ([]int16, int, string, error) {
	switch source {
	case "wav":
		samples, rate, err := CaptureWAV(positional, 22050)
		if err != nil {
			return nil, 0, "", err
		}
		return samples, rate, fmt.Sprintf("wav %s", positional), nil

	case "mic":
		seconds, err := atoiPositive(positional, "seconds")
		if err != nil {
			return nil, 0, "", err
		}
		samples, rate, err := CaptureMic(ctx, seconds)
		if err != nil {
			return nil, 0, "", err
		}
		return samples, rate, fmt.Sprintf("mic %ds", seconds), nil

	case "text":
		samples, rate, err := CaptureTTS(ctx, ttsURL, ttsModel, ttsVoice, positional)
		if err != nil {
			return nil, 0, "", err
		}
		return samples, rate, fmt.Sprintf("tts %s/%s %q", ttsModel, ttsVoice, positional), nil

	default:
		return nil, 0, "", fmt.Errorf("stt: unknown source %q (want wav|mic|text)", source)
	}
}

func atoiPositive(s, label string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("expected positive integer for %s, got %q", label, s)
	}
	return n, nil
}
