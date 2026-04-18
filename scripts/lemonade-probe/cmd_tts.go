package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// runTTS handles `lemonade-probe tts "<text>" [-out PATH] [-voice V]`.
// Writes a WAV (stereo, native Kokoro rate) to -out or stdout when
// -out is "-". No streaming, no realtime WS — pure synthesis.
func runTTS(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("tts: missing text argument")
	}
	text, flagArgs := args[0], args[1:]

	fs := flag.NewFlagSet("tts", flag.ContinueOnError)
	url := fs.String("url", "http://localhost:13305/api/v1/audio/speech", "Lemonade TTS endpoint")
	model := fs.String("model", "kokoro-v1", "TTS model id")
	voice := fs.String("voice", "fable", "TTS voice id (Lemonade 10.2 silently returns empty body for unknown voices)")
	out := fs.String("out", "-", "output WAV path; - writes to stdout")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	samples, rate, err := CaptureTTS(context.Background(), *url, *model, *voice, text)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "synthesized %q via %s/%s → %d samples @ %d Hz\n",
		text, *model, *voice, len(samples), rate)

	stereo := monoToStereo(samples)
	if *out == "-" {
		if err := writeWAVTo(os.Stdout, stereo, rate, 2); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote stereo WAV @ %d Hz to stdout\n", rate)
		return nil
	}
	if err := writeWAV(*out, stereo, rate, 2); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (stereo @ %d Hz) — replay with `stt wav %s`\n", *out, rate, *out)
	return nil
}
