// Package protocol provides consumer-friendly helpers for building Yjs
// collaboration servers in Go: message framing, a sync-protocol state machine,
// awareness helpers, and a registry for custom message types. It is the Go
// equivalent of the y-protocols npm package and is an optional import — the core
// y_crdt package has no dependency on it.
//
// The transport (WebSocket, etc.), rooms, auth and persistence are the
// application's concern; this package only frames and dispatches messages.
package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/antst/go-yjs/internal/lib0"
)

// Message type constants. Types 2+ are available for custom handlers registered
// via SyncHandler.RegisterHandler, matching y-protocols' dispatch-by-type-byte
// pattern.
const (
	MessageSync      uint8 = 0
	MessageAwareness uint8 = 1
)

// ErrShortMessage is returned when a framed message has no complete type byte —
// an empty buffer, or a type varuint truncated mid-read (io.EOF /
// io.ErrUnexpectedEOF). A complete-but-out-of-range type prefix (a varuint that
// overflows uint64) is ErrInvalidMessageType, not this.
var ErrShortMessage = errors.New("protocol: message too short to contain a type")

// ErrInvalidMessageType is returned when a frame's type field does not fit in a
// uint8 (so it cannot be a valid message type and must not be silently aliased
// into MessageSync by truncation).
var ErrInvalidMessageType = errors.New("protocol: message type out of range")

// WriteMessage writes a generic outer envelope: [msgType as VarUint] followed
// by the raw payload. It deliberately does not add message-type-specific body
// framing. Sync sub-messages already carry their own VarUint8Array bodies;
// awareness messages instead require a length prefix around the entire update
// and must be written with EncodeAwarenessUpdateMessage.
func WriteMessage(buf *bytes.Buffer, msgType uint8, payload []byte) {
	lib0.WriteVarUint(buf, uint64(msgType))
	buf.Write(payload)
}

// ReadMessage reads one generic outer envelope from buf — the leading VarUint
// type, then the remaining bytes as its still-message-specific payload (one
// message per buffer, as the transport frames it). SyncHandler performs the
// awareness-specific VarUint8Array extraction after this generic read. It
// rejects a type that does not fit in uint8 (rather than truncating it, which
// could misroute an out-of-range type into MessageSync).
func ReadMessage(buf *bytes.Buffer) (msgType uint8, payload []byte, err error) {
	if buf.Len() == 0 {
		return 0, nil, ErrShortMessage
	}
	t, err := lib0.ReadVarUint(buf)
	if err != nil {
		// Distinguish a genuinely short frame from a complete-but-invalid type
		// prefix. A truncated type varuint (io.EOF / io.ErrUnexpectedEOF) means the
		// frame really has no complete type byte -> ErrShortMessage. A varuint that
		// is fully present but overflows uint64 is an out-of-range type, not a short
		// frame -> ErrInvalidMessageType (consistent with the t > 255 case below).
		// Either way wrap the underlying cause so it stays retrievable via errors.Is.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("%w: %w", ErrShortMessage, err)
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrInvalidMessageType, err)
	}
	if t > 255 {
		return 0, nil, ErrInvalidMessageType
	}
	return uint8(t), buf.Bytes(), nil
}
