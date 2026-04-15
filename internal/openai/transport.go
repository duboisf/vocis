package openai

import (
	"context"

	"github.com/gorilla/websocket"
)

// Transport abstracts the backend-specific parts of a realtime transcription
// connection. The OpenAI realtime API and Lemonade's local realtime endpoint
// share the same on-the-wire message protocol once a WebSocket is established,
// but differ in how the connection is opened (auth, URL shape, port discovery)
// and in the shape of the initial session.update payload.
//
// Implementations live alongside this file: openaiTransport, lemonadeTransport.
type Transport interface {
	// Dial opens an authenticated WebSocket connection to the backend.
	Dial(ctx context.Context) (*websocket.Conn, error)

	// SessionUpdate returns the initial session.update payload to send on
	// the open connection. Both backends accept the same top-level message
	// type but disagree on the nested `session` shape.
	SessionUpdate() map[string]any

	// SampleRate is the PCM sample rate (Hz) the backend expects to receive
	// audio at. Audio is resampled to this rate by pcmEncoder before
	// being base64-framed into input_audio_buffer.append events.
	SampleRate() int
}
