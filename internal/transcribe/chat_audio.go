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
	"time"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/audiocapture"
	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

// chat-audio protocol-shape constants pinned at the package level.
// These used to be YAML knobs but had defaults nobody changed in
// practice, so they live as Go consts at the consumer site to keep
// the config surface small.
const (
	// defaultChunkMaxSeconds is the upper bound on a single chunk's
	// audio duration. Gemma 3n / 4 cap audio at 30 s per request, so
	// we hold a 2 s safety margin. A long monologue without a
	// VAD-detected pause gets force-cut at this boundary and the
	// remainder rolls into the next chunk.
	defaultChunkMaxSeconds = 28
	// defaultSileroSilenceMS / defaultSileroSpeechMS /
	// defaultSileroMinUtteranceMS are the Silero hysteresis knobs
	// used by the chat-audio chunker. Pinned here because nobody
	// ever tuned them in practice — the values match the OpenAI-
	// realtime defaults the original code shipped with.
	defaultSileroSilenceMS      = 500
	defaultSileroSpeechMS       = 150
	defaultSileroMinUtteranceMS = 1000
)

// DefaultBatchPrompt drives the one-shot multi-segment batch path
// used by `vocis recall last`. Each segment arrives as a labelled
// input_audio part of the form "[clip N captured at HH:MM:SS]:" and
// the model is asked to emit exactly one line per segment, prefixed
// with that timestamp. {language} expands to TranscriptionConfig.Language
// at request build time. Used to live as `transcription.batch_prompt`.
const DefaultBatchPrompt = "Transcribe each of the following speech segments in {language}. " +
	"Each segment is preceded by a label of the form \"[clip N captured at HH:MM:SS]:\". " +
	"Output one line per input segment, in input order, formatted exactly as:\n" +
	"  HH:MM:SS\\t<transcript>\n" +
	"where HH:MM:SS is copied verbatim from the segment's label and <transcript> is the cleaned speech.\n" +
	"Cleanup: remove fillers (um, uh), fix punctuation, write digits for numbers (1.7 not one point seven). " +
	"If a segment has no intelligible speech, output its timestamp followed by a tab and nothing else. " +
	"Never output preamble, commentary, bullet points, or anything beyond the requested lines."

// chatAudioSession is the lemonade-chat backend's implementation of the
// dictation surface. The whole dictation becomes ONE /chat/completions
// POST at release: the audio pump runs Silero VAD while the hotkey is
// held and cuts the speech into clips at pauses (and at the 28 s
// per-clip cap), Finalize gates out silent clips and sends every clip
// as its own input_audio part in a single request. The model sees the
// full dictation in one go, so punctuation and casing across pauses
// come from context, and nothing is ever transcribed twice.
//
// Nothing goes on the wire while the user is speaking; SSE partials
// stream during the Finishing phase.
type chatAudioSession struct {
	httpClient *http.Client
	endpoint   string
	model      string

	chunkMaxSamples int
	promptTemplate  string
	// promptHint is appended to the rendered prompt with a blank-line
	// separator when non-empty (transcription.prompt_hint).
	promptHint   string
	language     string
	minChunkPeak float64
	minChunkRMS  float64

	// Audio assumptions: PCM16 mono at this sample rate. Lemonade's
	// gemma audio path expects 16 kHz; the recorder already produces
	// that, so no resampling. Set from DictationOpts.SampleRate at
	// startup; chunkMaxSamples is derived from this.
	sampleRate int

	hallucinationFilters map[string]bool

	// audioCapture mirrors the POSTed WAV to disk for replay.
	// Owned by the session; opened from cfg.AudioCapture at startup.
	// A nil-pointer call is a safe no-op when the feature is disabled.
	audioCapture *audiocapture.Writer

	events   chan DictationEvent
	pumpDone chan pumpResult
	cancel   context.CancelFunc
}

// pumpResult is what the audio pump hands to Finalize once the samples
// channel closes: every clip cut during the hold, in spoken order, plus
// whether Silero reported speech at any point. speech lets the energy
// gate skip its RMS arm — a clip that is mostly pause with a short
// quiet phrase averages below min_chunk_rms even though it holds words.
type pumpResult struct {
	clips  [][]int16
	speech bool
	err    error
}

// startChatAudioSession constructs and starts a chat-audio dictation
// session. The signature mirrors StartDictation so the Client can
// dispatch on backend without the caller seeing the difference.
func startChatAudioSession(
	ctx context.Context,
	cfg config.TranscriptionConfig,
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
	// Open the audio-capture writer for this dictation session. Errors
	// degrade to a no-op writer rather than failing the dictation —
	// capture is a debug-replay aid, not a correctness path.
	writer, err := audiocapture.NewWriter(cfg.AudioCapture)
	if err != nil {
		sessionlog.Warnf("chat-audio: audio capture disabled — %v", err)
		writer = nil
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	s := &chatAudioSession{
		httpClient:           httpClient,
		endpoint:             endpoint,
		model:                cfg.Model,
		chunkMaxSamples:      defaultChunkMaxSeconds * opts.SampleRate,
		promptTemplate:       cfg.Prompt,
		promptHint:           cfg.PromptHint,
		language:             cfg.Language,
		minChunkPeak:         cfg.MinChunkPeak,
		minChunkRMS:          cfg.MinChunkRMS,
		sampleRate:           opts.SampleRate,
		hallucinationFilters: buildHallucinationSet(cfg.HallucinationFilters),
		audioCapture:         writer,
		events:               make(chan DictationEvent, 16),
		pumpDone:             make(chan pumpResult, 1),
		cancel:               cancel,
	}

	sessionlog.Infof("chat-audio: session started model=%q clip_max=%ds prompt_hint_chars=%d (one POST at release)",
		s.model, defaultChunkMaxSeconds, len(strings.TrimSpace(s.promptHint)))

	// OnConnected fires from the pump on the first audio chunk rather
	// than here: by then app.go has reached the Listening state, so the
	// overlay's SetConnected does not short-circuit.
	go s.run(pumpCtx, opts.Samples, cfg.Silero, opts.OnConnected)
	return s, nil
}

func (s *chatAudioSession) Events() <-chan DictationEvent { return s.events }

// Finalize waits for the audio pump to drain, drops silent clips, and
// sends every remaining clip in one /chat/completions POST. Returns
// the transcript. Events() closes when Finalize returns.
func (s *chatAudioSession) Finalize(ctx context.Context) (FinalizeResult, error) {
	defer close(s.events)
	defer s.cancel()

	var res pumpResult
	select {
	case res = <-s.pumpDone:
	case <-ctx.Done():
		return FinalizeResult{}, ctx.Err()
	}
	if res.err != nil {
		return FinalizeResult{}, res.err
	}

	// Energy gate per clip — drop silent clips before the POST. Without
	// VAD, a hold over silence would otherwise send the entire silent
	// buffer to Gemma, which hallucinates a long "I cannot
	// transcribe..." response. When VAD saw speech only the peak arm runs.
	clips := res.clips[:0:0]
	for _, c := range res.clips {
		peak, rms, ok := s.passEnergyGate(c, res.speech)
		if !ok {
			sessionlog.Infof("chat-audio: dropped silent clip peak=%.4f rms=%.4f (min_peak=%.4f min_rms=%.4f vad_speech=%t)",
				peak, rms, s.minChunkPeak, s.minChunkRMS, res.speech)
			continue
		}
		if res.speech && s.minChunkRMS > 0 && rms < s.minChunkRMS {
			sessionlog.Infof("chat-audio: energy gate kept clip on VAD verdict — rms=%.4f is under min_rms=%.4f but Silero saw speech",
				rms, s.minChunkRMS)
		}
		clips = append(clips, c)
	}
	if len(clips) == 0 {
		sessionlog.Infof("chat-audio: no clips survived the energy gate; nothing to transcribe")
		return FinalizeResult{}, nil
	}
	if merged := mergeClips(clips, s.chunkMaxSamples); len(merged) != len(clips) {
		sessionlog.Infof("chat-audio: merged %d clip(s) into %d part(s) under the %ds cap", len(clips), len(merged), s.chunkMaxSamples/s.sampleRate)
		clips = merged
	}

	// Mirror the WAV to disk BEFORE the POST so a failed/cancelled
	// request still leaves replayable audio on disk.
	var audioPath string
	if s.audioCapture != nil {
		audioPath = s.audioCapture.WriteChunk("release", encodePCM16WAV(concatClips(clips), s.sampleRate))
	}

	text, err := s.transcribeChunk(ctx, clips)
	if err != nil {
		return FinalizeResult{}, err
	}
	text = strings.TrimSpace(text)
	if text != "" {
		sessionlog.Infof("chat-audio: response %q", text)
	}
	if text != "" && s.isHallucination(text) {
		sessionlog.Infof("chat-audio: dropped hallucinated transcript: %q", text)
		text = ""
	}
	return FinalizeResult{Text: text, AudioPath: audioPath}, nil
}

// run is the audio pump. It reads samples, feeds Silero, and cuts the
// buffer into clips on speech_stopped or at chunkMaxSamples. Nothing is
// sent while recording; the clips are handed to Finalize as one batch
// when the samples channel closes (hotkey release / recorder stop).
// Fires OnConnected on the first audio chunk so the overlay transition
// out of the default "Connecting" subtitle lands after app.go has
// reached the Listening state.
func (s *chatAudioSession) run(
	ctx context.Context,
	samples <-chan []int16,
	silero config.SileroConfig,
	onConnected func(),
) {
	var vad *SileroVAD
	if err := initSilero(silero.OnnxruntimeLibrary); err != nil {
		sessionlog.Warnf("chat-audio: silero init failed, falling back to clip_max-only cutting: %v", err)
	} else if s.sampleRate != sileroSampleRate {
		sessionlog.Warnf("chat-audio: silero requires 16kHz, got %d; falling back to clip_max-only", s.sampleRate)
	} else {
		v, err := NewSileroVAD(defaultSileroSilenceMS, defaultSileroSpeechMS, defaultSileroMinUtteranceMS)
		if err != nil {
			sessionlog.Warnf("chat-audio: silero construction failed: %v", err)
		} else {
			defer v.Destroy()
			vad = v
			sessionlog.Infof("chat-audio: silero VAD active silence=%dms speech=%dms min_utterance=%dms",
				defaultSileroSilenceMS, defaultSileroSpeechMS, defaultSileroMinUtteranceMS)
		}
	}

	var buf []int16
	var clips [][]int16
	sawSpeech := false
	cut := func(reason string, n int) {
		clip := make([]int16, n)
		copy(clip, buf[:n])
		buf = append(buf[:0], buf[n:]...)
		clips = append(clips, clip)
		sessionlog.Debugf("chat-audio: cut clip reason=%s clip_ms=%d clips=%d",
			reason, n*1000/s.sampleRate, len(clips))
	}

	for {
		select {
		case <-ctx.Done():
			sessionlog.Debugf("chat-audio: pump cancelled with %d clip(s) buffered", len(clips))
			s.pumpDone <- pumpResult{err: ctx.Err()}
			return
		case chunk, ok := <-samples:
			if !ok {
				if len(buf) > 0 {
					cut("release", len(buf))
				}
				var total int
				for _, c := range clips {
					total += len(c)
				}
				sessionlog.Infof("chat-audio: release with %d clip(s), %dms of audio, vad_speech=%t",
					len(clips), total*1000/s.sampleRate, sawSpeech)
				s.pumpDone <- pumpResult{clips: clips, speech: sawSpeech}
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if onConnected != nil {
				onConnected()
				onConnected = nil
			}
			buf = append(buf, chunk...)
			if vad != nil {
				switch vad.Feed(chunk) {
				case VADSpeechStarted:
					sawSpeech = true
				case VADSpeechStopped:
					sawSpeech = true
					vad.Reset()
					cut("vad_stopped", len(buf))
					continue
				}
			}
			// One samples-channel write may push buf well past the cap
			// if the recorder hands us a chunk larger than clip_max at
			// once. Loop so a single oversize arrival yields several
			// capped clips without losing the tail between them.
			for len(buf) >= s.chunkMaxSamples {
				sessionlog.Warnf("chat-audio: forced cut at %ds (utterance longer than the per-clip cap)",
					s.chunkMaxSamples/s.sampleRate)
				cut("force_cut", s.chunkMaxSamples)
				if vad != nil {
					vad.Reset()
				}
			}
		}
	}
}

// transcribeChunk wraps each clip as WAV, builds the message list,
// posts to /chat/completions, and returns the assembled transcript.
// Emits SSE deltas as DictationEventPartial events while the response
// streams. Multi-clip batches produce one POST with N input_audio parts.
func (s *chatAudioSession) transcribeChunk(ctx context.Context, clips [][]int16) (text string, err error) {
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
	defer func() {
		span.SetAttributes(attribute.String("chunk.response_text", text))
		telemetry.EndSpan(span, err)
	}()

	messages := s.buildMessages(wavs)
	body := map[string]any{
		"model":    s.model,
		"messages": messages,
		"stream":   true,
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
		attribute.Int("chunk.request_bytes", len(raw)),
	)
	sessionlog.Infof("chat-audio: posting chunk clips=%d wav=%dB req=%dB",
		len(clips), totalWAVBytes, len(raw))
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
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post chat-audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("chat-audio HTTP %d: %s", resp.StatusCode, httpBodyExcerpt(resp))
	}
	return s.readSSE(resp.Body)
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
// User content carries only audio. Two shapes:
//
//  1. Single clip:
//     system:   instruction
//     user:     [audio current]
//
//  2. Multi-clip current (force-cut batch):
//     system:   instruction (+ "transcribe ALL clips as one continuous
//     utterance" framing)
//     user:     [text "[clip 1]:", audio 1, text "[clip 2]:", audio 2, ...]
func (s *chatAudioSession) buildMessages(currentWAVs [][]byte) []map[string]any {
	multiClip := len(currentWAVs) > 1

	systemPrompt := s.renderPrompt()
	if hint := strings.TrimSpace(s.promptHint); hint != "" {
		systemPrompt = systemPrompt + "\n\n" + hint
	}
	if multiClip {
		systemPrompt = "You will receive several audio clips that together form ONE dictation " +
			"(the audio was split for size). Transcribe ALL clips IN ORDER as one transcript with normal " +
			"sentence punctuation and capitalization — do not echo the clip labels and do not insert separators.\n\n" +
			systemPrompt
	}

	var user any = []map[string]any{audioPart(currentWAVs[0])}
	if multiClip {
		user = multiClipContent(currentWAVs)
	}
	return []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": user},
	}
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

// mergeClips glues adjacent clips back together while the result stays
// under maxSamples. VAD cuts only exist to give the per-clip cap a
// pause-aligned split point; the model transcribes best when it hears
// the whole dictation as one input_audio part (multi-part requests came
// back without punctuation or casing), so anything that fits goes out
// as a single clip.
func mergeClips(clips [][]int16, maxSamples int) [][]int16 {
	out := make([][]int16, 0, len(clips))
	for _, c := range clips {
		if n := len(out); n > 0 && len(out[n-1])+len(c) <= maxSamples {
			out[n-1] = append(out[n-1], c...)
			continue
		}
		out = append(out, append([]int16(nil), c...))
	}
	return out
}

// concatClips joins multiple PCM clips into one slice for the audio
// capture mirror.
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

// emitPartial is display-only, so a slow consumer drops the delta
// instead of stalling the SSE read. The segment event that follows is
// sent blocking and carries the authoritative text.
func (s *chatAudioSession) emitPartial(text string) {
	select {
	case s.events <- DictationEvent{Type: DictationEventPartial, Text: text}:
	default:
	}
}

// passEnergyGate returns (peak, rms, ok) for the chunk. ok=false when
// either threshold is configured (>0) and the chunk falls below it.
// Both metrics are normalized to 0-1 by /32768. A chunk with sustained
// low-level noise (fan, room tone) fails the RMS check while peak alone
// might falsely pass it. vadSpeech=true disables the RMS arm: Silero
// already vouched for speech, and a long pause before a short phrase
// drags the average below the threshold without the clip being silent.
func (s *chatAudioSession) passEnergyGate(pcm []int16, vadSpeech bool) (float64, float64, bool) {
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
	if !vadSpeech && s.minChunkRMS > 0 && rmsNorm < s.minChunkRMS {
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

// BatchSegment is one audio clip plus the wall-clock time it was
// captured. TranscribeBatchAudios consumes a slice of these to produce
// a single timestamp-prefixed transcript over all clips in one POST.
type BatchSegment struct {
	PCM        []int16
	SampleRate int
	CapturedAt time.Time
}

// resolveBatchBudget returns the per-request audio-duration cap (in
// seconds) and a label describing where the value came from. When the
// user has pinned transcription.BatchMaxAudioSeconds, that value is used
// verbatim. When it's 0, we query Lemonade /api/v1/health for the
// loaded model's recipe_options.ctx_size and compute a safe budget:
//
//   - Audio costs 25 tokens per second on Gemma 4 (Google's audio
//     docs: "Each second of audio is 25 tokens for Gemma 4"). Gemma 3n
//     was 6.25; using that figure here packed 4x too much audio per
//     request.
//   - Reserve 60% of context for non-audio overhead: system prompt
//     (~200 tokens), per-segment labels (~10 tok each, and there can
//     be 50+ on a multi-minute window of small VAD segments), and
//     the model's reply. We saw an 80-segment request fail at
//     ~4175 tokens against a 4096 ctx; tighter reservation prevents
//     that recurring.
//   - Floor of 10s so a tiny ctx_size still does something useful.
//
// On /health failure we fall back to 30 s — matches Gemma's per-clip
// cap and is safe for any reasonable model.
func (c *Client) resolveBatchBudget(ctx context.Context) (int, string) {
	const (
		audioTokensPerSec = 25
		audioFraction     = 0.40 // 40% of ctx for audio, 60% for prompt + labels + response
		fallback          = 30
		floor             = 10
	)
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	health, err := FetchLemonadeHealth(probeCtx, c.cfg.BaseURL)
	if err != nil {
		sessionlog.Warnf("chat-audio: batch budget falling back to %ds (health probe failed: %v)", fallback, err)
		return fallback, "fallback_health_err"
	}
	var ctxSize int
	for _, m := range health.Loaded {
		if m.Name == c.cfg.Model && m.RecipeOptions.CtxSize > 0 {
			ctxSize = m.RecipeOptions.CtxSize
			break
		}
	}
	if ctxSize <= 0 {
		sessionlog.Warnf("chat-audio: batch budget falling back to %ds (no ctx_size for model %q in /health)", fallback, c.cfg.Model)
		return fallback, "fallback_no_ctx_size"
	}
	budget := int(float64(ctxSize) * audioFraction / audioTokensPerSec)
	if budget < floor {
		budget = floor
	}
	sessionlog.Infof("chat-audio: batch budget auto=%ds (ctx_size=%d, %.0f%% reserved for audio at %.2f tok/s)",
		budget, ctxSize, audioFraction*100, audioTokensPerSec)
	return budget, "auto_from_ctx_size"
}

// TranscribeBatchAudios sends a sequence of segments to
// /chat/completions, packing as many segments as fit under the
// configured BatchMaxAudioSeconds budget into each request. Each
// segment is sent as its own labelled input_audio part; the model is
// asked (via BatchPrompt) to produce one line per segment in the form
// `HH:MM:SS\t<transcript>`. Sub-batch responses are concatenated with
// newlines in input order.
//
// Why we split: Gemma's audio inputs are individually capped at ~30s
// AND the model degrades when given many multimodal inputs at once.
// A multi-minute window cannot be sent as a single request. The
// audio-duration budget is the conservative knob — pack small until
// the user finds a number their model handles reliably.
//
// Unlike StartDictation, there is no streaming pump, no VAD, no
// chunk_max splitting — the segments
// themselves are the input; the batch prompt already produces cleaned
// text.
func (c *Client) TranscribeBatchAudios(ctx context.Context, segments []BatchSegment) (string, error) {
	if len(segments) == 0 {
		return "", errors.New("transcribe batch: no segments")
	}
	endpoint, err := buildChatCompletionsURL(c.cfg.BaseURL)
	if err != nil {
		return "", err
	}
	for i, seg := range segments {
		if seg.SampleRate <= 0 {
			return "", fmt.Errorf("batch segment %d: invalid sample_rate=%d", i, seg.SampleRate)
		}
	}

	budgetSeconds, budgetSource := c.resolveBatchBudget(ctx)
	subBatches := packBatchSegments(segments, budgetSeconds)

	ctx, span := telemetry.StartSpan(ctx, "vocis.transcribe.chat_audio.batch",
		attribute.Int("batch.segment_count", len(segments)),
		attribute.Int("batch.sub_batch_count", len(subBatches)),
		attribute.Int("batch.max_audio_seconds", budgetSeconds),
		attribute.String("batch.budget_source", budgetSource),
	)
	defer telemetry.EndSpan(span, nil)

	sessionlog.Infof("chat-audio: batch start segments=%d sub_batches=%d max_audio_s=%d budget_source=%s",
		len(segments), len(subBatches), budgetSeconds, budgetSource)

	results := make([]string, 0, len(subBatches))
	for i, batch := range subBatches {
		text, err := c.transcribeBatchSub(ctx, endpoint, i+1, len(subBatches), batch)
		if err != nil {
			return "", err
		}
		results = append(results, strings.TrimRight(text, "\n"))
	}
	return strings.Join(results, "\n"), nil
}

// packBatchSegments greedily groups consecutive segments into
// sub-batches whose accumulated audio duration stays under
// budgetSeconds. A segment that alone exceeds the budget still gets
// its own sub-batch — splitting one segment further would lose its
// timestamp boundary, and the user's chunk_max_seconds already caps
// individual segments below Gemma's per-input limit. budgetSeconds<=0
// means "single sub-batch with everything" (use with caution).
func packBatchSegments(segments []BatchSegment, budgetSeconds int) [][]BatchSegment {
	if budgetSeconds <= 0 {
		return [][]BatchSegment{segments}
	}
	var out [][]BatchSegment
	var current []BatchSegment
	var currentSamples int
	for _, seg := range segments {
		segSamples := len(seg.PCM)
		budgetSamples := budgetSeconds * seg.SampleRate
		// Flush current if adding this segment would overflow AND
		// current already has something. A single oversize segment
		// goes alone in its own sub-batch on the next iteration.
		if currentSamples+segSamples > budgetSamples && len(current) > 0 {
			out = append(out, current)
			current = nil
			currentSamples = 0
		}
		current = append(current, seg)
		currentSamples += segSamples
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

// transcribeBatchSub posts one sub-batch and returns its text.
func (c *Client) transcribeBatchSub(ctx context.Context, endpoint string, index, total int, segments []BatchSegment) (string, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.transcribe.chat_audio.batch_sub",
		attribute.Int("sub.index", index),
		attribute.Int("sub.total", total),
		attribute.Int("sub.segment_count", len(segments)),
	)
	defer telemetry.EndSpan(span, nil)

	systemPrompt := strings.ReplaceAll(DefaultBatchPrompt, "{language}", c.cfg.Language)
	parts := make([]map[string]any, 0, 2*len(segments))
	var totalWAVBytes, totalSamples int
	for i, seg := range segments {
		wav := encodePCM16WAV(seg.PCM, seg.SampleRate)
		label := fmt.Sprintf("[clip %d captured at %s]:", i+1, seg.CapturedAt.Format("15:04:05"))
		parts = append(parts,
			map[string]any{"type": "text", "text": label},
			audioPart(wav),
		)
		totalWAVBytes += len(wav)
		totalSamples += len(seg.PCM)
	}
	audioSeconds := float64(totalSamples) / float64(segments[0].SampleRate)
	body := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": parts},
		},
		"stream": false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal batch sub-request %d/%d: %w", index, total, err)
	}
	span.SetAttributes(
		attribute.Int("sub.wav_bytes", totalWAVBytes),
		attribute.Int("sub.request_bytes", len(raw)),
		attribute.Float64("sub.audio_seconds", audioSeconds),
	)
	sessionlog.Infof("chat-audio: batch POST %d/%d segments=%d audio=%.2fs wav=%dB req=%dB",
		index, total, len(segments), audioSeconds, totalWAVBytes, len(raw))
	if redacted, err := redactedRequestJSON(body); err == nil {
		sessionlog.Debugf("chat-audio: batch %d/%d request body (audio redacted) → %s", index, total, redacted)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build batch sub-request %d/%d: %w", index, total, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post batch %d/%d: %w", index, total, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("batch %d/%d HTTP %d: %s", index, total, resp.StatusCode, httpBodyExcerpt(resp))
	}
	text, err := readChatCompletion(resp.Body)
	if err != nil {
		return "", fmt.Errorf("batch %d/%d: %w", index, total, err)
	}
	span.SetAttributes(attribute.Int("sub.response_length", len(text)))
	sessionlog.Infof("chat-audio: batch RESP %d/%d chars=%d", index, total, len(text))
	return text, nil
}
