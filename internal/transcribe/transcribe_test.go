package transcribe

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)


func TestPCMEncoderUpsamplesAndDownmixes(t *testing.T) {
	t.Parallel()

	encoder := newPCMEncoder(16000, 24000, 2)
	got := encoder.Encode([]int16{
		1000, 3000,
		-1000, -3000,
		2000, 4000,
		-2000, -4000,
	})

	gotSamples := make([]int16, len(got)/2)
	for i := range gotSamples {
		gotSamples[i] = int16(binary.LittleEndian.Uint16(got[i*2:]))
	}
	if len(gotSamples) != 6 {
		t.Fatalf("encoded sample count = %d, want 6", len(gotSamples))
	}
	want := []int16{2000, -2000, -2000, 3000, -3000, -3000}
	for i := range want {
		if gotSamples[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, gotSamples[i], want[i])
		}
	}
}

func TestFormatRealtimeErrorMarksEmptyCommit(t *testing.T) {
	t.Parallel()

	err := formatRealtimeError("input_audio_buffer_commit_empty", "buffer too small")
	if !errors.Is(err, ErrInputAudioBufferCommitEmpty) {
		t.Fatalf("errors.Is(err, ErrInputAudioBufferCommitEmpty) = false, err=%v", err)
	}
}

func TestReconcileCompletedTranscriptKeepsFullTurnWhenCompletedIsSuffix(t *testing.T) {
	t.Parallel()

	got := reconcileCompletedTranscript("hello world", "world")
	if got != "hello world" {
		t.Fatalf("reconcileCompletedTranscript = %q, want hello world", got)
	}
}

func TestReconcileCompletedTranscriptUsesCompletedWhenItIsIndependent(t *testing.T) {
	t.Parallel()

	got := reconcileCompletedTranscript("hello world", "goodbye world")
	if got != "goodbye world" {
		t.Fatalf("reconcileCompletedTranscript = %q, want goodbye world", got)
	}
}

func TestDictationSessionHandleStreamEventEmitsLiveSegment(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events: make(chan DictationEvent, 1),
		finals: make(chan finalResult, 1),
	}
	session.liveSegments.Store(true)

	err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventFinal,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	select {
	case event := <-session.events:
		if event.Type != DictationEventSegment {
			t.Fatalf("event.Type = %q, want segment", event.Type)
		}
		if event.Text != "hello world" {
			t.Fatalf("event.Text = %q, want hello world", event.Text)
		}
	default:
		t.Fatal("expected live segment event")
	}

	select {
	case result := <-session.finals:
		t.Fatalf("unexpected queued final: %+v", result)
	default:
	}
}

func TestDictationSessionDropsHallucinatedFinal(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events:               make(chan DictationEvent, 1),
		finals:               make(chan finalResult, 4),
		hallucinationFilters: buildHallucinationSet([]string{"Thank you.", "Bye."}),
	}
	session.liveSegments.Store(true)

	for _, text := range []string{"Thank you.", "thank you.", "  Thank you.  ", "BYE."} {
		err := session.handleStreamEvent(StreamEvent{Type: StreamEventFinal, Text: text})
		if err != nil {
			t.Fatalf("handleStreamEvent(%q): %v", text, err)
		}
		select {
		case evt := <-session.events:
			t.Fatalf("hallucination %q leaked as event %+v", text, evt)
		default:
		}
		// No empty final should be pushed for hallucinations: Stream's
		// pending-transcription counter has already been decremented by
		// the time we get here, so HasInflightWork goes false naturally
		// — waitForCompletion will exit on its next loop without us
		// nudging the channel.
		select {
		case r := <-session.finals:
			t.Fatalf("hallucination %q leaked as final %+v during live segments", text, r)
		default:
		}
	}

	// Legitimate final must still pass through.
	if err := session.handleStreamEvent(StreamEvent{Type: StreamEventFinal, Text: "real content"}); err != nil {
		t.Fatalf("handleStreamEvent real: %v", err)
	}
	select {
	case evt := <-session.events:
		if evt.Type != DictationEventSegment || evt.Text != "real content" {
			t.Fatalf("real final = %+v, want segment 'real content'", evt)
		}
	default:
		t.Fatal("real final dropped")
	}
}

// TestDictationSessionHallucinationAfterFinalizePushesEmptyMarker
// reproduces the timeout bug observed in session 20260430-154129 (the
// "Hey, by the way..." dictation): when Whisper transcribes the last
// server-VAD segment as a stock filter phrase like "Thank you.",
// handleStreamEvent must push an empty finalResult so that
// waitForCompletion's blocking select wakes up and observes
// HasInflightWork going false on the next iteration. Without the
// nudge, the wait loop sleeps until its full wait_final_seconds
// timeout (15 s in the user's session log) — paying a perceptible
// "Wrapping up..." stall on every dictation that ends in a
// hallucination-shaped trailing segment.
//
// An earlier version of this test asserted the OPPOSITE — that no
// marker should be pushed because "the drain loop polls
// HasInflightWork." That assumption was wrong: the drain loop only
// polls HasInflightWork at the TOP of each iteration, after either
// receiving on s.finals or the timeout firing. With nothing on
// s.finals, the select sleeps for the full timeout. The fix and the
// test were inverted together.
func TestDictationSessionHallucinationAfterFinalizePushesEmptyMarker(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events:               make(chan DictationEvent, 1),
		finals:               make(chan finalResult, 1),
		hallucinationFilters: buildHallucinationSet([]string{"Thank you."}),
	}
	// liveSegments = false simulates post-Finalize state.

	if err := session.handleStreamEvent(StreamEvent{Type: StreamEventFinal, Text: "Thank you."}); err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}
	select {
	case r := <-session.finals:
		if r.err != nil || strings.TrimSpace(r.text) != "" {
			t.Fatalf("hallucination drop pushed non-empty marker %+v — should be empty (text=\"\", err=nil) so applyResult is a no-op", r)
		}
	default:
		t.Fatal("hallucination drop in post-finalize phase did NOT push an empty marker — waitForCompletion's select will sleep until its full wait_final_seconds timeout instead of exiting promptly")
	}
}

// TestWaitForCompletionExitsPromptlyAfterHallucinationDrop is the
// integration-shaped check on the same bug: it drives the actual
// waitForCompletion drain loop with two server-VAD segments where
// the second one transcribes to a filter phrase. Before the fix the
// loop blocked the full waitFinalFloorSeconds (here 2 s) waiting
// for a final that would never come, then exited via timeout; after
// the fix it exits via "no_inflight_work" within tens of ms.
func TestWaitForCompletionExitsPromptlyAfterHallucinationDrop(t *testing.T) {
	t.Parallel()

	stream := &Stream{stats: newStats()}
	stream.stats.SpeechStartedCount = 2
	stream.stats.InboundCounts["conversation.item.input_audio_transcription.completed"] = 0

	session := &DictationSession{
		events:                make(chan DictationEvent, 4),
		finals:                make(chan finalResult, 4),
		waitFinalFloorSeconds: 2,
		hallucinationFilters:  buildHallucinationSet([]string{"Thank you."}),
	}
	session.stream.Store(stream)
	// liveSegments=false — we're past Finalize.

	go func() {
		// First segment: real content, mirrors Stream.handleMessage's
		// counter-then-event ordering.
		time.Sleep(20 * time.Millisecond)
		stream.statsMu.Lock()
		stream.stats.InboundCounts["conversation.item.input_audio_transcription.completed"]++
		stream.statsMu.Unlock()
		_ = session.handleStreamEvent(StreamEvent{Type: StreamEventFinal, Text: "real content."})

		// Second segment: hallucination — counter still ticks (it's a
		// real `transcription.completed` on the wire), then
		// handleStreamEvent silently drops the text.
		time.Sleep(20 * time.Millisecond)
		stream.statsMu.Lock()
		stream.stats.InboundCounts["conversation.item.input_audio_transcription.completed"]++
		stream.statsMu.Unlock()
		_ = session.handleStreamEvent(StreamEvent{Type: StreamEventFinal, Text: "Thank you."})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	got, err := session.waitForCompletion(ctx, stream, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if !strings.Contains(got, "real content") {
		t.Fatalf("waitForCompletion = %q, want it to contain 'real content'", got)
	}
	// The 2 s floor is the bug's signature — hitting or exceeding it
	// means the select slept until timeout instead of waking on the
	// hallucination drop. 500 ms gives generous slack for goroutine
	// scheduling on slow CI without masking the bug.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitForCompletion took %s after hallucination drop — bug: empty-marker nudge missing, loop slept until floor timeout (2 s)", elapsed.Round(time.Millisecond))
	}
}

// TestDictationSessionTrailingSkippedDoesNotPush pins the post-refactor
// contract for `input_audio_buffer.cleared`: it is now diagnostic-only.
// waitForCompletion's drain loop polls Stream.HasInflightWork to decide
// when finalize is done; the trailing-skipped handler must NOT push to
// s.finals regardless of partial state, because doing so could either
// (a) discard an in-flight delta (yesterday's bug), or
// (b) discard a server-VAD segment whose .completed hasn't arrived yet
//     (today's bug — a speech_started/_stopped pair with no delta yet).
// Both classes are handled by the drain loop; this handler stays out of
// the way.
func TestDictationSessionTrailingSkippedDoesNotPush(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events: make(chan DictationEvent, 4),
		finals: make(chan finalResult, 1),
	}
	// liveSegments = false simulates post-Finalize state.

	// Even with a delta on the wire, trailing-skipped must not push.
	if err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventPartial,
		Text: " would have its",
	}); err != nil {
		t.Fatalf("handleStreamEvent partial: %v", err)
	}
	if err := session.handleStreamEvent(StreamEvent{Type: StreamEventTrailingSkipped}); err != nil {
		t.Fatalf("handleStreamEvent trailing-skipped: %v", err)
	}
	select {
	case r := <-session.finals:
		t.Fatalf("trailing-skipped leaked a final %+v — must be diagnostic-only", r)
	default:
	}

	// And with no partial either (today's bug shape).
	session2 := &DictationSession{
		events: make(chan DictationEvent, 4),
		finals: make(chan finalResult, 1),
	}
	if err := session2.handleStreamEvent(StreamEvent{Type: StreamEventTrailingSkipped}); err != nil {
		t.Fatalf("handleStreamEvent trailing-skipped (no partial): %v", err)
	}
	select {
	case r := <-session2.finals:
		t.Fatalf("trailing-skipped without partial still leaked a final %+v — must be diagnostic-only", r)
	default:
	}

	// The eventual completion is what populates the channel.
	if err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventFinal,
		Text: "would have its environment",
	}); err != nil {
		t.Fatalf("handleStreamEvent final: %v", err)
	}
	select {
	case r := <-session.finals:
		if r.err != nil {
			t.Fatalf("final err = %v", r.err)
		}
		if r.text != "would have its environment" {
			t.Fatalf("final text = %q, want %q", r.text, "would have its environment")
		}
	default:
		t.Fatal("expected real final after completion event")
	}
}

func TestDictationSessionHandleStreamEventQueuesFinalAfterLiveDisabled(t *testing.T) {
	t.Parallel()

	session := &DictationSession{
		events: make(chan DictationEvent, 1),
		finals: make(chan finalResult, 1),
	}
	// liveSegments left false (zero value)

	err := session.handleStreamEvent(StreamEvent{
		Type: StreamEventFinal,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	select {
	case result := <-session.finals:
		if result.text != "hello world" {
			t.Fatalf("result.text = %q, want hello world", result.text)
		}
		if result.err != nil {
			t.Fatalf("result.err = %v, want nil", result.err)
		}
	default:
		t.Fatal("expected queued final result")
	}
}

// TestWaitForCompletionDrainsMultiplePendingSegments reproduces the
// transcript-loss bug from session 20260430-120706: at commit time, two
// `speech_started` events were outstanding but only one matching
// `completed` had landed. The old single-shot waitForFinal returned on
// the first final and abandoned the second. The drain-loop
// waitForCompletion must keep going until HasInflightWork goes false.
func TestWaitForCompletionDrainsMultiplePendingSegments(t *testing.T) {
	t.Parallel()

	stream := &Stream{stats: newStats()}
	// Two server-VAD segments started; one completed already drained,
	// one still owed (mirrors the bug's state at commit time).
	stream.stats.SpeechStartedCount = 2
	stream.stats.InboundCounts["conversation.item.input_audio_transcription.completed"] = 0

	session := &DictationSession{
		events:                make(chan DictationEvent, 4),
		finals:                make(chan finalResult, 4),
		waitFinalFloorSeconds: 2,
	}
	session.stream.Store(stream)

	// Goroutine simulates Whisper returning two completed events. Per the
	// real protocol, Stream.handleMessage updates the counter BEFORE
	// emitting StreamEventFinal — mirror that order so HasInflightWork
	// reflects the post-update state by the time the final hits s.finals.
	go func() {
		for _, text := range []string{"first segment.", "second segment."} {
			time.Sleep(20 * time.Millisecond)
			stream.statsMu.Lock()
			stream.stats.InboundCounts["conversation.item.input_audio_transcription.completed"]++
			stream.statsMu.Unlock()
			session.pushFinal(text, nil)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := session.waitForCompletion(ctx, stream, nil)
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if !strings.Contains(got, "first segment") || !strings.Contains(got, "second segment") {
		t.Fatalf("waitForCompletion = %q, want both 'first segment' and 'second segment'", got)
	}
}

// TestWaitForCompletionReturnsImmediatelyWhenNothingPending pins the
// no-speech case: no speech_started ever fired and no partial is in
// flight, so HasInflightWork is false from the start. The drain loop
// must return instantly without waiting on the floor timeout.
func TestWaitForCompletionReturnsImmediatelyWhenNothingPending(t *testing.T) {
	t.Parallel()

	stream := &Stream{stats: newStats()}
	session := &DictationSession{
		events:                make(chan DictationEvent, 1),
		finals:                make(chan finalResult, 1),
		waitFinalFloorSeconds: 60, // would fail loudly if we actually waited
	}
	session.stream.Store(stream)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	got, err := session.waitForCompletion(ctx, stream, nil)
	if err != nil {
		t.Fatalf("waitForCompletion: %v", err)
	}
	if got != "" {
		t.Fatalf("waitForCompletion = %q, want empty", got)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("waitForCompletion took %s with nothing pending; expected near-instant", elapsed)
	}
}

// TestStatsInPostCommittedSilenceTracksServerVADCycle pins the state
// machine behind the Finalize fast-path that skips an unnecessary commit
// when the server has already committed an utterance on its own.
func TestStatsInPostCommittedSilenceTracksServerVADCycle(t *testing.T) {
	t.Parallel()

	stats := newStats()
	now := time.Now()

	// speech_started: server VAD detected an utterance beginning.
	stats.observeInbound(now, jsonMessage{"type": "input_audio_buffer.speech_started"})
	if stats.InPostCommittedSilence {
		t.Fatal("speech_started must not set InPostCommittedSilence")
	}

	// speech_stopped: end of utterance detected, but buffer not yet committed.
	stats.observeInbound(now, jsonMessage{"type": "input_audio_buffer.speech_stopped"})
	if stats.InPostCommittedSilence {
		t.Fatal("speech_stopped must not set InPostCommittedSilence")
	}

	// committed: server VAD drained the buffer.
	stats.observeInbound(now, jsonMessage{"type": "input_audio_buffer.committed"})
	if !stats.InPostCommittedSilence {
		t.Fatal("input_audio_buffer.committed must set InPostCommittedSilence")
	}

	// Next utterance begins → flag clears so a real commit would fire.
	stats.observeInbound(now, jsonMessage{"type": "input_audio_buffer.speech_started"})
	if stats.InPostCommittedSilence {
		t.Fatal("next speech_started must clear InPostCommittedSilence")
	}
}

func TestAppendSegmentTextAddsSpaceBetweenChunks(t *testing.T) {
	t.Parallel()

	got := appendSegmentText("hello", "world")
	if got != "hello world" {
		t.Fatalf("appendSegmentText = %q, want hello world", got)
	}
}

// TestStreamAppendPartialIncrementalDeltas exercises the incremental
// delta semantics used by gpt-4o-transcribe et al.: each event carries
// only the new text to append.
func TestStreamAppendPartialIncrementalDeltas(t *testing.T) {
	t.Parallel()

	s := &Stream{mergeDelta: mergeIncrementalDelta}
	s.appendPartial("item_1", "Ok")
	s.appendPartial("item_1", " I")
	got := s.appendPartial("item_1", " see")
	if got != "Ok I see" {
		t.Fatalf("incremental partial = %q, want %q", got, "Ok I see")
	}
}

// TestStreamAppendPartialCumulativeDeltas exercises the cumulative
// delta semantics used by Whisper models: each event carries the full
// transcript so far. Naïve concatenation would produce
// "OkOK IOK I see" — the merge strategy must replace rather than append.
func TestStreamAppendPartialCumulativeDeltas(t *testing.T) {
	t.Parallel()

	s := &Stream{mergeDelta: mergeCumulativeDelta}
	s.appendPartial("item_1", "Ok")
	s.appendPartial("item_1", "OK I")
	got := s.appendPartial("item_1", "OK I see")
	if got != "OK I see" {
		t.Fatalf("cumulative partial = %q, want %q", got, "OK I see")
	}
}

// TestStreamAppendPartialDefaultsToIncremental documents that a zero-value
// Stream (constructed without a transport in tests) falls back to the
// OpenAI-style append. Protects helpers that build a Stream directly.
func TestStreamAppendPartialDefaultsToIncremental(t *testing.T) {
	t.Parallel()

	s := &Stream{}
	got := s.appendPartial("", "hello") + s.appendPartial("", " world")
	if got != "hellohello world" {
		// first call returns "hello" (partial=""), second returns "hello world".
		// sum is "hello" + "hello world" = "hellohello world".
		t.Fatalf("default-merge partials = %q", got)
	}
}


func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
