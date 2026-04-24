package recall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/config"
	"vocis/internal/recorder"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
	"vocis/internal/transcribe"
)

// Daemon owns the recall lifecycle: recorder → Silero VAD → segment
// ring buffer, plus a Unix-socket server that responds to list/
// transcribe/drop/status/shutdown requests from the `vocis recall`
// subcommands.
type Daemon struct {
	cfg    config.Config
	apiKey string

	ring *Ring

	socketPath string
	listener   net.Listener

	// Transcription is done by reusing the existing streaming client.
	// One per daemon; requests are serialized with a mutex so we don't
	// trample the Silero session or spawn parallel WebSocket dials.
	transcribeMu     sync.Mutex
	transcribeClient *transcribe.Client

	shutdownOnce sync.Once
	shutdown     chan struct{}
	wg           sync.WaitGroup
}

// DaemonOpts bundles the dependencies the daemon needs. apiKey is only
// required for cloud backends — Lemonade runs unauthenticated.
type DaemonOpts struct {
	Config config.Config
	APIKey string
}

// NewDaemon constructs a Daemon but does not start capture or open the
// socket yet — call Run for that.
func NewDaemon(opts DaemonOpts) *Daemon {
	ring := NewRing(opts.Config.Recall.MaxSegments,
		time.Duration(opts.Config.Recall.RetentionSeconds)*time.Second)
	return &Daemon{
		cfg:              opts.Config,
		apiKey:           opts.APIKey,
		ring:             ring,
		shutdown:         make(chan struct{}),
		transcribeClient: transcribe.New(opts.APIKey, opts.Config.Transcription, opts.Config.Streaming),
	}
}

// Run starts the capture goroutine and serves the socket until ctx is
// cancelled, a shutdown request is received, or the recorder errors
// out. Blocks until cleanup is done.
func (d *Daemon) Run(ctx context.Context) error {
	if d.cfg.Recording.SampleRate != 16000 {
		return fmt.Errorf("recall requires recording.sample_rate=16000 (current: %d) — Silero VAD is hard-wired to 16 kHz",
			d.cfg.Recording.SampleRate)
	}

	if err := transcribe.InitSilero(d.cfg.Streaming.OnnxruntimeLibrary); err != nil {
		return fmt.Errorf("init silero: %w", err)
	}

	if err := d.initPersistence(); err != nil {
		return fmt.Errorf("init persistence: %w", err)
	}

	path, err := ResolveSocketPath(d.cfg.Recall.SocketPath)
	if err != nil {
		return fmt.Errorf("resolve socket path: %w", err)
	}
	d.socketPath = path

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// If a stale socket is sitting around from a previous daemon crash,
	// remove it — bind will fail otherwise. If another daemon is live,
	// the follow-up listen will fail loudly on address-in-use (can't
	// bind, not silently overwrite).
	if err := removeIfStaleSocket(path); err != nil {
		return err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	d.listener = ln
	if err := os.Chmod(path, 0o600); err != nil {
		sessionlog.Warnf("recall: chmod socket: %v", err)
	}
	sessionlog.Infof("recall: listening on %s", path)

	captureCtx, cancelCapture := context.WithCancel(ctx)
	defer cancelCapture()

	captureErr := make(chan error, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		captureErr <- d.runCapture(captureCtx)
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.runAccept(ctx)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		sessionlog.Infof("recall: context cancelled, shutting down")
	case <-d.shutdown:
		sessionlog.Infof("recall: shutdown requested via socket")
	case err := <-captureErr:
		if err != nil {
			runErr = fmt.Errorf("capture loop: %w", err)
		}
	}

	_ = ln.Close()
	_ = os.Remove(path)
	cancelCapture()
	d.wg.Wait()
	return runErr
}

// runCapture streams mic audio through Silero VAD and appends completed
// segments to the ring buffer. Returns when ctx is cancelled or the
// recorder errors.
func (d *Daemon) runCapture(ctx context.Context) error {
	rec := recorder.New()
	recSession, err := rec.Start(ctx, d.cfg.Recording)
	if err != nil {
		return fmt.Errorf("start recorder: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = recSession.Stop(stopCtx)
		cancel()
		recSession.Cleanup()
	}()

	vad, err := transcribe.NewSileroVAD(
		d.cfg.Recall.MinSilenceMS,
		d.cfg.Recall.MinSpeechMS,
		d.cfg.Recall.MinUtteranceMS,
	)
	if err != nil {
		return fmt.Errorf("build silero vad: %w", err)
	}
	defer vad.Destroy()

	sampleRate := recSession.SampleRate()
	// Preroll holds the most-recent PrerollMS + MinSpeechMS of audio,
	// so when VAD eventually declares SpeechStarted we still have the
	// audio from slightly before the utterance began — Silero's
	// hysteresis needs the speech to already be present by the time it
	// reports the transition, so without preroll the segment would clip
	// the onset of the first word.
	prerollSamples := (d.cfg.Recall.PrerollMS + d.cfg.Recall.MinSpeechMS) * sampleRate / 1000
	preroll := newRing16(prerollSamples)

	maxSegmentSamples := d.cfg.Recall.MaxSegmentSeconds * sampleRate

	var active *Segment
	var activePeak int16
	var activeSumSq int64

	samples := recSession.Samples()
	for {
		select {
		case <-ctx.Done():
			return nil
		case chunk, ok := <-samples:
			if !ok {
				return nil
			}

			// Snapshot preroll BEFORE adding this chunk — we want the
			// segment to include audio from *before* SpeechStarted.
			prevPreroll := preroll.snapshot()
			preroll.push(chunk)

			event := vad.Feed(chunk)

			switch {
			case active == nil && event == transcribe.VADSpeechStarted:
				// Brand-new segment. Preroll gives us a bit of
				// pre-onset audio; this chunk is also speech.
				startedAt := time.Now().Add(
					-time.Duration(len(prevPreroll)+len(chunk)) * time.Second / time.Duration(sampleRate),
				)
				active = &Segment{
					StartedAt:  startedAt,
					SampleRate: sampleRate,
					PCM:        make([]int16, 0, len(prevPreroll)+len(chunk)+sampleRate),
				}
				active.PCM = append(active.PCM, prevPreroll...)
				active.PCM = append(active.PCM, chunk...)
				activePeak = peakAbs16(prevPreroll)
				if p := peakAbs16(chunk); p > activePeak {
					activePeak = p
				}
				activeSumSq = sumSquares16(prevPreroll) + sumSquares16(chunk)

			case active != nil:
				active.PCM = append(active.PCM, chunk...)
				if p := peakAbs16(chunk); p > activePeak {
					activePeak = p
				}
				activeSumSq += sumSquares16(chunk)
			}

			if active != nil && event == transcribe.VADSpeechStopped {
				d.finalizeActive(active, activePeak, activeSumSq, sampleRate, false)
				active = nil
				activePeak = 0
				activeSumSq = 0
			} else if active != nil && len(active.PCM) >= maxSegmentSamples {
				// Monologue with no pause — flush so the ring buffer
				// bound is enforceable and downstream transcription
				// stays within token/size limits. Start a fresh
				// segment that inherits the in-speech state; Silero
				// will naturally report SpeechStopped when the real
				// pause arrives.
				d.finalizeActive(active, activePeak, activeSumSq, sampleRate, true)
				active = &Segment{
					StartedAt:  time.Now(),
					SampleRate: sampleRate,
					PCM:        make([]int16, 0, sampleRate),
				}
				activePeak = 0
				activeSumSq = 0
			}
		}
	}
}

// finalizeActive stamps the final duration/peak/RMS onto the segment
// and, if it looks like real speech, adds it to the ring buffer. Emits
// a `vocis.recall.capture` trace span (a fresh root span, not attached
// to the daemon context) so each captured utterance shows up as its
// own trace in Jaeger. forceFlushed is true when MaxSegmentSeconds cut
// the segment short rather than a natural VAD-stopped event.
//
// Segments are dropped as silence/noise when peak < min_segment_peak
// (filters out below-noise-floor segments entirely) or RMS <
// min_segment_rms (filters out mostly-silent segments with the
// occasional click pop that peak alone would miss — the classic 24 s
// near-silence with one keyboard clack). Both are logged so users can
// see why a segment didn't land in the ring.
func (d *Daemon) finalizeActive(seg *Segment, peak int16, sumSq int64, sampleRate int, forceFlushed bool) {
	seg.Duration = time.Duration(len(seg.PCM)) * time.Second / time.Duration(sampleRate)
	seg.PeakLevel = float64(peak) / 32768.0
	if len(seg.PCM) > 0 {
		seg.AvgLevel = math.Sqrt(float64(sumSq)/float64(len(seg.PCM))) / 32768.0
	}

	var dropReason string
	switch {
	case seg.PeakLevel < d.cfg.Recall.MinSegmentPeak:
		dropReason = fmt.Sprintf("peak=%.4f < min_peak=%.4f", seg.PeakLevel, d.cfg.Recall.MinSegmentPeak)
	case seg.AvgLevel < d.cfg.Recall.MinSegmentRMS:
		dropReason = fmt.Sprintf("rms=%.4f < min_rms=%.4f", seg.AvgLevel, d.cfg.Recall.MinSegmentRMS)
	}

	var id int64
	if dropReason == "" {
		id = d.ring.Add(seg)
	}

	_, span := telemetry.StartSpan(context.Background(), "vocis.recall.capture",
		attribute.Int64("segment.id", id),
		attribute.Int("segment.duration_ms", int(seg.Duration/time.Millisecond)),
		attribute.Int("segment.sample_count", len(seg.PCM)),
		attribute.Int("segment.sample_rate", sampleRate),
		attribute.Float64("segment.peak_level", seg.PeakLevel),
		attribute.Float64("segment.avg_level", seg.AvgLevel),
		attribute.Float64("segment.min_peak_threshold", d.cfg.Recall.MinSegmentPeak),
		attribute.Float64("segment.min_rms_threshold", d.cfg.Recall.MinSegmentRMS),
		attribute.Bool("segment.force_flushed", forceFlushed),
		attribute.Bool("segment.dropped_as_silence", dropReason != ""),
		attribute.String("segment.drop_reason", dropReason),
	)
	telemetry.EndSpan(span, nil)

	if dropReason != "" {
		sessionlog.Infof("recall: dropped silence segment (%.2fs, peak=%.4f, rms=%.4f, force_flushed=%t, reason=%s)",
			seg.Duration.Seconds(), seg.PeakLevel, seg.AvgLevel, forceFlushed, dropReason)
		return
	}
	sessionlog.Infof("recall: captured segment #%d (%.2fs, peak=%.4f, rms=%.4f, force_flushed=%t)",
		id, seg.Duration.Seconds(), seg.PeakLevel, seg.AvgLevel, forceFlushed)
}

// sumSquares16 returns the sum of x*x over all samples as int64. Used
// for RMS energy tracking during capture — caller keeps the running
// total across chunks and divides by sample count at finalize time.
// Worst case int16 squared ≈ 1.07e9; at 16 kHz we'd need ≈8.5e9
// samples (~5.9 days) to overflow int64, so no saturation concern.
func sumSquares16(samples []int16) int64 {
	var s int64
	for _, x := range samples {
		v := int64(x)
		s += v * v
	}
	return s
}

// runAccept is the socket accept loop. Each connection handles a single
// request/response pair and closes — simpler than persistent framing.
func (d *Daemon) runAccept(ctx context.Context) {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			sessionlog.Warnf("recall: accept: %v", err)
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleConn(ctx, conn)
		}()
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Short deadline around the decode so a half-open peer can't wedge
	// the handler — after we have a request, the client may legitimately
	// wait for a long-running op (a big `recall last` batch can take
	// tens of minutes), so the deadline is cleared below.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	dec := json.NewDecoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		writeErr(conn, fmt.Errorf("decode request: %w", err))
		return
	}

	// Clear the deadline now that the request is in hand — a long batch
	// transcription must not be capped by a connection-level timer.
	_ = conn.SetDeadline(time.Time{})

	// Watch the idle connection for a client disconnect (Ctrl-C on the
	// CLI, process exit, etc.) and cancel the request context so the
	// in-flight op — which may be a Finalize blocked on Lemonade —
	// tears down its WebSocket promptly instead of running to
	// completion with no one to receive results.
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	go func() {
		// Clients don't send a second message on the same conn, so this
		// Read blocks until EOF (clean close) or an error (peer gone).
		// Either way, that's our signal to cancel.
		var buf [1]byte
		_, err := conn.Read(buf[:])
		cancelReq()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			sessionlog.Debugf("recall: conn disconnect watch: %v", err)
		}
	}()

	resp := d.dispatch(reqCtx, req)
	resp.Version = protocolVersion
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		sessionlog.Warnf("recall: encode response: %v", err)
	}
}

func (d *Daemon) dispatch(ctx context.Context, req Request) Response {
	switch req.Op {
	case OpList:
		return Response{Segments: d.listSegments()}
	case OpTranscribe:
		text, err := d.transcribeSegment(ctx, req.SegmentID, req.PostProcess)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Transcript: text}
	case OpTranscribeBatch:
		text, err := d.transcribeBatch(ctx, req.SegmentIDs, req.PostProcess)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Transcript: text}
	case OpDrop:
		d.ring.Drop(req.SegmentID)
		return Response{}
	case OpStatus:
		stats := d.ring.Stats()
		return Response{Stats: &StatsInfo{
			Count:       stats.Count,
			TotalSeen:   stats.TotalSeen,
			OldestAgeMS: stats.OldestAge.Milliseconds(),
			NewestAgeMS: stats.NewestAge.Milliseconds(),
			TotalFrames: stats.TotalFrames,
		}}
	case OpShutdown:
		d.shutdownOnce.Do(func() { close(d.shutdown) })
		return Response{}
	case OpGetAudio:
		seg, err := d.ring.Get(req.SegmentID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{
			AudioPCMBase64:  encodePCM16(seg.PCM),
			AudioSampleRate: seg.SampleRate,
		}
	default:
		return Response{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}

func (d *Daemon) listSegments() []SegmentInfo {
	segs := d.ring.List()
	out := make([]SegmentInfo, 0, len(segs))
	for _, s := range segs {
		out = append(out, SegmentInfo{
			ID:          s.ID,
			StartedAt:   s.StartedAt,
			DurationMS:  int(s.Duration / time.Millisecond),
			PeakLevel:   s.PeakLevel,
			AvgLevel:    s.AvgLevel,
			Transcribed: s.Transcript != "",
			CachedText:  s.Transcript,
		})
	}
	return out
}

// transcribeSegment feeds a segment's PCM through the realtime
// transcription pipeline by emulating a recorder's sample stream.
// Serialized with a mutex so we don't run parallel transports.
//
// ctx is the request-scoped context from handleConn — cancelling it
// (e.g. client disconnected) propagates into the dictation session so
// the Lemonade WebSocket tears down promptly instead of running to
// completion with no one to receive results. The OTel span is still
// started under ctx so it remains a root span (handleConn's ctx carries
// no existing span), giving each pick its own trace in Jaeger.
func (d *Daemon) transcribeSegment(ctx context.Context, id int64, postprocess bool) (string, error) {
	d.transcribeMu.Lock()
	defer d.transcribeMu.Unlock()

	goroutinesBefore := runtime.NumGoroutine()

	spanCtx, span := telemetry.StartSpan(ctx, "vocis.recall.transcribe",
		attribute.Int64("segment.id", id),
		attribute.Bool("postprocess", postprocess),
	)
	var err error
	defer func() {
		span.SetAttributes(attribute.Int("runtime.goroutines_delta",
			runtime.NumGoroutine()-goroutinesBefore))
		telemetry.EndSpan(span, err)
	}()

	seg, err := d.ring.Get(id)
	if err != nil {
		return "", err
	}
	span.SetAttributes(
		attribute.Int("segment.duration_ms", int(seg.Duration/time.Millisecond)),
		attribute.Int("segment.sample_count", len(seg.PCM)),
		attribute.Float64("segment.peak_level", seg.PeakLevel),
	)

	if seg.Transcript != "" && !postprocess {
		span.SetAttributes(attribute.Bool("cache_hit", true))
		return seg.Transcript, nil
	}
	span.SetAttributes(attribute.Bool("cache_hit", false))

	sampleRate := seg.SampleRate

	dictCtx, cancel := context.WithTimeout(spanCtx, 60*time.Second)
	defer cancel()

	// Buffered so the feed goroutine doesn't block on the first send
	// before StartDictation's pump is ready to read.
	samples := make(chan []int16, 8)

	session, err := d.transcribeClient.StartDictation(dictCtx, transcribe.DictationOpts{
		SampleRate: sampleRate,
		Channels:   d.cfg.Recording.Channels,
		Samples:    samples,
		// Segment duration is known upfront — feed it to waitForFinal
		// so the post-commit budget scales with the audio being
		// transcribed instead of falling back to the 15 s floor.
		ExpectedAudioMS: int(seg.Duration / time.Millisecond),
	})
	if err != nil {
		return "", fmt.Errorf("start dictation: %w", err)
	}

	// Drain events so the dictation pump doesn't stall on a full
	// channel. DictationSession doesn't close its events channel on
	// Finalize — a naive `for range session.Events()` would block
	// forever, leaking one goroutine per pick. Bind to dictCtx so the
	// drain exits as soon as the transcribe call returns.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-dictCtx.Done():
				return
			case _, ok := <-session.Events():
				if !ok {
					return
				}
			}
		}
	}()

	// Feed the segment PCM as a pretend live stream. 2048 samples per
	// chunk (~128 ms at 16 kHz) matches the rough granularity the
	// recorder emits and keeps the transport's write bursts modest.
	// feedCtx must be parented under dictCtx so a dictation timeout or
	// consumer failure releases the send on `samples` — spanCtx is the
	// root OTel context and never cancels, which would deadlock the
	// feed goroutine holding transcribeMu.
	feedCtx, feedSpan := telemetry.StartSpan(dictCtx, "vocis.recall.transcribe.feed",
		attribute.Int("feed.chunk_samples", 2048),
	)
	const feedChunk = 2048
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(samples)
		for i := 0; i < len(seg.PCM); i += feedChunk {
			end := i + feedChunk
			if end > len(seg.PCM) {
				end = len(seg.PCM)
			}
			chunk := make([]int16, end-i)
			copy(chunk, seg.PCM[i:end])
			select {
			case <-feedCtx.Done():
				return
			case samples <- chunk:
			}
		}
	}()

	_, finalizeSpan := telemetry.StartSpan(spanCtx, "vocis.recall.transcribe.finalize")
	result, finalizeErr := session.Finalize(dictCtx)
	telemetry.EndSpan(finalizeSpan, finalizeErr)
	<-feedDone
	telemetry.EndSpan(feedSpan, nil)

	if finalizeErr != nil {
		err = fmt.Errorf("finalize: %w", finalizeErr)
		return "", err
	}

	text := result.Text
	span.SetAttributes(attribute.Int("transcript.length", len(text)))

	if postprocess && d.cfg.PostProcess.Enabled {
		_, ppSpan := telemetry.StartSpan(spanCtx, "vocis.recall.transcribe.postprocess")
		ppCtx, ppCancel := context.WithTimeout(context.Background(),
			time.Duration(d.cfg.PostProcess.TotalTimeoutSec)*time.Second)
		pp := d.transcribeClient.PostProcess(ppCtx, d.cfg.PostProcess, text, nil)
		ppCancel()
		if !pp.Skipped {
			text = pp.Text
		}
		ppSpan.SetAttributes(
			attribute.Bool("postprocess.skipped", pp.Skipped),
			attribute.Int("postprocess.text_length", len(text)),
		)
		telemetry.EndSpan(ppSpan, nil)
	}

	d.ring.SetTranscript(id, text)

	// Wait for the drain to exit so a tight pick loop doesn't leave
	// stragglers behind. dictCtx will already be cancelled by the
	// deferred cancel above.
	cancel()
	<-drainDone

	goroutinesAfter := runtime.NumGoroutine()
	sessionlog.Infof("recall: transcribe id=%d goroutines %d→%d (Δ=%+d)",
		id, goroutinesBefore, goroutinesAfter, goroutinesAfter-goroutinesBefore)
	return text, nil
}

// SocketPath returns the path the daemon is (or was) listening on.
// Useful for tests and status output.
func (d *Daemon) SocketPath() string { return d.socketPath }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ring16 is a simple int16 ring buffer used for the preroll window.
// Not safe for concurrent use — owned entirely by the capture loop.
type ring16 struct {
	buf    []int16
	head   int
	filled int
}

func newRing16(capacity int) *ring16 {
	if capacity <= 0 {
		return &ring16{}
	}
	return &ring16{buf: make([]int16, capacity)}
}

func (r *ring16) push(samples []int16) {
	if len(r.buf) == 0 {
		return
	}
	for _, s := range samples {
		r.buf[r.head] = s
		r.head = (r.head + 1) % len(r.buf)
		if r.filled < len(r.buf) {
			r.filled++
		}
	}
}

// snapshot returns the currently-retained samples in chronological order.
func (r *ring16) snapshot() []int16 {
	if r.filled == 0 {
		return nil
	}
	out := make([]int16, r.filled)
	start := (r.head - r.filled + len(r.buf)) % len(r.buf)
	for i := 0; i < r.filled; i++ {
		out[i] = r.buf[(start+i)%len(r.buf)]
	}
	return out
}

func peakAbs16(samples []int16) int16 {
	var peak int16
	for _, s := range samples {
		v := s
		if v == math.MinInt16 {
			v = math.MaxInt16
		} else if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

func writeErr(w io.Writer, err error) {
	_ = json.NewEncoder(w).Encode(Response{Version: protocolVersion, Error: err.Error()})
}

// initPersistence opens the configured persist directory when the user
// has set recall.persist.mode=disk, reloads segments that still fit
// under the current retention, deletes the ones that don't, and
// attaches the persister so future Add/Drop operations mirror to disk.
// In the default in_memory mode this is a no-op — the ring stays
// entirely in memory and nothing touches disk.
func (d *Daemon) initPersistence() error {
	if d.cfg.Recall.Persist.Mode != config.RecallPersistDisk {
		return nil
	}
	dir := d.cfg.Recall.Persist.Dir
	if dir == "" {
		return fmt.Errorf("recall.persist.mode=disk but recall.persist.dir is empty")
	}
	persister, err := NewFilePersister(dir)
	if err != nil {
		return err
	}
	loaded, err := persister.Load()
	if err != nil {
		return fmt.Errorf("load persisted segments: %w", err)
	}

	// Apply current retention to the loaded set. Segments outside the
	// window are dropped from disk now — the user changed retention
	// since the last run and those files are no longer wanted.
	var keep []*Segment
	var stale []*Segment
	cutoff := time.Time{}
	if d.cfg.Recall.RetentionSeconds > 0 {
		cutoff = time.Now().Add(-time.Duration(d.cfg.Recall.RetentionSeconds) * time.Second)
	}
	for _, s := range loaded {
		if !cutoff.IsZero() && s.StartedAt.Before(cutoff) {
			stale = append(stale, s)
			continue
		}
		keep = append(keep, s)
	}

	// Apply max-count bound too — oldest first.
	if d.cfg.Recall.MaxSegments > 0 && len(keep) > d.cfg.Recall.MaxSegments {
		drop := len(keep) - d.cfg.Recall.MaxSegments
		stale = append(stale, keep[:drop]...)
		keep = keep[drop:]
	}

	for _, s := range stale {
		if err := persister.Delete(s.ID); err != nil {
			sessionlog.Warnf("recall: prune persisted segment %d: %v", s.ID, err)
		}
	}

	d.ring.Reload(keep)
	d.ring.SetPersister(persister)

	sessionlog.Infof("recall: persistence dir=%s, reloaded=%d segments, pruned=%d",
		persister.Dir(), len(keep), len(stale))
	return nil
}

// removeIfStaleSocket deletes a leftover socket file from a previous
// daemon run. If a live daemon still holds the socket, Dial will
// succeed and we refuse to clobber it — the caller will get a clear
// error.
func removeIfStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket — refusing to remove", path)
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("another vocis recall daemon is already listening on %s", path)
	}
	return os.Remove(path)
}
