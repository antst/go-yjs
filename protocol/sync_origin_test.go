package protocol

import (
	"bytes"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

type relayOrigin struct{ connectionID int }

func TestHandleMessageWithOriginPropagatesDocumentAndAwarenessOrigin(t *testing.T) {
	origin := &relayOrigin{connectionID: 7}

	t.Run("document update", func(t *testing.T) {
		source := crdt.NewDoc("origin-doc", crdt.WithGC(false), crdt.WithClientID(41))
		source.GetText("t").Insert(0, "remote", crdt.Object{})
		update, err := crdt.EncodeStateAsUpdate(source, nil)
		if err != nil {
			t.Fatal(err)
		}

		receiver := crdt.NewDoc("origin-doc", crdt.WithGC(false))
		var observed any
		receiver.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
			observed = args[1]
		}))
		handler := NewSyncHandler(receiver)
		if typ, err := handler.HandleMessageWithOrigin(EncodeUpdate(update), &bytes.Buffer{}, origin); err != nil || typ != MessageSync {
			t.Fatalf("HandleMessageWithOrigin = type %d err %v", typ, err)
		}
		if observed != origin {
			t.Fatalf("document observer origin = %#v, want caller origin %#v", observed, origin)
		}
	})

	t.Run("awareness update", func(t *testing.T) {
		sourceDoc := crdt.NewDoc("origin-awareness", crdt.WithClientID(43))
		source := crdt.NewAwareness(sourceDoc)
		t.Cleanup(source.Destroy)
		if err := source.SetLocalState(crdt.MakeObject("name", "remote")); err != nil {
			t.Fatal(err)
		}
		body := crdt.EncodeAwarenessUpdate(source, []crdt.Number{source.ClientID}, nil)

		receiverDoc := crdt.NewDoc("origin-awareness")
		receiver := crdt.NewAwareness(receiverDoc)
		t.Cleanup(receiver.Destroy)
		var observed any
		receiver.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
			observed = args[1]
		}))
		handler := NewSyncHandler(receiverDoc)
		handler.SetAwareness(receiver)
		if typ, err := handler.HandleMessageWithOrigin(EncodeAwarenessUpdateMessage(body), &bytes.Buffer{}, origin); err != nil || typ != MessageAwareness {
			t.Fatalf("HandleMessageWithOrigin = type %d err %v", typ, err)
		}
		if observed != origin {
			t.Fatalf("awareness observer origin = %#v, want caller origin %#v", observed, origin)
		}
	})
}

func TestHandleMessagePreservesHandlerAsDefaultOrigin(t *testing.T) {
	t.Run("document update", func(t *testing.T) {
		source := crdt.NewDoc("default-origin", crdt.WithGC(false), crdt.WithClientID(47))
		source.GetText("t").Insert(0, "remote", crdt.Object{})
		update, err := crdt.EncodeStateAsUpdate(source, nil)
		if err != nil {
			t.Fatal(err)
		}

		receiver := crdt.NewDoc("default-origin", crdt.WithGC(false))
		var observed any
		receiver.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
			observed = args[1]
		}))
		handler := NewSyncHandler(receiver)
		if _, err := handler.HandleMessage(EncodeUpdate(update), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if observed != handler {
			t.Fatalf("legacy HandleMessage origin = %#v, want handler %#v", observed, handler)
		}
	})

	t.Run("awareness update", func(t *testing.T) {
		sourceDoc := crdt.NewDoc("default-origin-awareness", crdt.WithClientID(49))
		source := crdt.NewAwareness(sourceDoc)
		t.Cleanup(source.Destroy)
		if err := source.SetLocalState(crdt.MakeObject("name", "remote")); err != nil {
			t.Fatal(err)
		}
		body := crdt.EncodeAwarenessUpdate(source, []crdt.Number{source.ClientID}, nil)

		receiverDoc := crdt.NewDoc("default-origin-awareness")
		receiver := crdt.NewAwareness(receiverDoc)
		t.Cleanup(receiver.Destroy)
		var observed any
		receiver.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
			observed = args[1]
		}))
		handler := NewSyncHandler(receiverDoc)
		handler.SetAwareness(receiver)
		if _, err := handler.HandleMessage(EncodeAwarenessUpdateMessage(body), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if observed != handler {
			t.Fatalf("legacy HandleMessage awareness origin = %#v, want handler %#v", observed, handler)
		}
	})
}

func TestHandleMessageWithOriginSurfacesApplyErrors(t *testing.T) {
	receiver := crdt.NewDoc("origin-error", crdt.WithGC(false))
	handler := NewSyncHandler(receiver)
	// The outer and sync body framing are valid, so InspectMessage accepts this;
	// the update body itself is truncated and ApplyUpdate must surface that error.
	frame := EncodeUpdate([]byte{0})
	if _, err := InspectMessage(frame); err != nil {
		t.Fatalf("valid frame was rejected before apply: %v", err)
	}
	if _, err := handler.HandleMessageWithOrigin(frame, &bytes.Buffer{}, "connection"); err == nil {
		t.Fatal("malformed update was logged/swallowed instead of returned")
	}
}

func TestHandleMessageWithOriginSurfacesStep1ReplyErrors(t *testing.T) {
	doc := crdt.NewDoc("origin-step1-error", crdt.WithGC(false))
	handler := NewSyncHandler(doc)

	// A one-byte state vector that declares one client but contains no client or
	// clock is canonically framed yet invalid. The former ReadSyncMessage route
	// logged WriteSyncStep2's error and returned success with no reply.
	var syncPayload bytes.Buffer
	lib0.WriteVarUint(&syncPayload, SyncMessageStep1)
	lib0.WriteVarUint8Array(&syncPayload, []byte{1})
	var frame bytes.Buffer
	WriteMessage(&frame, MessageSync, syncPayload.Bytes())
	var reply bytes.Buffer
	if _, err := handler.HandleMessageWithOrigin(frame.Bytes(), &reply, "connection"); err == nil {
		t.Fatal("malformed Step1 was logged/swallowed instead of returned")
	}
	if reply.Len() != 0 {
		t.Fatalf("malformed Step1 produced a partial reply: %x", reply.Bytes())
	}
}

func TestSyncOverrideRetainsRawPayloadPrecedence(t *testing.T) {
	doc := crdt.NewDoc("sync-override", crdt.WithGC(false))
	handler := NewSyncHandler(doc)
	// A built-in Update body with a truncated length prefix is deliberately used:
	// the whole-sync override owns this grammar and must receive the raw payload
	// before the built-in inspector rejects it.
	payload := []byte{SyncMessageUpdate, 0x80}
	var frame bytes.Buffer
	WriteMessage(&frame, MessageSync, payload)
	var got []byte
	handler.RegisterHandler(MessageSync, func(raw []byte) error {
		got = append([]byte(nil), raw...)
		return nil
	})
	if _, err := handler.HandleMessageWithOrigin(frame.Bytes(), &bytes.Buffer{}, "connection"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("sync override payload = %x, want raw %x", got, payload)
	}
	if _, err := InspectMessage(frame.Bytes()); err == nil {
		t.Fatal("standalone inspector accepted the deliberately malformed built-in body")
	}
}
