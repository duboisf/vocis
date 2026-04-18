// Command lemonade-probe exercises Lemonade's realtime transcription
// and TTS endpoints from the command line. Two subcommands:
//
//	# Synthesize speech — pure HTTP, no realtime WS
//	lemonade-probe tts "hello world" [-out PATH|-] [-voice V] [-model M]
//
//	# Transcribe — opens the realtime WS and streams the chosen source
//	lemonade-probe stt wav  /path/to/audio.wav [flags]
//	lemonade-probe stt mic  5                  [flags]
//	lemonade-probe stt text "say this out loud and transcribe it" [flags]
//
// See `lemonade-probe tts -h` / `lemonade-probe stt wav -h` for flags.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "tts":
		err = runTTS(args)
	case "stt":
		err = runSTT(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: lemonade-probe <command> [args]

commands:
  tts "<text>" [flags]        synthesize speech, write WAV to -out (or stdout)
  stt wav  <path>   [flags]   transcribe a WAV file via realtime WS
  stt mic  <seconds> [flags]  record mic and transcribe via realtime WS
  stt text "<text>" [flags]   synthesize then transcribe in one shot

Each subcommand accepts its own -h for flag details.
`)
}
