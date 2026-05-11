package transcribe

// This file used to define a Transport interface that abstracted the
// realtime WebSocket backend. With Lemonade as the only realtime backend
// (the chat-audio path bypasses WebSockets entirely), the abstraction
// was a single-implementation indirection. The previous interface
// hand-waved at a future "openaiTransport" that never materialized.
//
// Realtime backend specifics now live directly on *lemonadeTransport
// in transport_lemonade.go.
