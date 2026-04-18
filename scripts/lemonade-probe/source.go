package main

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
	"sync"

	"github.com/jfreymuth/pulse"
)

// CaptureWAV reads PCM16 samples from a WAV file. When the header
// doesn't parse (or reports 0), fallbackHz is used so callers can
// supply espeak-ng's 22050 default.
func CaptureWAV(path string, fallbackHz int) ([]int16, int, error) {
	samples, rate, err := readWAV(path)
	if err != nil {
		return nil, 0, err
	}
	if rate == 0 {
		rate = fallbackHz
	}
	return samples, rate, nil
}

// CaptureMic records `seconds` of 16 kHz mono PCM16 from the default
// PulseAudio source and returns once the target sample count is reached.
func CaptureMic(_ context.Context, seconds int) ([]int16, int, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("lemonade-probe"))
	if err != nil {
		return nil, 0, fmt.Errorf("pulse client: %w", err)
	}
	defer client.Close()

	target := 16000 * seconds
	buf := make([]int16, 0, target)
	var mu sync.Mutex
	done := make(chan struct{})
	closeDone := func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}

	writer := pulse.Int16Writer(func(samples []int16) (int, error) {
		mu.Lock()
		remain := target - len(buf)
		if remain <= 0 {
			mu.Unlock()
			closeDone()
			return len(samples), nil
		}
		if len(samples) > remain {
			samples = samples[:remain]
		}
		buf = append(buf, samples...)
		full := len(buf) >= target
		mu.Unlock()
		if full {
			closeDone()
		}
		return len(samples), nil
	})

	stream, err := client.NewRecord(writer,
		pulse.RecordSampleRate(16000),
		pulse.RecordMono,
		pulse.RecordMediaName("lemonade-probe mic capture"),
		pulse.RecordLatency(0.05),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("open record stream: %w", err)
	}
	stream.Start()
	<-done
	stream.Stop()
	stream.Close()

	mu.Lock()
	out := append([]int16(nil), buf...)
	mu.Unlock()
	return out, 16000, nil
}

// CaptureTTS shells out to Lemonade's OpenAI-compatible
// POST /v1/audio/speech and parses the raw PCM16 response. Lemonade's
// `response_format=pcm` returns audio/l16;rate=N;endianness=little-endian
// so we can read samples straight from the body.
func CaptureTTS(ctx context.Context, url, model, voice, text string) ([]int16, int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           model,
		"input":           text,
		"voice":           voice,
		"response_format": "pcm",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("tts request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("tts body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("tts HTTP %d: %s", resp.StatusCode, string(data))
	}
	if len(data) == 0 {
		// Lemonade 10.2 returns 200 + zero bytes when the voice id is
		// unknown — fail loud rather than produce an empty WAV.
		return nil, 0, fmt.Errorf("tts returned empty body — voice %q probably not recognized", voice)
	}

	rate := 24000
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		for _, part := range strings.Split(ct, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(part), "rate="); ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					rate = n
				}
			}
		}
	}

	if len(data)%2 != 0 {
		return nil, 0, fmt.Errorf("tts returned odd byte count %d", len(data))
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return samples, rate, nil
}
