package protocol

import (
	"bytes"
	"fmt"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// MessageHandler processes a registered message payload. Custom and sync
// overrides receive everything after the VarUint type prefix. An awareness
// override receives the decoded awareness-update body after its canonical
// VarUint8Array length prefix. The type is VarUint-framed, so for type ids >=
// 128 the prefix is more than one byte.
type MessageHandler func(payload []byte) error

// SyncHandler manages the Yjs sync state machine (SyncStep1 -> SyncStep2 ->
// streaming Updates) for a single document, and dispatches incoming messages by
// type to the sync logic, awareness, or a registered custom handler.
type SyncHandler struct {
	doc       *crdt.Doc
	awareness *crdt.Awareness
	handlers  map[uint8]MessageHandler
}

// NewSyncHandler creates a handler for the given document.
func NewSyncHandler(doc *crdt.Doc) *SyncHandler {
	return &SyncHandler{
		doc:      doc,
		handlers: make(map[uint8]MessageHandler),
	}
}

// SetAwareness attaches an awareness instance so that MessageAwareness messages
// are applied to it. Without one, awareness messages are dispatched to a custom
// handler if registered, otherwise ignored.
func (h *SyncHandler) SetAwareness(a *crdt.Awareness) {
	h.awareness = a
}

// RegisterHandler registers a custom message-type handler. It can also override
// the awareness handling by registering MessageAwareness.
func (h *SyncHandler) RegisterHandler(msgType uint8, handler MessageHandler) {
	h.handlers[msgType] = handler
}

// Doc returns the document this handler syncs.
func (h *SyncHandler) Doc() *crdt.Doc { return h.doc }

// HandleMessage processes one incoming framed message, dispatching by type. For
// a sync message that requires a reply (SyncStep1), the framed response is
// written to encoder. It returns the message type that was handled.
//
// A malformed/truncated payload that panics deep inside the core decode path is
// recovered and surfaced as an error, so a single bad client frame cannot crash
// the connection handler.
func (h *SyncHandler) HandleMessage(msg []byte, encoder *bytes.Buffer) (retType uint8, retErr error) {
	return h.HandleMessageWithOrigin(msg, encoder, h)
}

// HandleMessageWithOrigin processes one incoming framed message like
// HandleMessage, but applies document and awareness updates with origin. A
// relay can pass the originating connection here and exclude it from fan-out
// when the synchronous update observers receive that same origin.
//
// Call InspectMessage first when authorization or post-apply size gates need to
// examine the message body before mutation. This method inspects the frame again
// so malformed input is never applied based on caller-supplied classification.
func (h *SyncHandler) HandleMessageWithOrigin(msg []byte, encoder *bytes.Buffer, origin any) (retType uint8, retErr error) {
	msgType, payload, err := readMessageView(msg)
	if err != nil {
		return 0, err
	}

	defer func() {
		if r := recover(); r != nil {
			retType = msgType
			retErr = fmt.Errorf("protocol: malformed message (type %d): %v", msgType, r)
		}
	}()

	// A whole-sync override owns that subprotocol and has always received the raw
	// bytes after the outer type. Preserve that precedence: do not impose the
	// built-in subtype/body grammar before handing its payload to the override.
	if msgType == MessageSync {
		if hdlr, ok := h.handlers[MessageSync]; ok {
			return MessageSync, hdlr(payload)
		}
	}

	inspected, err := inspectPayload(msgType, payload, len(msg))
	if err != nil {
		return msgType, err
	}

	switch msgType {
	case MessageSync:
		switch inspected.info.SyncType {
		case SyncMessageStep1:
			// SyncStep1 always produces SyncStep2. Reject a nil reply destination
			// before doing the potentially expensive state-vector diff.
			if encoder == nil {
				return MessageSync, fmt.Errorf("protocol: nil encoder for sync reply")
			}
			reply, err := EncodeSyncStep2(h.doc, inspected.info.Body)
			if err != nil {
				return MessageSync, err
			}
			_, _ = encoder.Write(reply)

		case SyncMessageStep2, SyncMessageUpdate:
			// ApplyUpdate returns after transaction cleanup and observer dispatch,
			// preserving yjs's partial-mutation-then-error contract for malformed
			// updates while giving the relay a signal that it must resynchronise.
			if err := crdt.ApplyUpdate(h.doc, inspected.info.Body, origin); err != nil {
				return MessageSync, fmt.Errorf("protocol: apply sync sub-message %d: %w", inspected.info.SyncType, err)
			}

		default:
			// Unknown in-range subtype: a no-op matching y-protocols leniency. The
			// caller can still inspect its actual id and forward the original frame.
		}
		return MessageSync, nil

	case MessageAwareness:
		if hdlr, ok := h.handlers[MessageAwareness]; ok {
			return MessageAwareness, hdlr(inspected.info.Body)
		}
		if h.awareness != nil {
			if err := crdt.ApplyAwarenessUpdate(h.awareness, inspected.info.Body, origin); err != nil {
				return MessageAwareness, err
			}
		}
		return MessageAwareness, nil

	default:
		if hdlr, ok := h.handlers[msgType]; ok {
			return msgType, hdlr(inspected.info.Body)
		}
		// Unknown type with no handler: a no-op (matches y-protocols leniency).
		return msgType, nil
	}
}

// EncodeSyncStep1 encodes a framed SyncStep1 message (a request carrying the
// document's state vector).
func EncodeSyncStep1(doc *crdt.Doc) []byte {
	return encodeSyncMessage(SyncMessageStep1, crdt.EncodeStateVector(doc))
}

// EncodeSyncStep2 encodes a framed SyncStep2 message (the structs the remote is
// missing, given its V1-encoded state vector). It returns an error if the
// document update cannot be encoded, so a failed encode is surfaced rather than
// framed as a misleadingly-complete empty SyncStep2.
func EncodeSyncStep2(doc *crdt.Doc, encodedStateVector []byte) ([]byte, error) {
	update, err := crdt.EncodeStateAsUpdate(doc, encodedStateVector)
	if err != nil {
		return nil, err
	}
	return encodeSyncMessage(SyncMessageStep2, update), nil
}

// EncodeUpdate encodes a framed sync Update message wrapping a raw document
// update (as emitted by the doc's "update" observer).
func EncodeUpdate(update []byte) []byte {
	return encodeSyncMessage(SyncMessageUpdate, update)
}

func encodeSyncMessage(messageType uint8, body []byte) []byte {
	payload := new(bytes.Buffer)
	lib0.WriteVarUint(payload, uint64(messageType))
	lib0.WriteVarUint8Array(payload, body)
	out := new(bytes.Buffer)
	WriteMessage(out, MessageSync, payload.Bytes())
	return out.Bytes()
}
