# Contract: Protocol Subpackage Public API

Package `protocol` provides consumer-friendly helpers for building
Yjs collaboration servers in Go.

## Message Framing

```go
// WriteMessage writes a framed message to buf.
// Format: [msgType as VarUint][payload bytes]
func WriteMessage(buf *bytes.Buffer, msgType uint8, payload []byte)

// ReadMessage reads a framed message from buf.
// Returns message type and payload.
func ReadMessage(buf *bytes.Buffer) (msgType uint8, payload []byte, err error)
```

## Sync Handler

```go
// SyncHandler manages the sync state machine for a Y.Doc.
type SyncHandler struct { ... }

// NewSyncHandler creates a handler for the given document.
func NewSyncHandler(doc *y_crdt.Doc) *SyncHandler

// HandleMessage processes an incoming message, dispatching by type.
// For sync messages, writes response to encoder if needed.
// Returns the message type that was handled.
func (h *SyncHandler) HandleMessage(msg []byte, encoder *bytes.Buffer) (uint8, error)

// RegisterHandler registers a custom message type handler.
func (h *SyncHandler) RegisterHandler(msgType uint8, handler MessageHandler)

// MessageHandler processes a message payload of a specific type.
type MessageHandler func(payload []byte) error

// EncodeSyncStep1 encodes a SyncStep1 message (state vector request).
func EncodeSyncStep1(doc *y_crdt.Doc) []byte

// EncodeSyncStep2 encodes a SyncStep2 message (state diff response).
func EncodeSyncStep2(doc *y_crdt.Doc, encodedStateVector []byte) ([]byte, error)
```

## Awareness Helpers

```go
// EncodeAwarenessMessage encodes an awareness update for broadcast.
func EncodeAwarenessMessage(clientID y_crdt.Number, state y_crdt.Object) []byte

// DecodeAwarenessMessage decodes an awareness update from bytes.
func DecodeAwarenessMessage(data []byte) (clientID y_crdt.Number, state y_crdt.Object, err error)
```

## Message Type Constants

```go
const (
    MessageSync      uint8 = 0
    MessageAwareness uint8 = 1
    // Types 2+ available for custom handlers
)
```
