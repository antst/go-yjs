package crdt

import (
	"bytes"
	"math"
	"testing"
)

func TestAwareness(t *testing.T) {
	doc1 := newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)
	doc2 := newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)

	doc1.ClientID = 0
	doc2.ClientID = 1

	aw1 := NewAwareness(doc1)
	aw2 := NewAwareness(doc2)

	clientID1 := aw1.ClientID
	clientID2 := aw2.ClientID

	aw1.On("update", NewObserverHandler(func(v ...interface{}) {
		clients := []Number{0}
		states := map[Number]Object{0: MakeObject("updated", "updated"), 1: MakeObject("added", "added", "removed", "removed")}
		enc := EncodeAwarenessUpdate(aw1, clients, states)
		// ApplyAwarenessUpdate returns an error; assert it inside the (synchronous)
		// observer callback. Use Errorf, not Fatalf: a Goexit mid-Emit would abort the
		// observer dispatch loop rather than just failing the test.
		if err := ApplyAwarenessUpdate(aw2, enc, "custom"); err != nil {
			t.Errorf("ApplyAwarenessUpdate (update observer) failed: %v", err)
		}
	}))

	var lastChangeLocal interface{}
	aw1.On("change", NewObserverHandler(func(v ...interface{}) {
		lastChangeLocal = MakeObject("change", "change")
	}))

	var lastChange interface{}
	aw2.On("change", NewObserverHandler(func(v ...interface{}) {
		lastChange = MakeObject("change", "change")
	}))

	if lastChangeLocal != nil {
		t.Errorf("expected last change local to be nil")
	}

	if lastChange != nil {
		t.Errorf("expected last change to be nil")
	}

	_ = aw1.SetLocalState(MakeObject("x", 3))
	_ = aw1.SetLocalStateField("hello", "world")

	aw1LocalState := aw1.GetLocalState()
	aw2LocalState := aw2.GetLocalState()
	aw2State := aw2.GetStates()

	clients := []Number{0}
	states := map[Number]Object{0: MakeObject("updated", "updated"), 1: MakeObject("added", "added", "removed", "removed")}
	enc := EncodeAwarenessUpdate(aw1, clients, states)
	if enc == nil {
		t.Errorf("expected enc to be non-nil")
	}

	if err := ApplyAwarenessUpdate(aw2, enc, "custom"); err != nil {
		t.Fatalf("ApplyAwarenessUpdate failed: %v", err)
	}
	RemoveAwarenessStates(aw1, clients, "timeout")

	aw1.Destroy()
	aw2.Destroy()

	if aw1LocalState.IsNil() {
		t.Errorf("expected aw1 local to be non nil")
	}

	if aw2LocalState.IsNil() {
		t.Errorf("expected aw2 local to be non nil")
	}

	if aw2State == nil {
		t.Errorf("expected aw2 state to be non nil")
	}

	if clientID1 != 0 {
		t.Errorf("expected clientID1 to be 0")
	}

	if clientID2 != 1 {
		t.Errorf("expected clientID2 to be 1")
	}

	if aw1 == nil {
		t.Errorf("expected aw1 to be non nil")
	}

	if aw2 == nil {
		t.Errorf("expected aw2 to be non nil")
	}
}

func TestSync(t *testing.T) {
	// ReadSyncMessage
	var mask = []byte{0x1, 0x3, 0x7, 0xf, 0x1f, 0x3f, 0x7f}
	decoder := newUpdateDecoderV1(mask)
	encoder := newUpdateEncoderV1()
	doc1 := newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)

	readSyncMessageForTest(decoder, encoder, doc1, "snapshot")

	// change mask
	mask = []byte{0x2, 0x3, 0x7, 0xf}
	decoder = newUpdateDecoderV1(mask)
	encoder = newUpdateEncoderV1()
	doc1 = newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)

	readSyncMessageForTest(decoder, encoder, doc1, "snapshot")

	// WriteSyncStep1
	encoder = newUpdateEncoderV1()
	doc1 = newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)

	writeSyncStep1(encoder, doc1)

	// WriteSyncStep1FromUpdate
	encoder = newUpdateEncoderV1()
	doc1 = newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)
	writeSyncStep1(encoder, doc1)

	encoder1 := newUpdateEncoderV1()
	update := mustBytes(EncodeStateAsUpdate(doc1, nil))
	if err := writeSyncStep1FromUpdate(encoder1, update); err != nil {
		t.Fatalf("WriteSyncStep1FromUpdate: %v", err)
	}

	if !bytes.Equal(encoder.rest.Bytes(), encoder1.rest.Bytes()) {
		t.Errorf("expected rest encoder to be equal")
	}

	// WriteSyncStep2FromUpdate
	encoder = newUpdateEncoderV1()
	doc1 = newDoc("6148fcd6-9d8c-4fbd-8420-676f5931f7aa", true, defaultGCFilter, nil, false)
	if err := writeSyncStep2(encoder, doc1, nil); err != nil {
		t.Fatalf("WriteSyncStep2: %v", err)
	}

	encoder1 = newUpdateEncoderV1()
	update = mustBytes(EncodeStateAsUpdate(doc1, nil))
	if err := writeSyncStep2FromUpdate(encoder1, update, nil); err != nil {
		t.Fatalf("WriteSyncStep2FromUpdate: %v", err)
	}

	if !bytes.Equal(encoder.rest.Bytes(), encoder1.rest.Bytes()) {
		t.Errorf("expected rest encoder to be equal")
	}

	// WriteSyncStep1
	mask = []byte{0x2, 0x3, 0x7, 0xf}
	encoder = newUpdateEncoderV1()
	writeUpdate(encoder, mask)
}

// TestReadSyncStep2SkipsApplyOnTruncatedPayload guards that ReadSyncStep2 (and
// the Update path that shares it) does NOT apply a payload whose VarUint8Array
// length exceeds the bytes actually present. After the SEC-002 hardening
// ReadVarUint8Array returns io.ErrUnexpectedEOF for such a frame; ReadSyncStep2
// must surface that and skip ApplyUpdate rather than apply the bytes.
//
// To make the assertion distinguish "skipped" from "applied", the payload is a
// FULL VALID update whose declared length is overstated. In the buggy
// (error-discarding) path ReadVarUint8Array still hands back the real update
// bytes and ApplyUpdate mutates the doc; the fixed path sees the error and skips,
// leaving the doc pristine.
func TestReadSyncStep2SkipsApplyOnTruncatedPayload(t *testing.T) {
	// Produce a real, applicable update from a source doc.
	src := newDoc("guid", true, defaultGCFilter, nil, false)
	src.GetText("t").Insert(0, "hello", Object{})
	validUpdate := mustBytes(EncodeStateAsUpdate(src, nil))
	if len(validUpdate) == 0 {
		t.Fatal("empty source update")
	}

	// Frame it as a sync-step2 message but overstate the VarUint8Array length so
	// it exceeds the bytes that follow → ReadVarUint8Array reports truncation.
	rest := new(bytes.Buffer)
	writeVarUint(rest, uint64(messageYjsSyncStep2))
	writeVarUint(rest, uint64(len(validUpdate)+10)) // declared > available
	rest.Write(validUpdate)

	decoder := newUpdateDecoderV1(rest.Bytes())
	encoder := newUpdateEncoderV1()
	dst := newDoc("guid", true, defaultGCFilter, nil, false)

	before := encodeStateVectorWith(dst, nil, newUpdateEncoderV1())
	readSyncMessageForTest(decoder, encoder, dst, "remote")
	after := encodeStateVectorWith(dst, nil, newUpdateEncoderV1())

	if !bytes.Equal(before, after) {
		t.Fatalf("ReadSyncStep2 applied a payload flagged as truncated: state vector changed")
	}
	if got := dst.GetText("t").ToString(); got != "" {
		t.Fatalf("ReadSyncStep2 applied a truncated update: text=%q (want empty)", got)
	}
}

// TestReadSyncMessageRejectsTruncatedType guards that ReadSyncMessage returns -1
// (rather than misclassifying as MessageYjsSyncStep1 == 0) when the message-type
// VarUint is truncated/missing. An empty rest buffer has no type byte.
func TestReadSyncMessageRejectsTruncatedType(t *testing.T) {
	decoder := newUpdateDecoderV1(nil) // empty: no type field
	encoder := newUpdateEncoderV1()
	doc := newDoc("guid", true, defaultGCFilter, nil, false)

	if got := readSyncMessageForTest(decoder, encoder, doc, "remote"); got != -1 {
		t.Fatalf("ReadSyncMessage on empty frame: want -1, got %d", got)
	}
}

// readSyncMessageType builds a rest buffer carrying just `typ` as a VarUint (no
// sub-payload) and returns ReadSyncMessage's classification of it.
func readSyncMessageType(t *testing.T, typ uint64) int {
	t.Helper()
	var rest bytes.Buffer
	writeVarUint(&rest, typ)
	decoder := newUpdateDecoderV1(rest.Bytes())
	encoder := newUpdateEncoderV1()
	doc := newDoc("guid", true, defaultGCFilter, nil, false)
	return readSyncMessageForTest(decoder, encoder, doc, "remote")
}

// TestReadSyncMessageRejectsOutOfRangeType guards the round-2 finding: the type is
// decoded as a uint64, but a hostile VarUint above the int range would, on the raw
// int() cast, wrap to an arbitrary (possibly negative) value. In particular
// math.MaxUint64 wraps to int(-1) — colliding with the truncation sentinel — and
// other huge values could wrap to a small valid type id. ReadSyncMessage must GUARD
// before the int() conversion: any type > math.MaxInt32 is classified as malformed
// and reported as -1, so a hostile type can never wrap into a valid/known type or
// collide ambiguously.
func TestReadSyncMessageRejectsOutOfRangeType(t *testing.T) {
	// The pathological value: int(math.MaxUint64) == -1 on a 64-bit int. Without the
	// guard this would "look like" the truncation sentinel; with the guard it is an
	// explicit malformed classification (still -1, but never the raw wrap).
	if got := readSyncMessageType(t, math.MaxUint64); got != -1 {
		t.Fatalf("type=math.MaxUint64 must be classified malformed (-1), got %d", got)
	}
	// Just past the boundary: math.MaxInt32+1 is out of the in-range window and must
	// be rejected (it must NOT round-trip as a large positive "valid-looking" type).
	if got := readSyncMessageType(t, math.MaxInt32+1); got != -1 {
		t.Fatalf("type=math.MaxInt32+1 must be classified malformed (-1), got %d", got)
	}
	// A value engineered to wrap to a SMALL positive int if the guard were absent:
	// math.MaxUint64 - 4 + 1 wraps to int(-4)+... — more directly, (1<<63)+5 wraps to
	// a negative; pick (math.MaxUint64 - 2) which casts to int(-3). Any of these must
	// be malformed, never silently treated as a type. Assert the broad property.
	for _, typ := range []uint64{1 << 63, math.MaxUint64 - 2, math.MaxInt64 + 3} {
		if got := readSyncMessageType(t, typ); got != -1 {
			t.Fatalf("hostile type=%d must be classified malformed (-1), got %d", typ, got)
		}
	}
}

// Control: the guard must not over-trigger. Known types still dispatch and report
// their real value, and an unknown-but-in-range type id (a plausible future type)
// round-trips its value as a no-op rather than being mislabeled malformed.
func TestReadSyncMessageInRangeTypesUnaffected(t *testing.T) {
	for _, typ := range []uint64{messageYjsSyncStep1, messageYjsSyncStep2, messageYjsUpdate} {
		if got := readSyncMessageType(t, typ); got != int(typ) {
			t.Fatalf("known type %d must round-trip, got %d", typ, got)
		}
	}
	// Unknown but representable (≤ math.MaxInt32): default no-op, value preserved.
	if got := readSyncMessageType(t, 5); got != 5 {
		t.Fatalf("unknown in-range type 5 must round-trip as no-op, got %d", got)
	}
	if got := readSyncMessageType(t, math.MaxInt32); got != math.MaxInt32 {
		t.Fatalf("boundary type math.MaxInt32 must round-trip, got %d", got)
	}
}
