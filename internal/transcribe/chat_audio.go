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

	chunkMaxSamples int
	historyTurns    int
	promptTemplate  string
	language        string
	streamSSE       bool
	tailSilenceMS   int

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
type chatChunk struct {
	pcm      []int16
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
		tailSilenceMS:        streaming.TailSilenceMS,
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

	// "Connection ready" is synthetic for the chat-audio backend — there's
	// no upfront handshake to await. Fire OnConnected immediately so the
	// overlay drops the connecting spinner.
	if opts.Callbacks.OnConnecting != nil {
		opts.Callbacks.OnConnecting(1, 1)
	}
	if opts.Callbacks.OnConnected != nil {
		opts.Callbacks.OnConnected()
	}

	sessionlog.Infof("chat-audio: session started model=%q chunk_max=%ds history_turns=%d stream=%t",
		s.model, chunkMax, s.historyTurns, s.streamSSE)

	go s.run(pumpCtx, opts.Samples, streaming)
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
// remaining buffered audio as the trailing chunk.
func (s *chatAudioSession) run(
	ctx context.Context,
	samples <-chan []int16,
	streaming config.StreamingConfig,
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
	flush := func(reason string, trailing bool) {
		if len(buf) == 0 {
			if trailing {
				// No trailing audio — still send the sentinel so
				// the worker closes finals.
				s.chunksCh <- chatChunk{trailing: true}
			}
			return
		}
		// Copy the slice so the worker owns its own memory. The
		// pump reuses buf across chunks.
		chunk := make([]int16, len(buf))
		copy(chunk, buf)
		buf = buf[:0]
		sessionlog.Debugf("chat-audio: flush chunk reason=%s samples=%d (~%dms) trailing=%t",
			reason, len(chunk), len(chunk)*1000/s.sampleRate, trailing)
		s.chunksCh <- chatChunk{pcm: chunk, trailing: trailing}
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
			buf = append(buf, chunk...)
			if vad != nil {
				if evt := vad.Feed(chunk); evt == VADSpeechStopped {
					vad.Reset()
					flush("vad_stopped", false)
					continue
				}
			}
			if len(buf) >= s.chunkMaxSamples {
				sessionlog.Warnf("chat-audio: forced cut at %ds (utterance > chunk_max_seconds)",
					len(buf)/s.sampleRate)
				flush("chunk_max", false)
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
			if len(chunk.pcm) == 0 {
				if chunk.trailing {
					return
				}
				continue
			}
			text, err := s.transcribeChunk(ctx, chunk.pcm)
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
				// even when the result hasn't been delivered yet.
				s.appendHistory(chatTurn{wav: encodePCM16WAV(chunk.pcm, s.sampleRate), transcript: text})
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

// transcribeChunk wraps PCM as WAV, builds the few-shot message list,
// posts to /chat/completions, and returns the assembled transcript.
// Emits SSE deltas as DictationEventPartial events while the response
// streams (when streamSSE is on).
func (s *chatAudioSession) transcribeChunk(ctx context.Context, pcm []int16) (string, error) {
	chunkCtx, span := telemetry.StartSpan(ctx, "vocis.transcribe.chat_audio.chunk",
		attribute.Int("chunk.samples", len(pcm)),
		attribute.Int("chunk.duration_ms", len(pcm)*1000/s.sampleRate),
	)
	defer telemetry.EndSpan(span, nil)

	wav := encodePCM16WAV(pcm, s.sampleRate)
	messages := s.buildMessages(wav)
	body := map[string]any{
		"model":    s.model,
		"messages": messages,
		"stream":   s.streamSSE,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal chat-audio request: %w", err)
	}
	span.SetAttributes(
		attribute.Int("chunk.wav_bytes", len(wav)),
		attribute.Int("chunk.history_turns", s.historyLen()),
		attribute.Int("chunk.request_bytes", len(raw)),
	)
	sessionlog.Infof("chat-audio: posting chunk wav=%dB history=%d req=%dB",
		len(wav), s.historyLen(), len(raw))

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

// buildMessages assembles the few-shot message list for one chunk.
// Pattern (matches the user's tested payload):
//
//	user:      [text instruction, input_audio: prior chunk 1]
//	assistant: prior transcript 1
//	user:      [text instruction, input_audio: prior chunk 2]
//	assistant: prior transcript 2
//	user:      [text instruction, input_audio: current chunk]
//
// historyTurns caps the history fed back to the model. When 0, only
// the current chunk is sent.
func (s *chatAudioSession) buildMessages(currentWAV []byte) []map[string]any {
	prompt := s.renderPrompt()
	history := s.historySnapshot()
	if s.historyTurns < len(history) {
		history = history[len(history)-s.historyTurns:]
	}
	msgs := make([]map[string]any, 0, 2*len(history)+1)
	for _, turn := range history {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": userContent(prompt, turn.wav),
		})
		msgs = append(msgs, map[string]any{
			"role":    "assistant",
			"content": turn.transcript,
		})
	}
	msgs = append(msgs, map[string]any{
		"role":    "user",
		"content": userContent(prompt, currentWAV),
	})
	return msgs
}

// userContent builds the multimodal content array: a text instruction
// followed by an input_audio part with base64-encoded WAV bytes.
func userContent(prompt string, wav []byte) []map[string]any {
	return []map[string]any{
		{"type": "text", "text": prompt},
		{
			"type": "input_audio",
			"input_audio": map[string]any{
				"data":   base64.StdEncoding.EncodeToString(wav),
				"format": "wav",
			},
		},
	}
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
	if !s.liveSegments.Load() {
		// Past Finalize, partials would race with the trailing-text
		// drain; the joined transcript is what matters.
		return
	}
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
