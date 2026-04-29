package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/tts"
)

var (
	speakOutPath string
	speakVoice   string
	speakModel   string
)

var speakCmd = &cobra.Command{
	Use:   "speak [text...]",
	Short: "Synthesize speech via Lemonade Kokoro TTS and play it",
	Long: `Sends text to Lemonade's OpenAI-compatible /audio/speech endpoint
(Kokoro TTS by default) and streams the resulting PCM into paplay so
you hear it through the default PulseAudio sink.

If no text is given on the command line, vocis reads from stdin —
useful for piping output of another command:

    echo "hello world" | vocis speak
    vocis recall last 5m | vocis speak

With --out PATH, the audio is written as a 24 kHz mono PCM16 WAV
instead of being played. Pass "-" to write the WAV to stdout.

Voice and model can be overridden per-call with --voice / --model;
otherwise the speak.voice and speak.model values from the config
are used. The Lemonade base URL falls back to transcription.base_url
when speak.base_url is empty in the config.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSpeak(args)
	},
}

func init() {
	speakCmd.Flags().StringVar(&speakOutPath, "out", "",
		"write a WAV file to PATH instead of playing (use - for stdout)")
	speakCmd.Flags().StringVar(&speakVoice, "voice", "",
		"override speak.voice from the config (e.g. af_heart, fable)")
	speakCmd.Flags().StringVar(&speakModel, "model", "",
		"override speak.model from the config (default kokoro-v1)")
	rootCmd.AddCommand(speakCmd)
}

func runSpeak(args []string) error {
	session, err := sessionlog.Start()
	if err != nil {
		return err
	}
	defer session.Close()

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	sessionlog.Infof("vocis %s speak (config=%s)", version, path)

	text, err := resolveSpeakText(args)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("no text to speak (give text as args or pipe it on stdin)")
	}

	baseURL := strings.TrimSpace(cfg.Speak.BaseURL)
	if baseURL == "" {
		baseURL = cfg.Transcription.BaseURL
	}
	if baseURL == "" {
		return fmt.Errorf("speak.base_url and transcription.base_url are both empty — set one to the Lemonade REST endpoint (e.g. http://localhost:13305/api/v1)")
	}

	model := cfg.Speak.Model
	if speakModel != "" {
		model = speakModel
	}
	voice := cfg.Speak.Voice
	if speakVoice != "" {
		voice = speakVoice
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "synthesizing %d chars via %s/%s...\n", len(text), model, voice)
	result, err := tts.Synthesize(ctx, baseURL, model, voice, text)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "got %d samples @ %d Hz (%.2fs)\n",
		len(result.PCM), result.SampleRate,
		float64(len(result.PCM))/float64(result.SampleRate))

	if speakOutPath != "" {
		return writeSpeakWAV(speakOutPath, result)
	}
	return playPCM(ctx, result.PCM, result.SampleRate)
}

// resolveSpeakText pulls speech text from CLI args (joined with spaces)
// or, when args is empty, from stdin. We never mix the two — args
// always wins to keep behavior predictable for shell completion users
// who happen to have stdin attached to a tty.
func resolveSpeakText(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("stat stdin: %w", err)
	}
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return "", fmt.Errorf("no text given and stdin is a tty — pipe text in or pass it as args")
	}
	buf, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(buf), nil
}

func writeSpeakWAV(path string, r *tts.Result) error {
	if path == "-" {
		if err := tts.WriteWAV(os.Stdout, r.PCM, r.SampleRate); err != nil {
			return fmt.Errorf("write WAV to stdout: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote WAV (%d Hz mono PCM16) to stdout\n", r.SampleRate)
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := tts.WriteWAV(f, r.PCM, r.SampleRate); err != nil {
		return fmt.Errorf("write WAV %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d Hz mono PCM16)\n", path, r.SampleRate)
	return nil
}

// playPCM streams raw int16 LE samples into paplay --raw at the given
// rate. Same shape as the recall replay path: persistent paplay
// process, write-then-close-stdin so paplay drains the device cleanly.
func playPCM(ctx context.Context, pcm []int16, rate int) error {
	if _, err := exec.LookPath("paplay"); err != nil {
		return fmt.Errorf("paplay not found on PATH (install pulseaudio-utils): %w", err)
	}

	cmd := exec.CommandContext(ctx, "paplay",
		"--raw",
		"--rate="+strconv.Itoa(rate),
		"--channels=1",
		"--format=s16le",
	)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("paplay stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start paplay: %w", err)
	}

	buf := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	if _, err := stdin.Write(buf); err != nil {
		stdin.Close()
		_ = cmd.Wait()
		return fmt.Errorf("pipe pcm: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("close paplay stdin: %w", err)
	}
	return cmd.Wait()
}
