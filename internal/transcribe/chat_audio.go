package transcribe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

// chatAudioSession is the lemonade-chat backend's implementation of the
// dictation surface. Unlike the realtime-WebSocket transports, this
// backend is request/response: each VAD-detected utterance becomes one
// /chat/completions POST with the audio embedded as an `input_audio`
// content part. The few-shot history of prior (audio, transcript)
// pairs gives the model context across the documented 30s per-call
// audio cap.
//
// The pump and HTTP worker are split:
//   - pump goroutine reads samples and runs Silero VAD; speech_stopped
//     transitions and chunk_max_seconds boundaries close a chunk and
//     hand it to the worker.
//   - worker goroutine drains chunks from chunksCh, posts them
//     serially (so each call sees the latest history), and pushes
//     transcripts back as DictationEvents (live) or finals (post-Finalize).
//
// Serialization matters: the assistant-turn from chunk N must be in
// the prompt for chunk N+1 to keep cross-chunk context coherent.
type chatAudioSession struct {
	httpClient *http.Client
	endpoint   string
	model      string

	chunkMaxSamples   int
	historyTurns      int
	promptTemplate    string
	language          string
	streamSSE         bool
	contextMode       string
	minChunkPeak      float64
	minChunkRMS       float64
	extraSystemPrompt string

	// Audio assumptions: PCM16 mono at this sample rate. Lemonade's
	// gemma audio path expects 16 kHz; the recorder already produces
	// that, so no resampling. Set from DictationOpts.SampleRate at
	// startup; chunkMaxSamples is derived from this.
	sampleRate int

	hallucinationFilters map[string]bool

	events   chan DictationEvent
	pumpDone chan error
	finals   chan finalResult
	chunksCh chan chatChunk
	cancel   context.CancelFunc

	liveSegments atomic.Bool
	segmentCount atomic.Int32

	historyMu sync.Mutex
	history   []chatTurn

	// workerDone closes once the worker exits, so Finalize can wait
	// for in-flight HTTP work to drain before returning.
	workerDone chan struct{}
}

// chatChunk is one VAD-bounded audio segment headed for the worker.
// trailing=true marks the last chunk produced after the samples
// channel closed — the worker treats it as the trigger to close the
// finals channel.
//
// Most chunks are single-clip (clips has one entry) — VAD-stopped
// utterances or short holds. When the user holds the hotkey through
// a long monologue and chunk_max_seconds force-cuts fire, the cut
// audio accumulates without going on the wire. The next natural
// flush (VAD-stopped pause or hotkey release) emits a multi-clip
// chunk that carries every accumulated force-cut as its own audio
// part. Lemonade's gemma-audio handles multiple input_audio parts
// in one user message per Google's docs, so the model sees the full
// utterance context in one round-trip.
type chatChunk struct {
	clips    [][]int16
	trailing bool
}

// chatTurn is a single (user-audio, assistant-transcript) pair that
// gets folded into the few-shot history list on subsequent requests.
type chatTurn struct {
	wav        []byte
	transcript string
}

// startChatAudioSession constructs and starts a chat-audio dictation
// session. The signature mirrors StartDictation so the Client can
// dispatch on backend without the caller seeing the difference.
func startChatAudioSession(
	ctx context.Context,
	cfg config.TranscriptionConfig,
	streaming config.StreamingConfig,
	httpClient *http.Client,
	opts DictationOpts,
) (*chatAudioSession, error) {
	if opts.SampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if opts.Channels != 1 {
		// gemma audio is single-channel. Mixing down would hide a
		// recorder misconfig; surface it loudly instead.
		return nil, fmt.Errorf("chat-audio backend requires mono audio, got channels=%d", opts.Channels)
	}
	endpoint, err := buildChatCompletionsURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	chunkMax := cfg.ChatAudio.ChunkMaxSeconds
	if chunkMax <= 0 {
		chunkMax = 28
	}
	contextMode := cfg.ChatAudio.ContextMode
	if contextMode == "" {
		contextMode = config.ChatAudioContextFewShot
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	s := &chatAudioSession{
		httpClient:           httpClient,
		endpoint:             endpoint,
		model:                cfg.Model,
		chunkMaxSamples:      chunkMax * opts.SampleRate,
		historyTurns:         cfg.ChatAudio.HistoryTurns,
		promptTemplate:       cfg.ChatAudio.Prompt,
		language:             cfg.ChatAudio.Language,
		streamSSE:            cfg.ChatAudio.Stream,
		contextMode:          contextMode,
		minChunkPeak:         cfg.ChatAudio.MinChunkPeak,
		minChunkRMS:          cfg.ChatAudio.MinChunkRMS,
		extraSystemPrompt:    opts.ExtraSystemPrompt,
		sampleRate:           opts.SampleRate,
		hallucinationFilters: buildHallucinationSet(cfg.HallucinationFilters),
		events:               make(chan DictationEvent, 16),
		pumpDone:             make(chan error, 1),
		finals:               make(chan finalResult, 8),
		chunksCh:             make(chan chatChunk, 4),
		cancel:               cancel,
		workerDone:           make(chan struct{}),
	}
	s.liveSegments.Store(true)

	sessionlog.Infof("chat-audio: session started model=%q chunk_max=%ds history_turns=%d stream=%t context_mode=%s",
		s.model, chunkMax, s.historyTurns, s.streamSSE, s.contextMode)

	// "Connection ready" is synthetic for the chat-audio backend — there
	// is no upfront handshake to await. The run goroutine fires
	// OnConnected on the first audio chunk it sees, which guarantees
	// app.go has finished initialization (ShowListening transitions the
	// overlay to the Listening state). A synchronous call here would
	// no-op because SetConnected short-circuits when the overlay isn't
	// yet Listening; a goroutine without a sync point would race
	// ShowListening. OnConnecting is skipped entirely — there's no real
	// connecting phase to surface, and the default Listening subtitle
	// shows "Connecting" until OnConnected swaps it.
	go s.run(pumpCtx, opts.Samples, streaming, opts.Callbacks)
	go s.worker(pumpCtx)
	return s, nil
}

func (s *chatAudioSession) Events() <-chan DictationEvent { return s.events }

// Finalize flips the session out of live-segment mode, waits for the
// audio pump to drain, then waits for the HTTP worker to finish any
// in-flight requests. Trailing transcripts arrive on s.finals and are
// joined into the FinalizeResult.
func (s *chatAudioSession) Finalize(ctx context.Context) (FinalizeResult, error) {
	s.liveSegments.Store(false)

	var pumpErr error
	select {
	case pumpErr = <-s.pumpDone:
	case <-ctx.Done():
		s.cancel()
		return FinalizeResult{}, ctx.Err()
	}
	if pumpErr != nil {
		s.cancel()
		return FinalizeResult{}, pumpErr
	}

	// Wait for worker to finish remaining chunks. The pump signals
	// trailing=true on the last chunk, after which the worker closes
	// finals and exits. A context cancel falls through to abort.
	collectCtx, collectSpan := telemetry.StartSpan(ctx, "vocis.transcribe.chat_audio.collect_trailing")
	defer telemetry.EndSpan(collectSpan, nil)

	var trailing strings.Builder
	for {
		select {
		case <-ctx.Done():
			s.cancel()
			return FinalizeResult{}, ctx.Err()
		case <-collectCtx.Done():
			return FinalizeResult{}, collectCtx.Err()
		case res, ok := <-s.finals:
			if !ok {
				return FinalizeResult{Text: trailing.String()}, nil
			}
			if res.err != nil {
				return FinalizeResult{}, res.err
			}
			trailing.WriteString(res.text)
		}
	}
}

// run is the audio pump. It reads samples, feeds Silero, and emits
// chunks on speech_stopped or chunk_max_samples force-cut. On samples
// channel close (hotkey release / recorder stop), it flushes any
// remaining buffered audio as the trailing chunk. Fires OnConnected
// on the first audio chunk so the overlay transition out of the
// default "Connecting" subtitle lands after app.go has reached the
// Listening state.
func (s *chatAudioSession) run(
	ctx context.Context,
	samples <-chan []int16,
	streaming config.StreamingConfig,
	callbacks ConnectCallbacks,
) {
	var vad *SileroVAD
	if err := initSilero(streaming.OnnxruntimeLibrary); err != nil {
		sessionlog.Warnf("chat-audio: silero init failed, falling back to chunk_max-only chunking: %v", err)
	} else if s.sampleRate != sileroSampleRate {
		sessionlog.Warnf("chat-audio: silero requires 16kHz, got %d; falling back to chunk_max-only", s.sampleRate)
	} else {
		minSilence := streaming.SilenceDurationMS
		if minSilence <= 0 {
			minSilence = 500
		}
		minSpeech := streaming.PrefixPaddingMS
		if minSpeech <= 0 {
			minSpeech = 150
		}
		minUtterance := streaming.MinUtteranceMS
		if minUtterance <= 0 {
			minUtterance = 1000
		}
		v, err := NewSileroVAD(minSilence, minSpeech, minUtterance)
		if err != nil {
			sessionlog.Warnf("chat-audio: silero construction failed: %v", err)
		} else {
			defer v.Destroy()
			vad = v
			sessionlog.Infof("chat-audio: silero VAD active silence=%dms speech=%dms min_utterance=%dms",
				minSilence, minSpeech, minUtterance)
		}
	}

	var buf []int16
	// pendingForceCuts accumulates clips force-cut at chunk_max_seconds.
	// They don't go on the wire individually — they wait for the next
	// natural flush (VAD-stopped pause or hotkey release) to be sent
	// as a single multi-clip request. Gives the model the full
	// utterance audio in one round-trip and avoids per-chunk
	// transcribe/respond cycles for long uninterrupted speech.
	var pendingForceCuts [][]int16
	connectedFired := false
	fireConnected := func() {
		if connectedFired {
			return
		}
		connectedFired = true
		if callbacks.OnConnected != nil {
			callbacks.OnConnected()
		}
	}
	flush := func(reason string, trailing bool) {
		if len(buf) == 0 && len(pendingForceCuts) == 0 {
			if trailing {
				// No audio at all — still send the sentinel so
				// the worker closes finals.
				s.chunksCh <- chatChunk{trailing: true}
			}
			return
		}
		// Drain pendingForceCuts ahead of the current buffer so the
		// model sees clips in spoken order.
		clips := pendingForceCuts
		pendingForceCuts = nil
		if len(buf) > 0 {
			tail := make([]int16, len(buf))
			copy(tail, buf)
			buf = buf[:0]
			clips = append(clips, tail)
		}
		var totalSamples int
		for _, c := range clips {
			totalSamples += len(c)
		}
		sessionlog.Debugf("chat-audio: flush chunk reason=%s clips=%d total_samples=%d (~%dms) trailing=%t",
			reason, len(clips), totalSamples, totalSamples*1000/s.sampleRate, trailing)
		s.chunksCh <- chatChunk{clips: clips, trailing: trailing}
	}
	// flushAtCap slices off exactly chunkMaxSamples from buf and
	// appends it to pendingForceCuts. The cut clip waits for the next
	// natural flush instead of going out as its own request — this is
	// the batching that lets a long monologue produce one multi-clip
	// POST instead of N separate ones.
	flushAtCap := func() {
		head := make([]int16, s.chunkMaxSamples)
		copy(head, buf[:s.chunkMaxSamples])
		tailLen := len(buf) - s.chunkMaxSamples
		copy(buf, buf[s.chunkMaxSamples:])
		buf = buf[:tailLen]
		pendingForceCuts = append(pendingForceCuts, head)
		sessionlog.Warnf("chat-audio: force-cut at %ds (utterance > chunk_max_seconds), %d clip(s) batched, %dms tail kept",
			s.chunkMaxSamples/s.sampleRate, len(pendingForceCuts), tailLen*1000/s.sampleRate)
	}

	for {
		select {
		case <-ctx.Done():
			flush("ctx_done", true)
			s.finishPump(nil)
			return
		case chunk, ok := <-samples:
			if !ok {
				flush("samples_closed", true)
				s.finishPump(nil)
				return
			}
			if len(chunk) == 0 {
				continue
			}
			fireConnected()
			buf = append(buf, chunk...)
			if vad != nil {
				if evt := vad.Feed(chunk); evt == VADSpeechStopped {
					vad.Reset()
					flush("vad_stopped", false)
					continue
				}
			}
			// One samples-channel write may push buf well past the cap
			// if the recorder hands us a chunk larger than chunk_max
			// at once. Loop the slice-and-flush so a single oversize
			// arrival emits multiple capped chunks without losing tail
			// audio between them.
			for len(buf) >= s.chunkMaxSamples {
				sessionlog.Warnf("chat-audio: forced cut at %ds (utterance > chunk_max_seconds)",
					s.chunkMaxSamples/s.sampleRate)
				flushAtCap()
				if vad != nil {
					vad.Reset()
				}
			}
		}
	}
}

// worker serially drains chunks from chunksCh, posts each to the chat-
// completions endpoint, and folds successful transcripts into history.
// Closes finals on the trailing-marker chunk so Finalize can return.
func (s *chatAudioSession) worker(ctx context.Context) {
	defer close(s.workerDone)
	defer close(s.finals)
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-s.chunksCh:
			if !ok {
				return
			}
			if len(chunk.clips) == 0 {
				if chunk.trailing {
					return
				}
				continue
			}
			// Energy gate per clip — drop any silent clips before
			// the POST. Without VAD, a hold over silence would
			// otherwise send the entire silent buffer to Gemma,
			// which hallucinates a long "I cannot transcribe..."
			// response. With VAD on, this is defense in depth.
			liveClips := chunk.clips[:0:0]
			for _, c := range chunk.clips {
				if peak, rms, ok := s.passEnergyGate(c); !ok {
					sessionlog.Infof("chat-audio: dropped silent clip peak=%.4f rms=%.4f (min_peak=%.4f min_rms=%.4f)",
						peak, rms, s.minChunkPeak, s.minChunkRMS)
					continue
				}
				liveClips = append(liveClips, c)
			}
			if len(liveClips) == 0 {
				if chunk.trailing {
					return
				}
				continue
			}
			text, err := s.transcribeChunk(ctx, liveClips)
			if err != nil {
				sessionlog.Errorf("chat-audio: chunk transcription failed: %v", err)
				if !s.liveSegments.Load() {
					select {
					case s.finals <- finalResult{err: err}:
					default:
					}
				}
				if chunk.trailing {
					return
				}
				continue
			}
			text = strings.TrimSpace(text)
			if text != "" && s.isHallucination(text) {
				sessionlog.Infof("chat-audio: dropped hallucinated final: %q", text)
				text = ""
			}
			if text != "" {
				// Push to history regardless of live/final routing —
				// the model needs continuity across the chunk boundary
				// even when the result hasn't been delivered yet. For
				// multi-clip requests we fold the clips into a single
				// concatenated WAV for the history slot since that
				// matches the assistant turn's transcript scope.
				s.appendHistory(chatTurn{
					wav:        encodePCM16WAV(concatClips(liveClips), s.sampleRate),
					transcript: text,
				})
				formatted := s.formatSegmentText(text)
				if s.liveSegments.Load() {
					select {
					case s.events <- DictationEvent{Type: DictationEventSegment, Text: formatted}:
					default:
					}
				} else {
					select {
					case s.finals <- finalResult{text: formatted}:
					default:
					}
				}
			}
			if chunk.trailing {
				return
			}
		}
	}
}

// transcribeChunk wraps each clip as WAV, builds the message list,
// posts to /chat/completions, and returns the assembled transcript.
// Emits SSE deltas as DictationEventPartial events while the response
// streams (when streamSSE is on). Multi-clip chunks (force-cut
// batches) produce one POST with N input_audio parts and skip
// few-shot history — the audio itself IS the cross-chunk context.
func (s *chatAudioSession) transcribeChunk(ctx context.Context, clips [][]int16) (string, error) {
	var totalSamples int
	wavs := make([][]byte, len(clips))
	for i, c := range clips {
		wavs[i] = encodePCM16WAV(c, s.sampleRate)
		totalSamples += len(c)
	}
	chunkCtx, span := telemetry.StartSpan(ctx, "vocis.transcribe.chat_audio.chunk",
		attribute.Int("chunk.clip_count", len(clips)),
		attribute.Int("chunk.total_samples", totalSamples),
		attribute.Int("chunk.duration_ms", totalSamples*1000/s.sampleRate),
	)
	defer telemetry.EndSpan(span, nil)

	messages := s.buildMessages(wavs)
	body := map[string]any{
		"model":    s.model,
		"messages": messages,
		"stream":   s.streamSSE,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal chat-audio request: %w", err)
	}
	var totalWAVBytes int
	for _, w := range wavs {
		totalWAVBytes += len(w)
	}
	span.SetAttributes(
		attribute.Int("chunk.wav_bytes", totalWAVBytes),
		attribute.Int("chunk.history_turns", s.historyLen()),
		attribute.Int("chunk.request_bytes", len(raw)),
	)
	sessionlog.Infof("chat-audio: posting chunk clips=%d wav=%dB history=%d req=%dB",
		len(clips), totalWAVBytes, s.historyLen(), len(raw))
	// Audit log of the exact request shape with audio bytes redacted
	// to a "<wav N bytes>" placeholder. Lets a session-log reader
	// inspect the prompt, model, message structure, and few-shot vs
	// inline-clips layout without dumping megabytes of base64 PCM
	// into the log file.
	if redacted, err := redactedRequestJSON(body); err == nil {
		sessionlog.Debugf("chat-audio: request body (audio redacted) → %s", redacted)
	}

	req, err := http.NewRequestWithContext(chunkCtx, http.MethodPost, s.endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build chat-audio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.streamSSE {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post chat-audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("chat-audio HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}

	if s.streamSSE {
		return s.readSSE(resp.Body)
	}
	return readChatCompletion(resp.Body)
}

// readSSE consumes an OpenAI-shaped SSE stream and returns the joined
// content. Per-delta text is forwarded as a DictationEventPartial so
// the overlay updates live during the request. Stops on `data: [DONE]`
// or end of stream.
func (s *chatAudioSession) readSSE(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	// SSE lines can be longer than the default 64 KiB scanner buffer
	// when the model emits a long single delta — bump the cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var full strings.Builder
	var partial strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		delta, finishReason, err := parseSSEDelta(payload)
		if err != nil {
			sessionlog.Tracef("chat-audio: unparseable SSE chunk: %v payload=%q", err, truncate(payload, 200))
			continue
		}
		if delta != "" {
			full.WriteString(delta)
			partial.WriteString(delta)
			sessionlog.Tracef("chat-audio: SSE delta %q", truncate(delta, 80))
			s.emitPartial(partial.String())
		}
		if finishReason != "" {
			sessionlog.Debugf("chat-audio: SSE finish_reason=%s", finishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read chat-audio SSE: %w", err)
	}
	return full.String(), nil
}

// readChatCompletion parses a non-streamed /chat/completions response
// and returns choices[0].message.content.
func readChatCompletion(body io.Reader) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return "", fmt.Errorf("decode chat-audio response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("chat-audio response had no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// parseSSEDelta decodes a single OpenAI-shaped SSE payload and returns
// the content delta and finish_reason (either may be empty). Tolerates
// the variations Lemonade emits — `delta.content` as a string, as a
// list of content parts, or a flat string under `text`.
func parseSSEDelta(payload string) (string, string, error) {
	var raw struct {
		Choices []struct {
			Delta        json.RawMessage `json:"delta"`
			Text         string          `json:"text"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", "", err
	}
	if len(raw.Choices) == 0 {
		return "", "", nil
	}
	c := raw.Choices[0]
	if c.Text != "" {
		return c.Text, c.FinishReason, nil
	}
	if len(c.Delta) == 0 {
		return "", c.FinishReason, nil
	}
	// Try {"content": "string"} shape first.
	var asString struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(c.Delta, &asString); err == nil && asString.Content != "" {
		return asString.Content, c.FinishReason, nil
	}
	// Fall back to {"content": [{"type":"text","text":"..."}]} shape.
	var asParts struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(c.Delta, &asParts); err == nil {
		var b strings.Builder
		for _, p := range asParts.Content {
			if p.Type == "text" || p.Type == "" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), c.FinishReason, nil
	}
	return "", c.FinishReason, nil
}

// buildMessages assembles the message list for one chunk.
//
// All instruction text lives in a single role:system message — keeping
// the prompt out of role:user content is the regurgitation fix: small
// instruct models like gemma4-it-e2b-FLM tend to echo prompt text
// back when it sits next to the audio in user content. The system
// role frames the same content as meta-instruction the model treats
// as out-of-band.
//
// User content carries only audio. Three shapes:
//
//	1. Single-clip, history-aware (few_shot mode):
//	   system:   instruction
//	   user:     [audio prior 1]
//	   assistant: prior transcript 1
//	   ...
//	   user:     [audio current]
//
//	2. Single-clip, history-aware (inline_clips mode):
//	   system:   instruction (+ "transcribe ONLY the FINAL clip" framing
//	             when history non-empty)
//	   user:     [text "[prior clip 1]:", audio prior 1, ..., text
//	             "[current clip]:", audio current]
//
//	3. Multi-clip current (force-cut batch):
//	   system:   instruction (+ "transcribe ALL clips as one continuous
//	             utterance" framing)
//	   user:     [text "[clip 1]:", audio 1, text "[clip 2]:", audio 2, ...]
//	   History is intentionally skipped — the audio is the context.
//
// historyTurns caps the history fed back to the model. When 0 (or no
// history yet, or multi-clip) the request reduces to one user message.
func (s *chatAudioSession) buildMessages(currentWAVs [][]byte) []map[string]any {
	multiClip := len(currentWAVs) > 1
	history := s.historySnapshot()
	if multiClip {
		// Skip history for multi-clip requests — the clips themselves
		// already give the model the full utterance audio. Mixing
		// per-clip and per-utterance history shapes confuses the
		// transcript boundary, and a multi-clip request body is
		// already large; not adding more.
		history = nil
	} else if s.historyTurns < len(history) {
		history = history[len(history)-s.historyTurns:]
	}

	systemPrompt := s.renderPrompt()
	switch {
	case multiClip:
		systemPrompt = "You will receive several short audio clips that together form ONE continuous utterance " +
			"(the audio was split for size). Transcribe ALL clips IN ORDER as a single continuous text — " +
			"no clip labels, no separators, just the spoken content as if it were one recording.\n\n" +
			systemPrompt
	case s.contextMode == config.ChatAudioContextInlineClips && len(history) > 0:
		systemPrompt = "You will receive several short audio clips, in order. " +
			"Transcribe ONLY the FINAL clip; the earlier clips are provided " +
			"as continuous context so you can keep proper-noun spelling, " +
			"language, and turn boundaries consistent.\n\n" + systemPrompt
	}
	if extra := strings.TrimSpace(s.extraSystemPrompt); extra != "" {
		systemPrompt = systemPrompt + "\n\n" + extra
	}

	msgs := make([]map[string]any, 0, 2+2*len(history)+1)
	msgs = append(msgs, map[string]any{
		"role":    "system",
		"content": systemPrompt,
	})

	switch {
	case multiClip:
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": multiClipContent(currentWAVs),
		})
		return msgs
	case s.contextMode == config.ChatAudioContextInlineClips:
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": inlineClipsContent(history, currentWAVs[0]),
		})
		return msgs
	}
	for _, turn := range history {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": []map[string]any{audioPart(turn.wav)},
		})
		msgs = append(msgs, map[string]any{
			"role":    "assistant",
			"content": turn.transcript,
		})
	}
	msgs = append(msgs, map[string]any{
		"role":    "user",
		"content": []map[string]any{audioPart(currentWAVs[0])},
	})
	return msgs
}

// multiClipContent builds the user message body for a force-cut batch —
// each clip gets a "[clip N]:" label so the model can see the order,
// then the input_audio part. The labels are conventional ordering
// hints; the system message asks the model to ignore them in the
// output and produce one continuous transcript.
func multiClipContent(wavs [][]byte) []map[string]any {
	parts := make([]map[string]any, 0, 2*len(wavs))
	for i, w := range wavs {
		parts = append(parts,
			map[string]any{"type": "text", "text": fmt.Sprintf("[clip %d]:", i+1)},
			audioPart(w),
		)
	}
	return parts
}

// inlineClipsContent builds the inline-clips multimodal content array
// used by the user message. The leading "transcribe ONLY the FINAL
// clip" framing now lives in the system message; this function just
// produces the audio sequence labelled by clip index.
func inlineClipsContent(history []chatTurn, currentWAV []byte) []map[string]any {
	parts := make([]map[string]any, 0, 2*(len(history)+1))
	for i, turn := range history {
		parts = append(parts,
			map[string]any{"type": "text", "text": fmt.Sprintf("[prior clip %d]:", i+1)},
			audioPart(turn.wav),
		)
	}
	parts = append(parts,
		map[string]any{"type": "text", "text": "[current clip]:"},
		audioPart(currentWAV),
	)
	return parts
}

// concatClips joins multiple PCM clips into one slice. Used when
// folding a multi-clip transcribed result into history — the
// assistant's transcript covers the whole concatenated audio, so a
// single chatTurn with all the audio is the right shape.
func concatClips(clips [][]int16) []int16 {
	total := 0
	for _, c := range clips {
		total += len(c)
	}
	out := make([]int16, 0, total)
	for _, c := range clips {
		out = append(out, c...)
	}
	return out
}

// audioPart wraps PCM-WAV bytes as a single input_audio content part
// in Lemonade's OpenAI-compat shape.
func audioPart(wav []byte) map[string]any {
	return map[string]any{
		"type": "input_audio",
		"input_audio": map[string]any{
			"data":   base64.StdEncoding.EncodeToString(wav),
			"format": "wav",
		},
	}
}

// redactedRequestJSON returns a pretty-printed JSON view of the
// request body with every input_audio.data payload replaced by a
// "<wav N bytes>" placeholder so the session log can show the exact
// prompt and message structure without spilling the raw audio.
// The original body is not mutated; a deep-redacted copy is built
// in place. HTML escaping is disabled so '<' / '>' in the
// placeholder render as themselves rather than < / > —
// this is a log line, not a browser response.
func redactedRequestJSON(body map[string]any) (string, error) {
	// Round-trip through JSON to get a generic map[string]any tree
	// we can walk safely without aliasing the live request body.
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	var clone any
	if err := json.Unmarshal(raw, &clone); err != nil {
		return "", err
	}
	redactAudioInPlace(clone)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(clone); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func (s *chatAudioSession) renderPrompt() string {
	if s.language == "" {
		return s.promptTemplate
	}
	return strings.ReplaceAll(s.promptTemplate, "{language}", s.language)
}

func (s *chatAudioSession) appendHistory(turn chatTurn) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append(s.history, turn)
	// Cap at 2*historyTurns to bound memory if Finalize never runs;
	// only the most recent historyTurns are sent on the wire anyway.
	if cap := s.historyTurns * 2; cap > 0 && len(s.history) > cap {
		s.history = s.history[len(s.history)-cap:]
	}
}

func (s *chatAudioSession) historySnapshot() []chatTurn {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	out := make([]chatTurn, len(s.history))
	copy(out, s.history)
	return out
}

func (s *chatAudioSession) historyLen() int {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return len(s.history)
}

func (s *chatAudioSession) emitPartial(text string) {
	// chat-audio's SSE deltas typically arrive AFTER Finalize() flips
	// liveSegments to false (the trailing chunk gets POSTed at hotkey
	// release, and the response streams in during the Finishing phase).
	// Always emit the partial — app.go routes it to whichever overlay
	// state is active (Listening or Finishing), so the user sees the
	// model's output as it generates regardless of phase.
	select {
	case s.events <- DictationEvent{Type: DictationEventPartial, Text: text}:
	default:
	}
}

func (s *chatAudioSession) finishPump(err error) {
	select {
	case s.pumpDone <- err:
	default:
	}
}

// passEnergyGate returns (peak, rms, ok) for the chunk. ok=false when
// either threshold is configured (>0) and the chunk falls below it.
// Both metrics are normalized to 0-1 by /32768. A chunk that's mostly
// silence with a brief loud spike still passes if peak alone is high
// enough; a chunk with sustained low-level noise (fan, room tone)
// fails the RMS check while peak alone might falsely pass it.
func (s *chatAudioSession) passEnergyGate(pcm []int16) (float64, float64, bool) {
	if s.minChunkPeak <= 0 && s.minChunkRMS <= 0 {
		return 0, 0, true
	}
	var peak int16
	var sumSq int64
	for _, v := range pcm {
		a := v
		if a < 0 {
			a = -a
			if a < 0 { // -math.MinInt16 overflows back to itself
				a = 32767
			}
		}
		if a > peak {
			peak = a
		}
		sumSq += int64(v) * int64(v)
	}
	peakNorm := float64(peak) / 32768.0
	var rmsNorm float64
	if len(pcm) > 0 {
		rmsNorm = math.Sqrt(float64(sumSq)/float64(len(pcm))) / 32768.0
	}
	if s.minChunkPeak > 0 && peakNorm < s.minChunkPeak {
		return peakNorm, rmsNorm, false
	}
	if s.minChunkRMS > 0 && rmsNorm < s.minChunkRMS {
		return peakNorm, rmsNorm, false
	}
	return peakNorm, rmsNorm, true
}

func (s *chatAudioSession) isHallucination(text string) bool {
	if len(s.hallucinationFilters) == 0 {
		return false
	}
	return s.hallucinationFilters[strings.ToLower(text)]
}

// formatSegmentText mirrors DictationSession's formatter so output
// concatenation is identical between backends.
func (s *chatAudioSession) formatSegmentText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if s.segmentCount.Add(1) == 1 {
		return text
	}
	if strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\n") {
		return text
	}
	if startsWithPunctuation(text) {
		return text
	}
	return " " + text
}

// encodePCM16WAV mirrors internal/tts.WriteWAV — duplicated here to
// avoid the recorder→transcribe→tts dependency direction. PCM16 mono
// only; Lemonade's gemma audio path doesn't accept multichannel anyway.
func encodePCM16WAV(samples []int16, rate int) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(sample))
	}
	return buf
}

// redactAudioInPlace walks a JSON-decoded tree and replaces every
// input_audio.data string with a placeholder describing the original
// base64 length in approximate decoded WAV bytes. Recurses into all
// maps and slices.
func redactAudioInPlace(node any) {
	switch v := node.(type) {
	case map[string]any:
		if audio, ok := v["input_audio"].(map[string]any); ok {
			if data, ok := audio["data"].(string); ok {
				// Base64 expands by 4/3; estimate decoded bytes for
				// the placeholder so the reader can sanity-check the
				// chunk size against `posting chunk wav=NB`.
				approx := len(data) * 3 / 4
				audio["data"] = fmt.Sprintf("<wav ~%d bytes, base64=%d chars>", approx, len(data))
			}
		}
		for _, child := range v {
			redactAudioInPlace(child)
		}
	case []any:
		for _, child := range v {
			redactAudioInPlace(child)
		}
	}
}

// buildChatCompletionsURL canonicalizes the configured base into a
// /chat/completions URL. Accepts inputs with or without the trailing
// /chat/completions and with or without a trailing slash so the user
// can paste either shape into config.
func buildChatCompletionsURL(base string) (string, error) {
	trimmed := strings.TrimRight(base, "/")
	if trimmed == "" {
		return "", errors.New("transcription.base_url is empty (required for lemonade-chat backend)")
	}
	if strings.HasSuffix(trimmed, "/chat/completions") {
		return trimmed, nil
	}
	return trimmed + "/chat/completions", nil
}
