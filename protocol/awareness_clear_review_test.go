package protocol

import (
	"bytes"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// Copilot 8e6e93a: EncodeAwarenessMessage emitted "{}" for a cleared (IsNil) state, not "null".
func TestEncodeAwarenessMessageClearedStateIsNull(t *testing.T) {
	var cleared crdt.Object // zero Object → IsNil
	payload := EncodeAwarenessMessage(crdt.Number(5), cleared)
	dec := bytes.NewBuffer(payload)
	_, _ = lib0.ReadVarUint(dec) // count
	_, _ = lib0.ReadVarUint(dec) // clientID
	_, _ = lib0.ReadVarUint(dec) // clock
	s, _ := lib0.ReadString(dec)
	if s != "null" {
		t.Fatalf("cleared state must encode as JSON null, got %q (ghost-cursor bug)", s)
	}
	// a present state still encodes as its JSON object
	p := EncodeAwarenessMessage(crdt.Number(6), crdt.MakeObject("n", float64(1)))
	d2 := bytes.NewBuffer(p)
	_, _ = lib0.ReadVarUint(d2)
	_, _ = lib0.ReadVarUint(d2)
	_, _ = lib0.ReadVarUint(d2)
	s2, _ := lib0.ReadString(d2)
	if s2 == "null" || s2 == "{}" {
		t.Fatalf("present state should be its JSON object, got %q", s2)
	}
}
