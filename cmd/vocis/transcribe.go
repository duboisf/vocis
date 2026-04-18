package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vocis/internal/config"
	"vocis/internal/openai"
	"vocis/internal/recorder"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

var (
	transcribeUsePostprocess bool
)

var transcribeCmd = &cobra.Command{
	Use:   "transcribe",
	Short: "One-shot dictation: speak, press Enter to finish, transcript prints to stdout",
	Long: `Records from the default microphone and streams to the configured backend
(openai or lemonade) without the overlay, hotkey, or paste injection — useful
for iterating on transcription quality / latency from the command line.

Logs go to stderr. The final transcript (after optional post-processing)
is the only thing written to stdout, so you can pipe it into other tools.

Press Enter to stop recording. Ctrl-C aborts without producing output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTranscribe()
	},
}

func init() {
	transcribeCmd.Flags().BoolVar(&transcribeUsePostprocess, "postprocess", false,
		"run the configured post-processing step on the final transcript before printing")
	rootCmd.AddCommand(transcribeCmd)
}

func runTranscribe() error {
	session, err := sessionlog.Start()
	if err != nil {
		return err
	}
	defer session.Close()

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	sessionlog.Infof("vocis %s transcribe (config=%s)", version, path)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Init(ctx, cfg.Telemetry, version)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer shutdownTelemetry(context.Background())

	apiKey := ""
	if cfg.OpenAI.Backend != config.BackendLemonade {
		key, err := securestore.New().APIKey()
		if err != nil {
			return fmt.Errorf("load api key: %w", err)
		}
		apiKey = key
	}

	rec := recorder.New()
	recordingCtx, cancelRecording := context.WithCancel(ctx)
	defer cancelRecording()

	recSession, err := rec.Start(recordingCtx, cfg.Recording)
	if err != nil {
		return fmt.Errorf("start recorder: %w", err)
	}
	defer recSession.Cleanup()

	client := openai.New(apiKey, cfg.OpenAI, cfg.Streaming)
	dictation, err := client.StartDictation(
		recordingCtx,
		recSession.SampleRate(),
		recSession.Channels(),
		recSession.Samples(),
		openai.ConnectCallbacks{
			OnConnecting: func(attempt, max int) {
				sessionlog.Infof("realtime: connecting (attempt %d/%d)", attempt, max)
			},
			OnConnected: func() {
				sessionlog.Infof("realtime: connected")
			},
		},
	)
	if err != nil {
		_ = recSession.Stop(context.Background())
		return fmt.Errorf("start dictation: %w", err)
	}

	// Consume partial/segment events for live progress on stderr.
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for ev := range dictation.Events() {
			switch ev.Type {
			case openai.DictationEventPartial:
				if ev.Text != "" {
					fmt.Fprintf(os.Stderr, "[partial] %s\n", ev.Text)
				}
			case openai.DictationEventSegment:
				if ev.Text != "" {
					fmt.Fprintf(os.Stderr, "[segment] %s\n", ev.Text)
				}
			}
		}
	}()

	// Stop trigger: Enter on stdin OR signal. The stdin reader runs in
	// a goroutine and we never `wait` on it — bufio.Reader on os.Stdin
	// can't be unblocked from another goroutine, so on signal we just
	// leak it and let process exit reclaim it.
	enter := make(chan struct{}, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')
		select {
		case enter <- struct{}{}:
		default:
		}
	}()

	fmt.Fprintln(os.Stderr, "recording — press Enter to finish (Ctrl-C to abort)")

	aborted := false
	select {
	case <-enter:
		sessionlog.Infof("stop requested via stdin enter")
	case <-ctx.Done():
		sessionlog.Infof("stop requested via signal — aborting")
		aborted = true
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := recSession.Stop(stopCtx); err != nil {
		sessionlog.Warnf("recorder stop: %v", err)
	}

	if aborted {
		// Don't wait for the backend to finalize — the user hit Ctrl-C.
		// Cancel the dictation context so the WebSocket read loop exits
		// and the events goroutine can drain.
		cancelRecording()
		<-eventsDone
		return fmt.Errorf("aborted by signal")
	}

	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer finalizeCancel()

	result, err := dictation.Finalize(finalizeCtx)
	cancelRecording()
	<-eventsDone
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}

	final := result.Text
	if transcribeUsePostprocess && cfg.PostProcess.Enabled {
		fmt.Fprintln(os.Stderr, "[postprocess] running")
		ppCtx, ppCancel := context.WithTimeout(context.Background(),
			time.Duration(cfg.PostProcess.TotalTimeoutSec)*time.Second)
		defer ppCancel()
		pp := client.PostProcess(ppCtx, cfg.PostProcess, final, func() {
			fmt.Fprintln(os.Stderr, "[postprocess] first token")
		})
		if pp.Skipped {
			fmt.Fprintln(os.Stderr, "[postprocess] skipped")
		} else {
			final = pp.Text
		}
	}

	fmt.Fprintf(os.Stderr, "transcript: %d chars\n", len(final))
	fmt.Println(final)
	return nil
}
