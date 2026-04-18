package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// readWAV loads a PCM16 WAV file into **mono** int16 samples and
// reports the rate from the RIFF header. Stereo (or higher) sources
// are averaged down to mono so downstream code never has to worry about
// channel layout. When the header can't be parsed, rate is 0 and the
// caller picks a default.
func readWAV(path string) (samples []int16, rate int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(raw) < 44 {
		return nil, 0, fmt.Errorf("wav too short: %d bytes", len(raw))
	}
	channels := 1
	if string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WAVE" && string(raw[12:16]) == "fmt " {
		rate = int(binary.LittleEndian.Uint32(raw[24:28]))
		channels = int(binary.LittleEndian.Uint16(raw[22:24]))
		if channels < 1 {
			channels = 1
		}
	}
	data := raw[44:]
	interleaved := make([]int16, len(data)/2)
	for i := range interleaved {
		interleaved[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	if channels == 1 {
		return interleaved, rate, nil
	}
	frames := len(interleaved) / channels
	mono := make([]int16, frames)
	for f := 0; f < frames; f++ {
		sum := 0
		for c := 0; c < channels; c++ {
			sum += int(interleaved[f*channels+c])
		}
		mono[f] = int16(sum / channels)
	}
	return mono, rate, nil
}

// writeWAV writes interleaved PCM16 samples with the given channel
// count. Mono audio should pass channels=1; monoToStereo widens
// mono→stereo for callers that want L==R playback.
func writeWAV(path string, samples []int16, rate, channels int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeWAVTo(f, samples, rate, channels)
}

func writeWAVTo(w io.Writer, samples []int16, rate, channels int) error {
	data := samplesToBytes(samples)
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(data)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(header[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(data)))

	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func samplesToBytes(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func monoToStereo(mono []int16) []int16 {
	out := make([]int16, len(mono)*2)
	for i, s := range mono {
		out[i*2] = s
		out[i*2+1] = s
	}
	return out
}

// resample converts between sample rates with a nearest-neighbor pick.
// Good enough for "does Whisper hear this"; for playback, keep the
// source rate to avoid aliasing.
func resample(in []int16, inRate, outRate int) []int16 {
	if inRate == outRate {
		return in
	}
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
