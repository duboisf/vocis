package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"vocis/internal/transcribe"
	"vocis/internal/recorder"
	"vocis/internal/sessionlog"
)

var (
	transcribeUsePostprocess bool
)

var transcribeCmd = &cobra.Command{
	Use:   "transcribe",
	Short: "One-shot dictation: speak, press Enter to finish, transcript prints to stdout",
	Long: `Records from the default microphone and streams to the configured Lemonade
backend (realtime WS or chat-audio) without the overlay, hotkey, or paste
injection — useful for iterating on transcription quality / latency from the
command line.

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
	cfg, ctx, cleanup, err := bootCLIWithTelemetry("transcribe")
	if err != nil {
		return err
	}
	defer cleanup()

	// Pin Lemonade's ctx_size if the user requested one. Runs BEFORE
	// we open the mic so we don't record into a void while Lemonade
	// is reloading. No-op when ctx_size is 0.
	if err := transcribe.EnsureModelCtxSizeFromConfig(ctx, cfg); err != nil {
		return err
	}

	rec := recorder.New()
	recordingCtx, cancelRecording := context.WithCancel(ctx)
	defer cancelRecording()

	recSession, err := rec.Start(recordingCtx, cfg.Recording)
	if err != nil {
		return fmt.Errorf("start recorder: %w", err)
	}

	client := transcribe.New(cfg.Transcription, cfg.Streaming)
	dictation, err := client.StartDictation(recordingCtx, transcribe.DictationOpts{
		SampleRate: recSession.SampleRate(),
		Channels:   recSession.Channels(),
		Samples:    recSession.Samples(),
		Callbacks: transcribe.ConnectCallbacks{
			OnConnecting: func(attempt, max int) {
				sessionlog.Infof("realtime: connecting (attempt %d/%d)", attempt, max)
			},
			OnConnected: func() {
				sessionlog.Infof("realtime: connected")
			},
		},
	})
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
			case transcribe.DictationEventPartial:
				if ev.Text != "" {
					fmt.Fprintf(os.Stderr, "[partial] %s\n", ev.Text)
				}
			case transcribe.DictationEventSegment:
				if ev.Text != "" {
					fmt.Fprintf(os.Stderr, "[segment] %s\n", ev.Text)
				}
			case transcribe.DictationEventReplaceSegment:
				if ev.Text != "" {
					fmt.Fprintf(os.Stderr, "[replace prev=%d] %s\n", ev.PrevLen, ev.Text)
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

	// Outer cap on chat-audio finalize. The chat-audio session has its
	// own per-chunk timeouts; this is a wall-clock backstop so the CLI
	// always returns. 60s leaves comfortable headroom for a multi-clip
	// trailing batch under a cold model load.
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 60*time.Second)
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
