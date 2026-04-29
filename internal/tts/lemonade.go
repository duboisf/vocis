// Package tts wraps Lemonade's OpenAI-compatible /audio/speech
// endpoint, which serves Kokoro TTS in the local stack. The endpoint
// returns raw PCM16 LE samples when response_format=pcm with
// Content-Type "audio/l16;rate=N;endianness=little-endian", so we can
// stream straight to paplay without an intermediate decode.
package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"vocis/internal/sessionlog"
)

const defaultRate = 24000

// Result holds the synthesized audio plus its native sample rate.
type Result struct {
	PCM        []int16
	SampleRate int
}

// Synthesize POSTs the text to baseURL + /audio/speech and returns the
// PCM16 LE samples. Lemonade silently returns 200 + empty body when the
// voice id is unrecognized — we treat that as an error so callers don't
// "play" zero seconds of audio and assume success.
func Synthesize(ctx context.Context, baseURL, model, voice, text string) (*Result, error) {
	url := strings.TrimRight(baseURL, "/") + "/audio/speech"
	body, err := json.Marshal(map[string]any{
		"model":           model,
		"input":           text,
		"voice":           voice,
		"response_format": "pcm",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	sessionlog.Infof("tts: POST %s model=%q voice=%q chars=%d", url, model, voice, len(text))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tts body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("tts returned empty body — voice %q probably not recognized by model %q", voice, model)
	}
	if len(data)%2 != 0 {
		return nil, fmt.Errorf("tts returned odd byte count %d (expected PCM16 LE pairs)", len(data))
	}

	rate := defaultRate
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		for _, part := range strings.Split(ct, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(part), "rate="); ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					rate = n
				}
			}
		}
	}

	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	sessionlog.Infof("tts: synthesized %d samples @ %d Hz (%.2fs)",
		len(samples), rate, float64(len(samples))/float64(rate))
	return &Result{PCM: samples, SampleRate: rate}, nil
}
