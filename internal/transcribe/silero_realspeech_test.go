package transcribe

import (
	"encoding/binary"
	"io"
	"os"
	"testing"
)

// TestSileroRealSpeech feeds a known 16 kHz mono PCM16 speech sample
// through SileroVAD and verifies the probability rises above the
// speech threshold at some point. If this test fails but the model
// loads, our feeding pipeline is wrong (not the model). If it passes
// but live dictation doesn't trigger, the live audio is somehow
// different from a clean WAV — scale, DC offset, or channel layout.
func TestSileroRealSpeech(t *testing.T) {
	libPath := os.ExpandEnv("$HOME/opt/onnxruntime/lib/libonnxruntime.so")
	if _, err := os.Stat(libPath); err != nil {
		t.Skipf("libonnxruntime.so not at %s: %v", libPath, err)
	}
	wavPath := "/tmp/silero_en.wav"
	if _, err := os.Stat(wavPath); err != nil {
		t.Skipf("speech sample not at %s: %v", wavPath, err)
	}

	samples, err := readPCM16MonoWAV(wavPath)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	t.Logf("loaded %d samples (%.1fs at 16 kHz)", len(samples), float64(len(samples))/16000)

	if err := initSilero(libPath); err != nil {
		t.Fatalf("initSilero: %v", err)
	}

	vad, err := NewSileroVAD(400, 100, 0)
	if err != nil {
		t.Fatalf("NewSileroVAD: %v", err)
	}
	defer vad.Destroy()

	// Feed in 512-sample windows to match the production path.
	var maxProb float64
	windows := 0
	for i := 0; i+sileroWindowSamples <= len(samples); i += sileroWindowSamples {
		vad.Feed(samples[i : i+sileroWindowSamples])
		snap := vad.Snapshot()
		if snap.LastRMS > maxProb {
			maxProb = snap.LastRMS
		}
		if windows < 5 || windows%200 == 0 {
			// Sum of abs hidden-state values — a good liveness
			// signal. Zero means state isn't being fed back.
			var stateSum float64
			for _, v := range vad.stateIn.GetData() {
				if v < 0 {
					stateSum -= float64(v)
				} else {
					stateSum += float64(v)
				}
			}
			t.Logf("window %d: prob=%.4f state_sum=%.4f", windows, snap.LastRMS, stateSum)
		}
		windows++
	}
	t.Logf("ran %d windows, max probability = %.4f", windows, maxProb)

	if maxProb < 0.5 {
		t.Fatalf("speech sample never crossed speech threshold: max prob = %.4f", maxProb)
	}
}

// readPCM16MonoWAV reads a 16-bit mono WAV file and returns the PCM
// samples as int16. Minimal parser: skips the 44-byte canonical header
// and reads the rest as little-endian int16. Good enough for test
// fixtures; not a general WAV reader.
func readPCM16MonoWAV(path string) ([]int16, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 44)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, err
	}
	// Read rest as int16 LE.
	var out []int16
	for {
		var s int16
		if err := binary.Read(f, binary.LittleEndian, &s); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
