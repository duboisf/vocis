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

// concatSegmentPCM joins a sequence of segments into a single PCM
// stream with `gapMS` of silence (zero-valued int16 samples) inserted
// between each pair. Returns the concatenated PCM, the common sample
// rate, or an error.
//
// Fails if segments have different sample rates (a batch must be a
// single realtime session, which is locked to one rate), if the input
// is empty, or if the total audio duration would exceed maxTotalSeconds
// (when > 0). The duration cap is the safety net for `recall last`:
// without it, a ring buffer with days of retention could be silently
// concatenated into a multi-hour blob.
func concatSegmentPCM(segs []*Segment, gapMS int, maxTotalSeconds int) ([]int16, int, error) {
	if len(segs) == 0 {
		return nil, 0, fmt.Errorf("no segments to concatenate")
	}
	sampleRate := segs[0].SampleRate
	if sampleRate <= 0 {
		return nil, 0, fmt.Errorf("segment %d has invalid sample_rate=%d", segs[0].ID, sampleRate)
	}
	for _, s := range segs[1:] {
		if s.SampleRate != sampleRate {
			return nil, 0, fmt.Errorf("segments have mixed sample rates (%d vs %d at id=%d) — batch requires one rate",
				sampleRate, s.SampleRate, s.ID)
		}
	}

	gapSamples := gapMS * sampleRate / 1000
	total := 0
	for _, s := range segs {
		total += len(s.PCM)
	}
	total += gapSamples * (len(segs) - 1)

	if maxTotalSeconds > 0 {
		maxSamples := maxTotalSeconds * sampleRate
		if total > maxSamples {
			return nil, 0, fmt.Errorf("concatenated audio %.1fs exceeds batch_max_seconds=%d — drop --last duration or raise the cap",
				float64(total)/float64(sampleRate), maxTotalSeconds)
		}
	}

	out := make([]int16, 0, total)
	for i, s := range segs {
		if i > 0 && gapSamples > 0 {
			// Append gapSamples zeros — literal silence between segments.
			out = append(out, make([]int16, gapSamples)...)
		}
		out = append(out, s.PCM...)
	}
	return out, sampleRate, nil
}

// transcribeBatch fetches each segment by ID, concatenates their PCM
// with a configured silence gap, and feeds the result through the
// realtime transcription pipeline as a single dictation session. The
// joint transcript is returned; individual segments' caches are NOT
// updated — a batch result is a different artifact from per-segment
// transcriptions, so clobbering per-segment caches would be wrong.
//
// Serialized on the same transcribeMu as single-segment transcription
// to avoid parallel transports.
//
// ctx is the request-scoped context from handleConn — client
// disconnection (Ctrl-C on the CLI) propagates through and tears down
// the Lemonade WebSocket so the model stops being fed audio no one is
// going to receive. A batch can legitimately take tens of minutes on a
// local model, so there is no internal wall-clock timeout — the user
// controls lifetime via Ctrl-C, and daemon shutdown also cancels.
func (d *Daemon) transcribeBatch(ctx context.Context, ids []int64, postprocess bool) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("no segment ids provided")
	}

	d.transcribeMu.Lock()
	defer d.transcribeMu.Unlock()

	goroutinesBefore := runtime.NumGoroutine()

	idsCopy := append([]int64(nil), ids...)

	spanCtx, span := telemetry.StartSpan(ctx, "vocis.recall.transcribe_batch",
		attribute.Int("segments.count", len(ids)),
		attribute.Int64Slice("segments.ids", idsCopy),
		attribute.Bool("postprocess", postprocess),
		attribute.Int("audio.gap_ms", d.cfg.Recall.BatchGapMS),
		attribute.Int("audio.max_seconds", d.cfg.Recall.BatchMaxSeconds),
	)
	var err error
	defer func() {
		span.SetAttributes(attribute.Int("runtime.goroutines_delta",
			runtime.NumGoroutine()-goroutinesBefore))
		telemetry.EndSpan(span, err)
	}()

	segs := make([]*Segment, 0, len(ids))
	for _, id := range ids {
		seg, getErr := d.ring.Get(id)
		if getErr != nil {
			err = fmt.Errorf("segment %d: %w", id, getErr)
			return "", err
		}
		segs = append(segs, seg)
	}

	pcm, sampleRate, catErr := concatSegmentPCM(segs,
		d.cfg.Recall.BatchGapMS, d.cfg.Recall.BatchMaxSeconds)
	if catErr != nil {
		err = catErr
		return "", err
	}
	totalMS := len(pcm) * 1000 / sampleRate
	span.SetAttributes(
		attribute.Int("audio.total_ms", totalMS),
		attribute.Int("audio.sample_count", len(pcm)),
		attribute.Int("audio.sample_rate", sampleRate),
	)
	sessionlog.Infof("recall: batch transcribe ids=%v segments=%d total=%.2fs gap=%dms postprocess=%t",
		idsCopy, len(ids), float64(totalMS)/1000.0, d.cfg.Recall.BatchGapMS, postprocess)

	// No internal wall-clock timeout: batches of 20+ minutes on a local
	// 2B model can legitimately take tens of minutes, and a fixed cap
	// was worse than useless (it cut off valid work without actually
	// stopping Lemonade, because the cap fired from Background while
	// the WS stayed open). Lifetime is driven by the caller's ctx —
	// client Ctrl-C or daemon shutdown both cancel cleanly and
	// Finalize then closes the WS.
	dictCtx, cancel := context.WithCancel(spanCtx)
	defer cancel()

	samples := make(chan []int16, 8)
	session, startErr := d.transcribeClient.StartDictation(dictCtx, transcribe.DictationOpts{
		SampleRate: sampleRate,
		Channels:   d.cfg.Recording.Channels,
		Samples:    samples,
		// Let waitForFinal scale its post-commit budget to the audio
		// we're about to feed — otherwise the 15 s wait_final floor
		// fires before a local model has time to transcribe anything
		// meaningful on a multi-minute batch.
		ExpectedAudioMS: totalMS,
	})
	if startErr != nil {
		err = fmt.Errorf("start dictation: %w", startErr)
		return "", err
	}

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

	// Parent under dictCtx, not spanCtx: if the dictation session times
	// out or its consumer dies mid-feed, nothing drains `samples` and
	// `samples <- chunk` blocks forever. dictCtx cancellation is our
	// release valve — spanCtx is the root OTel context and never
	// cancels, which would leak this goroutine and deadlock the
	// transcribeMu-holding call.
	feedCtx, feedSpan := telemetry.StartSpan(dictCtx, "vocis.recall.transcribe_batch.feed",
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

	_, finalizeSpan := telemetry.StartSpan(spanCtx, "vocis.recall.transcribe_batch.finalize")
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
		_, ppSpan := telemetry.StartSpan(spanCtx, "vocis.recall.transcribe_batch.postprocess")
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

	cancel()
	<-drainDone

	goroutinesAfter := runtime.NumGoroutine()
	sessionlog.Infof("recall: batch transcribe done ids=%v text_len=%d goroutines %d→%d (Δ=%+d)",
		idsCopy, len(text), goroutinesBefore, goroutinesAfter, goroutinesAfter-goroutinesBefore)

	return text, nil
}
