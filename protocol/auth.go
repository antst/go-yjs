package protocol

import (
	"bytes"
	"fmt"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// MessagePermissionDenied is the y-protocols/auth permission-denied subtype.
const MessagePermissionDenied uint8 = 0

// WritePermissionDenied writes a canonical y-protocols/auth permission-denied
// message body.
func WritePermissionDenied(encoder *bytes.Buffer, reason string) error {
	lib0.WriteVarUint(encoder, uint64(MessagePermissionDenied))
	return lib0.WriteString(encoder, reason)
}

// ReadAuthMessage decodes one y-protocols/auth message and invokes
// permissionDeniedHandler only after the complete reason has been decoded.
func ReadAuthMessage(decoder *bytes.Buffer, doc *crdt.Doc, permissionDeniedHandler func(doc *crdt.Doc, reason string)) error {
	messageType, err := lib0.ReadVarUint(decoder)
	if err != nil {
		return fmt.Errorf("read auth message type: %w", err)
	}
	if messageType != uint64(MessagePermissionDenied) {
		return nil
	}
	reason, err := lib0.ReadString(decoder)
	if err != nil {
		return fmt.Errorf("read permission-denied reason: %w", err)
	}
	permissionDeniedHandler(doc, reason)
	return nil
}
