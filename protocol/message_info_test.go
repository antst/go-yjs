package protocol

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"unsafe"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

func TestInspectMessageClassifiesCanonicalBodiesWithoutCopying(t *testing.T) {
	doc := crdt.NewDoc("inspect-message", crdt.WithGC(false), crdt.WithClientID(17))
	doc.GetText("t").Insert(0, "body", crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	step2, err := EncodeSyncStep2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	stateVector := crdt.EncodeStateVector(doc)

	awarenessDoc := crdt.NewDoc("inspect-awareness", crdt.WithClientID(23))
	awareness := crdt.NewAwareness(awarenessDoc)
	t.Cleanup(awareness.Destroy)
	if err := awareness.SetLocalState(crdt.MakeObject("name", "inspect")); err != nil {
		t.Fatal(err)
	}
	awarenessBody := crdt.EncodeAwarenessUpdate(awareness, []crdt.Number{awareness.ClientID}, nil)

	var unknownPayload bytes.Buffer
	lib0.WriteVarUint(&unknownPayload, 37)
	unknownBody := []byte{0xaa, 0xbb, 0xcc}
	unknownPayload.Write(unknownBody)
	var unknownFrame bytes.Buffer
	WriteMessage(&unknownFrame, MessageSync, unknownPayload.Bytes())

	var customFrame bytes.Buffer
	customBody := []byte("custom-body")
	WriteMessage(&customFrame, 42, customBody)

	cases := []struct {
		name     string
		frame    []byte
		msgType  uint8
		syncType int
		body     []byte
	}{
		{"sync-step1", EncodeSyncStep1(doc), MessageSync, SyncMessageStep1, stateVector},
		{"sync-step2", step2, MessageSync, SyncMessageStep2, update},
		{"sync-update", EncodeUpdate(update), MessageSync, SyncMessageUpdate, update},
		{"awareness", EncodeAwarenessUpdateMessage(awarenessBody), MessageAwareness, SyncMessageNone, awarenessBody},
		{"unknown-sync", unknownFrame.Bytes(), MessageSync, 37, unknownBody},
		{"custom", customFrame.Bytes(), 42, SyncMessageNone, customBody},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			info, err := InspectMessage(test.frame)
			if err != nil {
				t.Fatal(err)
			}
			if info.Type != test.msgType || info.SyncType != test.syncType {
				t.Fatalf("classification = type %d sync %d, want type %d sync %d", info.Type, info.SyncType, test.msgType, test.syncType)
			}
			if info.FrameLength != len(test.frame) || info.BodyLength != len(info.Body) {
				t.Fatalf("lengths = frame %d body %d/%d, want frame %d", info.FrameLength, info.BodyLength, len(info.Body), len(test.frame))
			}
			if test.body != nil && !bytes.Equal(info.Body, test.body) {
				t.Fatalf("body = %x, want %x", info.Body, test.body)
			}
			assertSliceViewsFrame(t, test.frame, info.Body)
		})
	}
}

func assertSliceViewsFrame(t *testing.T, frame, body []byte) {
	t.Helper()
	if len(body) == 0 {
		return
	}
	frameStart := uintptr(unsafe.Pointer(unsafe.SliceData(frame)))
	bodyStart := uintptr(unsafe.Pointer(unsafe.SliceData(body)))
	frameEnd := frameStart + uintptr(len(frame))
	if bodyStart < frameStart || bodyStart >= frameEnd || bodyStart+uintptr(len(body)) > frameEnd {
		t.Fatalf("body [%#x,%#x) does not view frame [%#x,%#x)", bodyStart, bodyStart+uintptr(len(body)), frameStart, frameEnd)
	}
}

var inspectedMessageSink MessageInfo

func TestInspectMessageDoesNotAllocateOrCopyTheBody(t *testing.T) {
	doc := crdt.NewDoc("inspect-allocs", crdt.WithGC(false), crdt.WithClientID(27))
	doc.GetText("t").Insert(0, "allocation-free body view", crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := EncodeUpdate(update)

	var inspectErr error
	allocs := testing.AllocsPerRun(1000, func() {
		inspectedMessageSink, inspectErr = InspectMessage(frame)
	})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if allocs != 0 {
		t.Fatalf("InspectMessage allocated %.2f times per frame, want zero", allocs)
	}
	assertSliceViewsFrame(t, frame, inspectedMessageSink.Body)
}

func TestInspectMessageRejectsMalformedBodiesBeforeReturningAView(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{"awareness-declared-length", []byte{MessageAwareness, 5, 1, 2}},
		{"sync-declared-length", []byte{MessageSync, SyncMessageUpdate, 5, 1, 2}},
		{"sync-truncated-type", []byte{MessageSync, 0x80}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectMessage(test.frame); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
			}
		})
	}

	var payload bytes.Buffer
	lib0.WriteVarUint(&payload, math.MaxInt32+1)
	var frame bytes.Buffer
	WriteMessage(&frame, MessageSync, payload.Bytes())
	if _, err := InspectMessage(frame.Bytes()); !errors.Is(err, ErrInvalidSyncMessageType) {
		t.Fatalf("oversized sync subtype: want ErrInvalidSyncMessageType, got %v", err)
	}
}

func TestInspectMessageBodyStopsAtDeclaredLength(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{"awareness", []byte{MessageAwareness, 1, 0xaa, 0xbb}},
		{"sync-update", []byte{MessageSync, SyncMessageUpdate, 1, 0xaa, 0xbb}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, err := InspectMessage(test.frame)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(info.Body, []byte{0xaa}) || info.BodyLength != 1 || info.FrameLength != len(test.frame) {
				t.Fatalf("inspection lengths/body = frame %d body %d/%x, want frame %d body 1/aa", info.FrameLength, info.BodyLength, info.Body, len(test.frame))
			}
		})
	}
}

func TestInspectMessagePreservesOuterTypeErrorClasses(t *testing.T) {
	var outOfRange bytes.Buffer
	lib0.WriteVarUint(&outOfRange, math.MaxUint8+1)
	tests := []struct {
		name  string
		frame []byte
		want  error
	}{
		{"empty", nil, ErrShortMessage},
		{"truncated", []byte{0x80}, ErrShortMessage},
		{"out-of-range", outOfRange.Bytes(), ErrInvalidMessageType},
		{"overflow", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, ErrInvalidMessageType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectMessage(test.frame); !errors.Is(err, test.want) {
				t.Fatalf("want errors.Is(%v), got %v", test.want, err)
			}
		})
	}
}

func TestInspectMessageSupportsPreApplyPolicyWithoutMutation(t *testing.T) {
	source := crdt.NewDoc("inspect-policy", crdt.WithGC(false), crdt.WithClientID(29))
	source.GetText("t").Insert(0, "candidate-state", crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := EncodeUpdate(update)

	receiver := crdt.NewDoc("inspect-policy", crdt.WithGC(false))
	info, err := InspectMessage(frame)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != MessageSync || info.SyncType != SyncMessageUpdate || !bytes.Equal(info.Body, update) {
		t.Fatalf("inspection did not expose the canonical update body: %+v", info)
	}

	// Model a relay's post-apply size gate: scratch-apply the inspected update,
	// then reject it without ever invoking the real handler.
	scratch := crdt.NewDoc("inspect-policy", crdt.WithGC(false))
	if err := crdt.ApplyUpdate(scratch, info.Body, "size-gate"); err != nil {
		t.Fatal(err)
	}
	if scratch.GetText("t").ToString() != "candidate-state" {
		t.Fatal("inspected body was not independently applicable")
	}
	if receiver.GetText("t").ToString() != "" {
		t.Fatalf("InspectMessage mutated the target document: %q", receiver.GetText("t").ToString())
	}
}
