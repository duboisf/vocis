package recall

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
	"vocis/internal/transcribe"
)

// SegmentIDsWithinWindow returns segment IDs whose StartedAt is within
// [now-window, now], preserving chronological (oldest-first) order.
// Exported so the CLI can filter the ring buffer before asking the
// daemon for a batch transcription.
func SegmentIDsWithinWindow(segs []SegmentInfo, now time.Time, window time.Duration) []int64 {
	cutoff := now.Add(-window)
	ids := make([]int64, 0, len(segs))
	for _, s := range segs {
		if s.StartedAt.Before(cutoff) {
			continue
		}
		ids = append(ids, s.ID)
	}
	return ids
}

// runDictation feeds a prepared PCM buffer through the realtime
// transcription pipeline as a single dictation session. Shared by
// transcribeSegment (single pick) and transcribeBatch (recall last).
// Everything from "open session" through "drain final transcript and
// optionally postprocess" is identical and lives here.
//
// `timeout` controls the dictation context: 0 means "no internal
// timeout, lifetime driven by spanCtx" (batches can legitimately take
// tens of minutes on a local model); positive means "cap at this".
// spanPrefix names the child spans (.feed / .finalize / .postprocess)
// under whatever root span the caller already opened.
func (d *Daemon) runDictation(
	spanCtx context.Context,
	timeout time.Duration,
	pcm []int16,
	sampleRate int,
	totalMS int,
	spanPrefix string,
	postprocess bool,
) (string, error) {
	var dictCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		dictCtx, cancel = context.WithTimeout(spanCtx, timeout)
	} else {
		dictCtx, cancel = context.WithCancel(spanCtx)
	}
	defer cancel()

	samples := make(chan []int16, 8)
	session, err := d.transcribeClient.StartDictation(dictCtx, transcribe.DictationOpts{
		SampleRate: sampleRate,
		Channels:   d.cfg.Recording.Channels,
		Samples:    samples,
		// Let waitForCompletion scale its post-commit budget to the
		// audio we're about to feed — otherwise the 15 s wait_final
		// floor fires before a local model has time to transcribe
		// anything meaningful on a multi-minute batch.
		ExpectedAudioMS: totalMS,
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
	// feedCtx is parented under dictCtx, not spanCtx: if the dictation
	// session times out or its consumer dies mid-feed, nothing drains
	// `samples` and `samples <- chunk` blocks forever. dictCtx
	// cancellation is our release valve — spanCtx is the root OTel
	// context and never cancels, which would leak this goroutine.
	feedCtx, feedSpan := telemetry.StartSpan(dictCtx, spanPrefix+".feed",
		attribute.Int("feed.chunk_samples", 2048),
		attribute.Int("feed.total_samples", len(pcm)),
	)
	const feedChunk = 2048
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(samples)
		for i := 0; i < len(pcm); i += feedChunk {
			end := i + feedChunk
			if end > len(pcm) {
				end = len(pcm)
			}
			chunk := make([]int16, end-i)
			copy(chunk, pcm[i:end])
			select {
			case <-feedCtx.Done():
				return
			case samples <- chunk:
			}
		}
	}()

	_, finalizeSpan := telemetry.StartSpan(spanCtx, spanPrefix+".finalize")
	result, finalizeErr := session.Finalize(dictCtx)
	telemetry.EndSpan(finalizeSpan, finalizeErr)
	<-feedDone
	telemetry.EndSpan(feedSpan, nil)

	if finalizeErr != nil {
		return "", fmt.Errorf("finalize: %w", finalizeErr)
	}

	text := result.Text

	if postprocess && d.cfg.PostProcess.Enabled {
		_, ppSpan := telemetry.StartSpan(spanCtx, spanPrefix+".postprocess")
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

	// Wait for the drain to exit so a tight pick loop doesn't leave
	// stragglers behind. Cancel explicitly first — session.Events()
	// doesn't close on Finalize, so the drain is blocked on
	// dictCtx.Done() and we'd deadlock if we just waited.
	cancel()
	<-drainDone

	return text, nil
}

// transcribeBatch fetches each segment by ID and sends them as ONE
// /chat/completions request with each segment as a labelled
// input_audio part. The model returns one line per segment in the
// form `HH:MM:SS\t<transcript>` per `transcription.batch_prompt`. The
// joint response is returned; individual segments' caches are NOT
// updated — a batch result is a different artifact from per-segment
// transcriptions, so clobbering per-segment caches would be wrong.
//
// The `postprocess` flag is ignored: the batch prompt itself produces
// cleaned text, and post-processing would mangle the timestamped line
// format the user is relying on. A warn-log fires when the caller
// passes true so the surprising no-op is visible.
//
// Concurrent calls with transcribeSegment are fine: http.Client is
// concurrency-safe and Lemonade schedules requests server-side.
//
// ctx is the request-scoped context from handleConn — client
// disconnection (Ctrl-C on the CLI) propagates through and aborts the
// HTTP POST. A batch can legitimately take a while on a local model,
// so there is no internal wall-clock timeout.
func (d *Daemon) transcribeBatch(ctx context.Context, ids []int64, postprocess bool) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("no segment ids provided")
	}

	goroutinesBefore := runtime.NumGoroutine()
	idsCopy := append([]int64(nil), ids...)

	spanCtx, span := telemetry.StartSpan(ctx, "vocis.recall.transcribe_batch",
		attribute.Int("segments.count", len(ids)),
		attribute.Int64Slice("segments.ids", idsCopy),
	)
	var err error
	defer func() {
		span.SetAttributes(attribute.Int("runtime.goroutines_delta",
			runtime.NumGoroutine()-goroutinesBefore))
		telemetry.EndSpan(span, err)
	}()

	if postprocess {
		sessionlog.Warnf("recall: batch transcribe ignoring postprocess=true — batch prompt already produces cleaned text")
	}

	batch := make([]transcribe.BatchSegment, 0, len(ids))
	var totalMS int
	for _, id := range ids {
		seg, getErr := d.ring.Get(id)
		if getErr != nil {
			err = fmt.Errorf("segment %d: %w", id, getErr)
			return "", err
		}
		batch = append(batch, transcribe.BatchSegment{
			PCM:        seg.PCM,
			SampleRate: seg.SampleRate,
			CapturedAt: seg.StartedAt,
		})
		totalMS += int(seg.Duration / time.Millisecond)
	}
	span.SetAttributes(attribute.Int("audio.total_ms", totalMS))
	sessionlog.Infof("recall: batch transcribe ids=%v segments=%d total=%.2fs (one-shot multi-clip POST)",
		idsCopy, len(ids), float64(totalMS)/1000.0)

	text, runErr := d.transcribeClient.TranscribeBatchAudios(spanCtx, batch)
	if runErr != nil {
		err = runErr
		return "", err
	}
	span.SetAttributes(attribute.Int("transcript.length", len(text)))

	reportGoroutineDelta(fmt.Sprintf("batch transcribe done ids=%v text_len=%d", idsCopy, len(text)), goroutinesBefore)
	return text, nil
}
