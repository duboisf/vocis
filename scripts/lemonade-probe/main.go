// Command lemonade-probe streams a short WAV clip to Lemonade's realtime
// WebSocket and logs every inbound event with elapsed ms. Used to isolate
// where the commit → transcription.completed latency comes from: VAD
// silence timers, a post-inference refinement pass, or something else.
//
// Usage:
//
//	go run ./scripts/lemonade-probe \
//	    -wav /tmp/probe.wav \
//	    -silence_ms 500 \
//	    -pause_before_commit_ms 200
//
// Generate /tmp/probe.wav with:
//
//	espeak-ng -w /tmp/probe.wav -s 150 "refactor the authentication middleware"
//
// The WAV is resampled from espeak-ng's 22050 Hz to Lemonade's expected
// 16000 Hz inline so no external resampler is required.
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := flag.String("url", "ws://localhost:9000/realtime?model=whisper-v3-turbo-FLM", "realtime ws url")
	wavPath := flag.String("wav", "/tmp/probe.wav", "wav file (PCM16 mono, any rate; resampled to 16k)")
	inRate := flag.Int("in_rate", 22050, "input wav sample rate")
	chunkMs := flag.Int("chunk_ms", 50, "audio chunk size in ms")
	silenceMs := flag.Int("silence_ms", 500, "VAD silence_duration_ms; -1 disables turn_detection entirely")
	pauseMs := flag.Int("pause_before_commit_ms", 200, "client-side sleep between last audio chunk and commit")
	padSilenceMs := flag.Int("pad_silence_ms", 0, "send this many ms of zero PCM after the audio (before the pause) so Lemonade VAD sees stream silence")
	waitForSpeechStopped := flag.Bool("wait_for_speech_stopped", false, "if true, wait for speech_stopped before committing instead of using pause_before_commit_ms")
	postSpeechStoppedMs := flag.Int("post_speech_stopped_ms", 0, "extra ms to wait AFTER speech_stopped before committing (only with -wait_for_speech_stopped)")
	skipCommit := flag.Bool("skip_commit", false, "do not send commit; rely entirely on Lemonade's VAD")
	flag.Parse()

	samples, err := loadWAV(*wavPath)
	if err != nil {
		log.Fatalf("load wav: %v", err)
	}
	resampled := resample(samples, *inRate, 16000)
	pcmBytes := samplesToBytes(resampled)

	audioDurMs := len(resampled) * 1000 / 16000
	fmt.Printf("loaded %s: %d samples @ %d Hz → %d samples @ 16kHz (%dms audio)\n",
		*wavPath, len(samples), *inRate, len(resampled), audioDurMs)

	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), *url, nil)
	if err != nil {
		log.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	log := func(tag, extra string) {
		if extra == "" {
			fmt.Printf("%7dms  %s\n", time.Since(start).Milliseconds(), tag)
		} else {
			fmt.Printf("%7dms  %s  %s\n", time.Since(start).Milliseconds(), tag, extra)
		}
	}
	logAt := func(at time.Time, tag, extra string) {
		if extra == "" {
			fmt.Printf("%7dms  %s\n", at.Sub(start).Milliseconds(), tag)
		} else {
			fmt.Printf("%7dms  %s  %s\n", at.Sub(start).Milliseconds(), tag, extra)
		}
	}

	// eventArrival is captured in the WS-reader goroutine so arrival times
	// reflect when bytes landed, not when the main loop got around to
	// draining the channel. Earlier versions tagged events at drain time,
	// which buried ~200ms worth of post-commit latency inside the "pause"
	// phase and made fast-path runs look slower than they were.
	type arrivedEvent struct {
		at  time.Time
		msg map[string]any
	}
	events := make(chan arrivedEvent, 256)
	go func() {
		defer close(events)
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			events <- arrivedEvent{at: time.Now(), msg: msg}
		}
	}()

	// Build session.update
	session := map[string]any{
		"model": "whisper-v3-turbo-FLM",
	}
	if *silenceMs >= 0 {
		session["turn_detection"] = map[string]any{
			"threshold":           0.01,
			"silence_duration_ms": *silenceMs,
			"prefix_padding_ms":   300,
		}
	}
	if err := conn.WriteJSON(map[string]any{
		"type":    "session.update",
		"session": session,
	}); err != nil {
		log("error", err.Error())
		os.Exit(1)
	}
	log("session.update.sent", fmt.Sprintf("silence_ms=%d", *silenceMs))

	// Drain session.created / session.updated before streaming audio so
	// the first audio frame doesn't race the session response.
	for seen := 0; seen < 2; {
		select {
		case ev, ok := <-events:
			if !ok {
				log("ws.closed", "before session ready")
				return
			}
			t, _ := ev.msg["type"].(string)
			logAt(ev.at, "inbound", t)
			if t == "session.created" || t == "session.updated" {
				seen++
			}
		case <-time.After(5 * time.Second):
			log("error", "timeout waiting for session ready")
			return
		}
	}

	// Stream audio in chunks, sleeping chunkMs between sends so we're not
	// flooding the buffer (which would let VAD trip on commit-only signals).
	chunkBytes := (16000 * *chunkMs / 1000) * 2
	for offset := 0; offset < len(pcmBytes); offset += chunkBytes {
		end := offset + chunkBytes
		if end > len(pcmBytes) {
			end = len(pcmBytes)
		}
		b64 := base64.StdEncoding.EncodeToString(pcmBytes[offset:end])
		if err := conn.WriteJSON(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": b64,
		}); err != nil {
			log("error", err.Error())
			return
		}
		// Drain any inbound events (speech_started, deltas) as they arrive.
		for {
			select {
			case ev := <-events:
				t, _ := ev.msg["type"].(string)
				logAt(ev.at, "inbound", t)
			default:
				goto sleep
			}
		}
	sleep:
		time.Sleep(time.Duration(*chunkMs) * time.Millisecond)
	}
	log("audio.end", "")

	// Optionally stream zero PCM so Lemonade's VAD has actual silence
	// frames to evaluate against silence_duration_ms. A client-side sleep
	// alone produces no audio frames at all, which is different from
	// "audio with quiet content".
	if *padSilenceMs > 0 {
		zeros := make([]byte, (16000**padSilenceMs/1000)*2)
		// chunk it the same way real audio gets chunked, so VAD sees it
		// at the same cadence as a real recording.
		for off := 0; off < len(zeros); off += chunkBytes {
			end := off + chunkBytes
			if end > len(zeros) {
				end = len(zeros)
			}
			b64 := base64.StdEncoding.EncodeToString(zeros[off:end])
			if err := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.append", "audio": b64}); err != nil {
				log("error", err.Error())
				return
			}
			for {
				select {
				case ev := <-events:
					t, _ := ev.msg["type"].(string)
					logAt(ev.at, "inbound", t)
				default:
					goto sleepZero
				}
			}
		sleepZero:
			time.Sleep(time.Duration(*chunkMs) * time.Millisecond)
		}
		log("pad.end", fmt.Sprintf("%dms zeros", *padSilenceMs))
	}

	if *waitForSpeechStopped {
		// Drain events until we see speech_stopped, then optionally wait
		// extra ms before committing. Lets us measure whether
		// commit-after-VAD timing affects post-commit latency.
		log("waiting", "for speech_stopped")
		deadline := time.After(8 * time.Second)
		got := false
		for !got {
			select {
			case ev, ok := <-events:
				if !ok {
					log("ws.closed", "before speech_stopped")
					return
				}
				t, _ := ev.msg["type"].(string)
				logAt(ev.at, "inbound", t)
				if t == "input_audio_buffer.speech_stopped" {
					got = true
				}
			case <-deadline:
				log("error", "no speech_stopped within 8s")
				return
			}
		}
		if *postSpeechStoppedMs > 0 {
			time.Sleep(time.Duration(*postSpeechStoppedMs) * time.Millisecond)
		}
	} else {
		time.Sleep(time.Duration(*pauseMs) * time.Millisecond)
	}

	if *skipCommit {
		log("commit.skipped", "")
	} else {
		if err := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
			log("error", err.Error())
			return
		}
		log("commit.sent", "")
	}

	var commitAt time.Time
	if !*skipCommit {
		commitAt = time.Now()
	}
	var firstPostCommitDeltaAt time.Time
	var firstPostCommitDeltaText string
	var completedAt time.Time
	var completedText string

	timeout := time.After(20 * time.Second)
	for {
		select {
		case <-timeout:
			log("timeout", "no completed event")
			return
		case ev, ok := <-events:
			if !ok {
				log("ws.closed", "")
				return
			}
			t, _ := ev.msg["type"].(string)
			extra := ""
			switch t {
			case "conversation.item.input_audio_transcription.delta":
				if d, ok := ev.msg["delta"].(string); ok {
					extra = fmt.Sprintf("delta=%q", d)
					if !commitAt.IsZero() && ev.at.After(commitAt) && firstPostCommitDeltaAt.IsZero() && d != "" {
						firstPostCommitDeltaAt = ev.at
						firstPostCommitDeltaText = d
					}
				}
			case "conversation.item.input_audio_transcription.completed":
				if tr, ok := ev.msg["transcript"].(string); ok {
					extra = fmt.Sprintf("transcript=%q", tr)
					completedAt = ev.at
					completedText = tr
				}
			}
			logAt(ev.at, "inbound", t+"  "+extra)
			if t == "conversation.item.input_audio_transcription.completed" ||
				t == "conversation.item.input_audio_transcription.failed" ||
				t == "error" {
				if !commitAt.IsZero() {
					if !firstPostCommitDeltaAt.IsZero() {
						matches := firstPostCommitDeltaText == completedText
						fmt.Printf("\nSUMMARY  post_commit_delta=%dms  completed=%dms  saving=%dms  text_matches=%v\n",
							firstPostCommitDeltaAt.Sub(commitAt).Milliseconds(),
							completedAt.Sub(commitAt).Milliseconds(),
							completedAt.Sub(firstPostCommitDeltaAt).Milliseconds(),
							matches)
					} else {
						fmt.Printf("\nSUMMARY  no_post_commit_delta  completed=%dms (fast path — completed IS the first answer)\n",
							completedAt.Sub(commitAt).Milliseconds())
					}
				}
				return
			}
		}
	}
}

// loadWAV reads a PCM16 mono WAV file and returns the raw samples. Skips
// the 44-byte RIFF header. Does not validate format tags — callers are
// expected to pass the correct sample rate.
func loadWAV(path string) ([]int16, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 44 {
		return nil, fmt.Errorf("wav too short: %d bytes", len(raw))
	}
	data := raw[44:]
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return samples, nil
}

// resample is a nearest-neighbor sample-rate converter. Matches the shape
// of pcmEncoder in internal/openai for consistency with the main code.
func resample(in []int16, inRate, outRate int) []int16 {
	out := make([]int16, 0, len(in)*outRate/inRate+2)
	accum := 0
	for _, s := range in {
		accum += outRate
		for accum >= inRate {
			out = append(out, s)
			accum -= inRate
		}
	}
	return out
}

func samplesToBytes(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}
