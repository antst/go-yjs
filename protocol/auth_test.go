package protocol

import (
	"bytes"
	"testing"

	"github.com/antst/go-yjs/crdt"
)

func TestPermissionDeniedMatchesYProtocolsFraming(t *testing.T) {
	doc := crdt.NewDoc("auth-framing", crdt.WithGC(false))
	var frame bytes.Buffer
	if err := WritePermissionDenied(&frame, "read only"); err != nil {
		t.Fatal(err)
	}
	if got, want := frame.Bytes(), []byte{MessagePermissionDenied, 9, 'r', 'e', 'a', 'd', ' ', 'o', 'n', 'l', 'y'}; !bytes.Equal(got, want) {
		t.Fatalf("permission-denied frame = %x, want canonical y-protocols bytes %x", got, want)
	}

	called := false
	if err := ReadAuthMessage(bytes.NewBuffer(frame.Bytes()), doc, func(gotDoc *crdt.Doc, reason string) {
		called = true
		if gotDoc != doc || reason != "read only" {
			t.Fatalf("callback = (%p, %q), want (%p, %q)", gotDoc, reason, doc, "read only")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("permission-denied callback was not invoked")
	}
}

func TestReadAuthMessageSurfacesTruncationBeforeCallback(t *testing.T) {
	doc := crdt.NewDoc("auth-errors", crdt.WithGC(false))
	for _, test := range []struct {
		name  string
		frame []byte
	}{
		{name: "message type", frame: []byte{0x80}},
		{name: "reason", frame: []byte{MessagePermissionDenied, 2, 'x'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := ReadAuthMessage(bytes.NewBuffer(test.frame), doc, func(*crdt.Doc, string) {
				called = true
			})
			if err == nil {
				t.Fatal("truncated auth frame returned nil error")
			}
			if called {
				t.Fatal("permission callback ran before the auth frame decoded completely")
			}
		})
	}
}
