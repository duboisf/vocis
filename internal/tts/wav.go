package tts

import (
	"encoding/binary"
	"io"
)

// WriteWAV writes a single-channel PCM16 LE WAV stream. Used by
// `vocis speak --out PATH`; for playback we stream the raw PCM into
// paplay --raw and skip the header entirely.
func WriteWAV(w io.Writer, samples []int16, rate int) error {
	channels := 1
	dataLen := len(samples) * 2
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataLen))
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
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataLen))

	if _, err := w.Write(header); err != nil {
		return err
	}
	buf := make([]byte, dataLen)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	_, err := w.Write(buf)
	return err
}
