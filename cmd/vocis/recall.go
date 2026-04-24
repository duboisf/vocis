package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vocis/internal/config"
	"vocis/internal/recall"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

var (
	recallPickSelection   string
	recallPickPostprocess bool
	recallPickJoin        string
	recallPickNoFZF       bool
)

var recallLastPostprocess bool

var recallCmd = &cobra.Command{
	Use:   "recall",
	Short: "Always-on dictation: capture continuously, transcribe on demand",
	Long: `Wokis Recall — an alternative to the push-to-talk serve mode. The
daemon captures microphone audio continuously, segments it with Silero
VAD, and keeps a bounded ring buffer of speech episodes. Use
"vocis recall pick" to browse recent segments and transcribe one on
demand.`,
}

var recallStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Run the recall daemon in the foreground",
	Long: `Starts the recall daemon: opens the configured microphone, runs Silero
VAD, and listens on the configured Unix socket for list/transcribe/drop
requests from the other recall subcommands. Runs until killed (Ctrl-C /
SIGTERM) or until "vocis recall stop" asks it to exit.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallStart()
	},
}

var recallPickCmd = &cobra.Command{
	Use:   "pick",
	Short: "Transcribe one or more of the recent segments",
	Long: `Lists the current ring buffer and asks you to pick segments to
transcribe. The selection accepts a comma-separated mix of:

    3       single id
    3-5     closed range (inclusive)
    3-      id 3 and every newer segment
    -5      every segment up to id 5
    all, *  everything currently buffered

Example: "3,5-7,10-" transcribes ids 3, 5, 6, 7, and anything ≥ 10.

Transcripts for multiple segments are joined with a space (override with
--join). Use --ids to skip the interactive prompt.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallPick()
	},
}

var recallStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report ring-buffer stats from a running daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallStatus()
	},
}

var recallStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Ask the running daemon to exit",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallStop()
	},
}

var recallDropIDs string

var (
	recallReplayIDs string
	recallReplayGap time.Duration
)

var recallReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Play back the raw audio of one or more segments",
	Long: `Pipes each segment's raw 16 kHz mono PCM into paplay so you can hear
what the daemon actually captured — useful for verifying whether a
suspicious long segment is really silence/noise before deciding to
drop it. Selection syntax matches pick (3, 3-5, 3-, -5, all).

Requires paplay on PATH (part of pulseaudio-utils on most distros).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallReplay()
	},
}

var recallLastCmd = &cobra.Command{
	Use:   "last <duration>",
	Short: "Transcribe every segment from the last N minutes as one joint transcript",
	Long: `Concatenates every segment whose StartedAt falls within the given
duration window (most recent first) and sends the combined audio
through a single transcription request. Cheaper than picking "all":
one realtime session instead of N, and the ASR keeps context across
segment boundaries.

Duration accepts any Go time.ParseDuration string — "10m", "2h",
"45s", "1h30m". A short silence gap (recall.batch_gap_ms) is inserted
between segments so words from adjacent segments don't weld together.

Example:
    vocis recall last 10m                   # last 10 minutes → stdout
    vocis recall last 2h --postprocess      # also run LLM cleanup`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallLast(args[0])
	},
}

var recallDropCmd = &cobra.Command{
	Use:   "drop",
	Short: "Remove segments from the ring buffer (and persisted files)",
	Long: `Removes the given segments from the daemon's ring buffer. When
recall.persist.mode is "disk", the matching seg-<id>.json files are
also deleted. Selection syntax matches the pick subcommand:

    3       single id
    3-5     closed range
    3-      id 3 and newer
    -5      everything up to 5
    all, *  every segment in the buffer`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecallDrop()
	},
}

func init() {
	recallPickCmd.Flags().StringVar(&recallPickSelection, "ids", "",
		"selection string (e.g. \"3\", \"3-5\", \"3-\", \"-5\", \"all\", or a comma-separated mix) — skips the interactive prompt")
	recallPickCmd.Flags().BoolVar(&recallPickPostprocess, "postprocess", false,
		"run the configured LLM cleanup on each transcript before joining")
	recallPickCmd.Flags().StringVar(&recallPickJoin, "join", " ",
		"separator inserted between segment transcripts when selecting multiple")
	recallPickCmd.Flags().BoolVar(&recallPickNoFZF, "no-fzf", false,
		"force the plain-table picker instead of fzf (default is fzf when it's on PATH)")

	recallDropCmd.Flags().StringVar(&recallDropIDs, "ids", "",
		"selection string (same syntax as pick --ids); required")
	_ = recallDropCmd.MarkFlagRequired("ids")

	recallReplayCmd.Flags().StringVar(&recallReplayIDs, "ids", "",
		"selection string (same syntax as pick --ids); required")
	recallReplayCmd.Flags().DurationVar(&recallReplayGap, "gap", 300*time.Millisecond,
		"silence inserted between segments when playing multiple")
	_ = recallReplayCmd.MarkFlagRequired("ids")

	recallLastCmd.Flags().BoolVar(&recallLastPostprocess, "postprocess", false,
		"run the configured LLM cleanup on the joint transcript before printing")

	recallCmd.AddCommand(recallStartCmd)
	recallCmd.AddCommand(recallPickCmd)
	recallCmd.AddCommand(recallLastCmd)
	recallCmd.AddCommand(recallStatusCmd)
	recallCmd.AddCommand(recallStopCmd)
	recallCmd.AddCommand(recallDropCmd)
	recallCmd.AddCommand(recallReplayCmd)
	rootCmd.AddCommand(recallCmd)
}

func runRecallStart() error {
	session, err := sessionlog.Start()
	if err != nil {
		return err
	}
	defer session.Close()

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	sessionlog.Infof("vocis %s recall start (config=%s)", version, path)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Init(ctx, cfg.Telemetry, version)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer shutdownTelemetry(context.Background())

	apiKey := ""
	if cfg.Transcription.Backend != config.BackendLemonade {
		key, err := securestore.New().APIKey()
		if err != nil {
			return fmt.Errorf("load api key: %w", err)
		}
		apiKey = key
	}

	d := recall.NewDaemon(recall.DaemonOpts{Config: cfg, APIKey: apiKey})
	fmt.Fprintln(os.Stderr, "recall daemon started — speak normally; use `vocis recall pick` from another terminal to transcribe a segment")
	return d.Run(ctx)
}

func runRecallPick() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	client := recall.NewClient(socket)

	// List runs on a short deadline; the transcribe calls each get their
	// own deadline below. A single long deadline around everything would
	// have to cover N transcriptions plus user thinking time at the
	// prompt, which is awkward to bound.
	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	segs, err := client.List(listCtx)
	listCancel()
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		fmt.Fprintln(os.Stderr, "no segments in buffer yet — speak into the mic first")
		return nil
	}

	availableIDs := make([]int64, len(segs))
	for i, s := range segs {
		availableIDs[i] = s.ID
	}

	var ids []int64
	if sel := strings.TrimSpace(recallPickSelection); sel != "" {
		ids, err = recall.ParseSelection(sel, availableIDs)
		if err != nil {
			return err
		}
	} else if useFZF := !recallPickNoFZF && fzfAvailable(); useFZF {
		ids, err = pickSegmentsWithFZF(segs)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "no selection")
			return nil
		}
	} else {
		printSegmentTable(os.Stderr, segs)
		ids, err = promptSegmentSelection(availableIDs)
		if err != nil {
			return err
		}
	}

	parts := make([]string, 0, len(ids))
	empties := 0
	for _, id := range ids {
		fmt.Fprintf(os.Stderr, "transcribing segment #%d...\n", id)
		txCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		text, err := client.Transcribe(txCtx, id, recallPickPostprocess)
		cancel()
		if err != nil {
			return fmt.Errorf("segment %d: %w", id, err)
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			empties++
			fmt.Fprintf(os.Stderr, "  segment #%d: (empty — likely silence or noise)\n", id)
			continue
		}
		parts = append(parts, trimmed)
	}
	if len(parts) == 0 {
		fmt.Fprintf(os.Stderr, "all %d segment(s) transcribed empty — nothing to print\n", empties)
		return nil
	}
	if empties > 0 {
		fmt.Fprintf(os.Stderr, "note: %d of %d segment(s) were empty and skipped\n", empties, len(ids))
	}
	fmt.Println(strings.Join(parts, recallPickJoin))
	return nil
}

func runRecallLast(rawDuration string) error {
	window, err := time.ParseDuration(rawDuration)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", rawDuration, err)
	}
	if window <= 0 {
		return fmt.Errorf("duration must be > 0 (got %s)", rawDuration)
	}

	session, sessErr := sessionlog.Start()
	if sessErr != nil {
		return sessErr
	}
	defer session.Close()

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	client := recall.NewClient(socket)

	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	segs, err := client.List(listCtx)
	listCancel()
	if err != nil {
		return err
	}

	ids := recall.SegmentIDsWithinWindow(segs, time.Now(), window)

	// Sum segment audio durations for a user-visible progress hint.
	// Also logged so session logs show the same figure the user saw on
	// stderr, per AGENTS.md's "every branch leaves evidence" rule.
	var totalAudio time.Duration
	idSet := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	for _, s := range segs {
		if _, ok := idSet[s.ID]; ok {
			totalAudio += time.Duration(s.DurationMS) * time.Millisecond
		}
	}

	sessionlog.Infof("recall last: window=%s available=%d matched=%d total_audio=%s ids=%v",
		window, len(segs), len(ids), totalAudio.Round(100*time.Millisecond), ids)

	if len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "no segments captured in the last %s\n", window)
		return nil
	}

	// No client-side timeout: local backends on long batches can
	// legitimately take tens of minutes, and a client-side cap would
	// just cut off valid work. The user drives lifetime via Ctrl-C —
	// signal.NotifyContext cancels the request ctx, which cleanly
	// closes the socket to the daemon, which in turn cancels its
	// dictation session and tears down the Lemonade WebSocket so the
	// model stops processing audio no one will read.
	txCtx, txCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer txCancel()

	fmt.Fprintf(os.Stderr, "transcribing %d segment(s), %s of audio covering the last %s... (Ctrl-C to abort)\n",
		len(ids), totalAudio.Round(time.Second), window)

	startedAt := time.Now()
	text, err := client.TranscribeBatch(txCtx, ids, recallLastPostprocess)
	elapsed := time.Since(startedAt).Round(100 * time.Millisecond)
	if err != nil {
		if errors.Is(err, context.Canceled) || txCtx.Err() == context.Canceled {
			fmt.Fprintf(os.Stderr, "aborted after %s\n", elapsed)
			return nil
		}
		return err
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		fmt.Fprintf(os.Stderr, "batch transcript was empty after %s (likely silence or noise)\n", elapsed)
		return nil
	}
	fmt.Fprintf(os.Stderr, "done in %s (%d chars)\n", elapsed, len(trimmed))
	fmt.Println(trimmed)
	return nil
}

func runRecallStatus() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := recall.NewClient(socket)
	stats, err := client.Status(ctx)
	if err != nil {
		return err
	}
	if stats == nil {
		fmt.Println("no stats returned")
		return nil
	}
	fmt.Printf("socket:    %s\n", socket)
	fmt.Printf("segments:  %d (ever captured: %d)\n", stats.Count, stats.TotalSeen)
	if stats.Count > 0 {
		fmt.Printf("oldest:    %s ago\n", time.Duration(stats.OldestAgeMS)*time.Millisecond)
		fmt.Printf("newest:    %s ago\n", time.Duration(stats.NewestAgeMS)*time.Millisecond)
		fmt.Printf("frames:    %d\n", stats.TotalFrames)
	}
	return nil
}

func runRecallReplay() error {
	if _, err := exec.LookPath("paplay"); err != nil {
		return fmt.Errorf("paplay not found on PATH (install pulseaudio-utils): %w", err)
	}

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	client := recall.NewClient(socket)

	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	segs, err := client.List(listCtx)
	listCancel()
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		fmt.Fprintln(os.Stderr, "no segments to replay")
		return nil
	}
	availableIDs := make([]int64, len(segs))
	for i, s := range segs {
		availableIDs[i] = s.ID
	}
	ids, err := recall.ParseSelection(recallReplayIDs, availableIDs)
	if err != nil {
		return err
	}

	// We fetch each segment's PCM from the daemon one at a time and
	// stream it into a single paplay process. Streaming into a
	// persistent paplay (instead of one-paplay-per-segment) keeps the
	// audio device open and avoids per-segment startup clicks.
	cmd := exec.Command("paplay",
		"--raw",
		"--rate=16000",
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

	gapSamples := int(recallReplayGap.Seconds() * 16000)
	gapBuf := make([]byte, gapSamples*2) // zeros = silence

	for i, id := range ids {
		fmt.Fprintf(os.Stderr, "playing segment #%d...\n", id)
		fetchCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		pcm, sampleRate, err := client.GetAudio(fetchCtx, id)
		cancel()
		if err != nil {
			stdin.Close()
			_ = cmd.Wait()
			return fmt.Errorf("segment %d: %w", id, err)
		}
		if sampleRate != 16000 {
			stdin.Close()
			_ = cmd.Wait()
			return fmt.Errorf("segment %d sample_rate=%d, only 16 kHz supported by replay", id, sampleRate)
		}
		if err := writePCM16LE(stdin, pcm); err != nil {
			stdin.Close()
			_ = cmd.Wait()
			return fmt.Errorf("pipe segment %d: %w", id, err)
		}
		if i < len(ids)-1 && gapSamples > 0 {
			if _, err := stdin.Write(gapBuf); err != nil {
				stdin.Close()
				_ = cmd.Wait()
				return fmt.Errorf("pipe gap: %w", err)
			}
		}
	}

	if err := stdin.Close(); err != nil {
		return fmt.Errorf("close paplay stdin: %w", err)
	}
	return cmd.Wait()
}

// writePCM16LE writes int16 samples as little-endian bytes — matches
// the `--format=s16le` paplay expects.
func writePCM16LE(w io.Writer, pcm []int16) error {
	buf := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	_, err := w.Write(buf)
	return err
}

func runRecallDrop() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	client := recall.NewClient(socket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	segs, err := client.List(ctx)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		fmt.Fprintln(os.Stderr, "no segments to drop")
		return nil
	}
	availableIDs := make([]int64, len(segs))
	for i, s := range segs {
		availableIDs[i] = s.ID
	}
	ids, err := recall.ParseSelection(recallDropIDs, availableIDs)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := client.Drop(ctx, id); err != nil {
			return fmt.Errorf("drop segment %d: %w", id, err)
		}
	}
	fmt.Fprintf(os.Stderr, "dropped %d segment(s)\n", len(ids))
	return nil
}

func runRecallStop() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	socket, err := recall.ResolveSocketPath(cfg.Recall.SocketPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := recall.NewClient(socket)
	if err := client.Shutdown(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "shutdown requested")
	return nil
}

// printSegmentTable renders a compact table of ring-buffer segments to
// w. Stable columns so users can predict what they're picking from.
// peak + rms let you spot noise-only segments at a glance: real speech
// has rms well above ~0.01 even at quiet volumes, while silence with
// a click-pop can have peak around 0.05 but rms under 0.003.
func printSegmentTable(w *os.File, segs []recall.SegmentInfo) {
	fmt.Fprintln(w, "  id    age        dur      peak    rms     transcript")
	fmt.Fprintln(w, "  ----  ---------  -------  ------  ------  ---------------------------------------")
	now := time.Now()
	for _, s := range segs {
		age := now.Sub(s.StartedAt).Round(time.Second)
		dur := time.Duration(s.DurationMS) * time.Millisecond
		preview := "(not transcribed yet)"
		if s.Transcribed {
			preview = truncateOneLine(s.CachedText, 80)
		}
		fmt.Fprintf(w, "  %4d  %9s  %6.2fs  %6.3f  %6.3f  %s\n",
			s.ID, age, dur.Seconds(), s.PeakLevel, s.AvgLevel, preview)
	}
}

func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// fzfAvailable reports whether the fzf binary is on PATH. Used to
// pick the default `recall pick` UI — fzf when present, table prompt
// otherwise — without the user needing a config flag.
func fzfAvailable() bool {
	_, err := exec.LookPath("fzf")
	return err == nil
}

// pickSegmentsWithFZF runs fzf as the selection UI. Each segment is
// piped in as a tab-separated "ID<TAB>ALIGNED_ROW" line; fzf's preview
// pane cats a per-segment file holding the full cached transcript (or
// a placeholder for segments that haven't been transcribed yet).
//
// Returns the selected IDs in the order the user marked them.
// Multi-select is on (tab marks); plain enter confirms the current
// line. Ctrl-P on the focused row plays the segment audio via
// `vocis recall replay` (requires paplay, same as the replay
// subcommand).
func pickSegmentsWithFZF(segs []recall.SegmentInfo) ([]int64, error) {
	fzfPath, err := exec.LookPath("fzf")
	if err != nil {
		return nil, fmt.Errorf("fzf required but not on PATH: %w", err)
	}

	// Resolve our own path so the Ctrl-P replay binding runs the same
	// binary the user invoked, not some other vocis that happens to be
	// earlier on PATH (matters when running from a build dir).
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve vocis executable path: %w", err)
	}

	// Temp dir for preview files. Cleaned up after fzf exits regardless
	// of outcome — we don't want to leave transcripts lying around.
	previewDir, err := os.MkdirTemp("", "vocis-recall-preview-*")
	if err != nil {
		return nil, fmt.Errorf("create preview dir: %w", err)
	}
	defer os.RemoveAll(previewDir)

	now := time.Now()
	var input strings.Builder
	for _, s := range segs {
		age := now.Sub(s.StartedAt).Round(time.Second)
		dur := time.Duration(s.DurationMS) * time.Millisecond
		short := "(not transcribed yet)"
		if s.Transcribed {
			short = truncateOneLine(s.CachedText, 120)
		}
		// Fixed-width columns so rows align inside fzf. Widths:
		//   id    5  (#12345)
		//   age   12 ("72h0m0s ago")
		//   dur   7  ("999.99s")
		//   text  remainder
		// Field 1 (tab-delimited) is the raw ID for fzf substitution;
		// fields 2+ are the visible row rendered via --with-nth=2..
		ageField := age.String() + " ago"
		row := fmt.Sprintf("#%-5d %-12s %6.2fs  %s", s.ID, ageField, dur.Seconds(), short)
		fmt.Fprintf(&input, "%d\t%s\n", s.ID, row)

		body := "(not transcribed yet — select this row + enter to transcribe)"
		if s.Transcribed {
			body = s.CachedText
		}
		header := fmt.Sprintf("segment #%d\nstarted: %s (%s ago)\nduration: %s\npeak: %.3f  rms: %.3f\n\n",
			s.ID, s.StartedAt.Format(time.RFC3339), age, dur, s.PeakLevel, s.AvgLevel)
		path := filepath.Join(previewDir, fmt.Sprintf("%d.txt", s.ID))
		if err := os.WriteFile(path, []byte(header+body), 0o600); err != nil {
			return nil, fmt.Errorf("write preview for segment %d: %w", s.ID, err)
		}
	}

	cmd := exec.Command(fzfPath,
		"--multi",
		"--no-sort",
		"--tac", // show newest segment first (we feed oldest-first)
		"--delimiter=\t",
		"--with-nth=2..",
		"--preview", fmt.Sprintf("cat %s/{1}.txt", previewDir),
		"--preview-window=down,50%,wrap",
		"--prompt=recall> ",
		"--header=tab=mark  enter=confirm  ctrl-p=play  esc=abort",
		"--bind", fmt.Sprintf("ctrl-p:execute(%s recall replay --ids={1})", self),
	)
	cmd.Stdin = strings.NewReader(input.String())
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 130 {
			// fzf convention: 130 = user cancelled (Ctrl-C / Esc). Treat
			// as empty selection rather than an error.
			return nil, nil
		}
		return nil, fmt.Errorf("fzf: %w", err)
	}

	var ids []int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		idStr, _, _ := strings.Cut(line, "\t")
		var id int64
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			return nil, fmt.Errorf("parse fzf output %q: %w", line, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// promptSegmentSelection reads a selection string from stdin and turns
// it into a concrete list of IDs to transcribe. Blank input defaults to
// the most recent segment, which is the common case.
func promptSegmentSelection(available []int64) ([]int64, error) {
	latest := available[len(available)-1]
	fmt.Fprintf(os.Stderr,
		"pick [default %d=latest; accepts id, range like 3-5, open range 3-, or \"all\"]: ", latest)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read pick: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return []int64{latest}, nil
	}
	return recall.ParseSelection(line, available)
}
