package crdt

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// T077. These paths were TOUCHED by this feature but left unexercised: varint overflow guards,
// the spliceArray overwrite branch, ContentBinary.Copy's defensive copy, and awarenessStatesKeys.
// Each defends a real invariant — a missing overflow guard is a decoder that accepts hostile bytes
// and a shared backing array is silent cross-document corruption.

// A 10-byte varint whose final byte carries more than one bit cannot fit in 64 bits. Accepting it
// would wrap silently and yield an attacker-chosen clock.
func TestReadVarIntRejectsOverflow(t *testing.T) {
	// Encoding: the first byte carries 6 magnitude bits, bit7 = sign, bit8 = continuation; each
	// continuation byte adds 7 more. The guard fires on the 10th continuation byte (i==9) when it
	// carries more than the single bit that still fits in 64.
	over := append([]byte{0x80}, bytes.Repeat([]byte{0x80}, 9)...)
	over = append(over, 0x02)
	if _, err := readVarInt(bytes.NewBuffer(over)); !errors.Is(err, errOverflow) {
		t.Errorf("ReadVarInt on an over-64-bit varint returned err=%v, want errOverflow", err)
	}
	// Continuation bits that never terminate within the maximum width must also be rejected.
	neverEnds := append([]byte{0x80}, bytes.Repeat([]byte{0x80}, 10)...)
	if _, err := readVarInt(bytes.NewBuffer(neverEnds)); !errors.Is(err, errOverflow) {
		t.Errorf("ReadVarInt on a never-terminating varint returned err=%v, want errOverflow", err)
	}
	// A continuation bit that never terminates must also be rejected rather than read past.
	unterminated := []byte{0x80, 0x80, 0x80}
	if _, err := readVarInt(bytes.NewBuffer(unterminated)); err == nil {
		t.Error("ReadVarInt accepted an unterminated varint")
	}
	// And a legal value must still decode, so the guard is not simply rejecting everything.
	if v, err := readVarInt(bytes.NewBuffer([]byte{0x02})); err != nil || v.(Number) != 2 {
		t.Errorf("ReadVarInt(0x02) = %v, %v; want 2, nil", v, err)
	}
	// bit7 set means negative: 0x42 is magnitude 2 with the sign bit.
	if v, err := readVarInt(bytes.NewBuffer([]byte{0x42})); err != nil || v.(Number) != -2 {
		t.Errorf("ReadVarInt(0x42) = %v, %v; want -2, nil", v, err)
	}
}

func TestReadVarIntSignedRejectsOverflow(t *testing.T) {
	// shift reaches 62 on the 9th continuation byte, where only 2 bits still fit; a chunk of 4
	// therefore has a bit that would be lost.
	over := append([]byte{0x80}, bytes.Repeat([]byte{0x80}, 8)...)
	over = append(over, 0x04)
	if _, _, err := readVarIntSigned(bytes.NewBuffer(over)); !errors.Is(err, errOverflow) {
		t.Errorf("readVarIntSigned on an over-wide varint returned err=%v, want errOverflow", err)
	}
	mag, neg, err := readVarIntSigned(bytes.NewBuffer([]byte{0x03}))
	if err != nil || neg || mag != 3 {
		t.Errorf("readVarIntSigned(0x03) = (%d,%v,%v); want (3,false,nil)", mag, neg, err)
	}
	// bit7 marks the magnitude negative without changing it.
	mag, neg, err = readVarIntSigned(bytes.NewBuffer([]byte{0x43}))
	if err != nil || !neg || mag != 3 {
		t.Errorf("readVarIntSigned(0x43) = (%d,%v,%v); want (3,true,nil)", mag, neg, err)
	}
}

// spliceArray's fast path overwrites in place when at least as many elements are removed as
// inserted. It must still produce exactly the JS splice result, including the shortened length.
func TestSpliceArrayOverwriteBranch(t *testing.T) {
	cases := []struct {
		name        string
		in          ArrayAny
		start       int
		deleteCount int
		elements    ArrayAny
		want        ArrayAny
	}{
		{"delete more than inserted", ArrayAny{1, 2, 3, 4, 5}, 1, 3, ArrayAny{9}, ArrayAny{1, 9, 5}},
		{"delete exactly as many", ArrayAny{1, 2, 3, 4}, 1, 2, ArrayAny{8, 9}, ArrayAny{1, 8, 9, 4}},
		{"pure delete", ArrayAny{1, 2, 3}, 0, 2, nil, ArrayAny{3}},
		{"delete to the end", ArrayAny{1, 2, 3}, 1, 2, ArrayAny{7}, ArrayAny{1, 7}},
		{"insert more than deleted", ArrayAny{1, 2, 3}, 1, 1, ArrayAny{7, 8, 9}, ArrayAny{1, 7, 8, 9, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := make(ArrayAny, len(tc.in))
			copy(a, tc.in)
			spliceArray(&a, tc.start, tc.deleteCount, tc.elements)
			if len(a) != len(tc.want) {
				t.Fatalf("len = %d (%v), want %d (%v)", len(a), a, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if a[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", a, tc.want)
				}
			}
		})
	}
}

// Copy must DEEP-copy the bytes. Sharing the backing array means mutating one document's binary
// content silently mutates another's.
func TestContentBinaryCopyIsDeep(t *testing.T) {
	orig := newContentBinary([]uint8{1, 2, 3})
	dup, ok := orig.copyContent().(*contentBinary)
	if !ok {
		t.Fatal("Copy did not return a *ContentBinary")
	}
	dup.value[0] = 99
	if orig.value[0] != 1 {
		t.Errorf("mutating the copy changed the original (%v); Copy must not share the backing array",
			orig.value)
	}
	if len(dup.value) != 3 || dup.value[1] != 2 || dup.value[2] != 3 {
		t.Errorf("copy lost content: %v", dup.value)
	}
}

func TestAwarenessStatesKeys(t *testing.T) {
	if got := awarenessStatesKeys(map[Number]Object{}); len(got) != 0 {
		t.Errorf("empty states gave %v, want no keys", got)
	}
	states := map[Number]Object{7: newObject(), 42: newObject()}
	got := awarenessStatesKeys(states)
	if len(got) != 2 {
		t.Fatalf("got %d keys, want 2", len(got))
	}
	seen := map[Number]bool{}
	for _, k := range got {
		seen[k] = true
	}
	if !seen[7] || !seen[42] {
		t.Errorf("keys = %v, want both 7 and 42", got)
	}
}

// DeleteSet.ClientOrder must be TOTAL and deterministic. Clients recorded through noteClient keep
// their first-insertion order (the undo-ordering fix depends on it), but a client present in the
// map without having been noted — e.g. one added by a decoder path that bypasses noteClient — must
// still appear, in sorted order, rather than vanish from every iteration built on this.
func TestDeleteSetClientOrderIsTotal(t *testing.T) {
	ds := newDeleteSet()
	addToDeleteSet(ds, 30, 0, 1)
	addToDeleteSet(ds, 10, 0, 1)
	addToDeleteSet(ds, 20, 0, 1)

	if got := ds.orderedClients(); len(got) != 3 || got[0] != 30 || got[1] != 10 || got[2] != 20 {
		t.Fatalf("ClientOrder = %v, want first-insertion order [30 10 20]", got)
	}

	// Inject a client directly into the map, as a decode path that skips noteClient would.
	ds.clients[5] = []*deleteItem{{clock: 0, length: 1}}
	ds.clients[1] = []*deleteItem{{clock: 0, length: 1}}
	got := ds.orderedClients()
	if len(got) != 5 {
		t.Fatalf("ClientOrder = %v, want all 5 clients; an unnoted client would be skipped by "+
			"every iteration built on this", got)
	}
	// The noted ones keep insertion order; the rest are appended sorted, so the result is
	// deterministic rather than dependent on Go's randomised map iteration.
	if got[0] != 30 || got[1] != 10 || got[2] != 20 {
		t.Errorf("noted clients lost their insertion order: %v", got)
	}
	if got[3] != 1 || got[4] != 5 {
		t.Errorf("unnoted clients = %v, want them appended in sorted order [1 5]", got[3:])
	}
}

// The managed presence type's accessors must all delegate to the same underlying Awareness, and
// Start must be idempotent — two timers renewing the same clock would double the update rate.
func TestManagedAwarenessAccessorsAndIdempotentStart(t *testing.T) {
	base := NewAwareness(newDoc("g", true, defaultGCFilter, nil, false, WithClientID(3)))
	m := NewManagedAwarenessFrom(base)
	defer base.Destroy()
	if m.Awareness() != base {
		t.Fatal("NewManagedAwarenessFrom did not adopt the given Awareness")
	}

	st := newObject()
	st.Set("k", "v")
	_ = m.SetLocalState(st)
	if got := m.GetLocalState(); got.IsNil() {
		t.Error("GetLocalState returned nothing after SetLocalState")
	}
	if _, ok := m.GetStates()[base.ClientID]; !ok {
		t.Error("GetStates does not include the local client")
	}
	if _, ok := m.GetMeta()[base.ClientID]; !ok {
		t.Error("GetMeta does not include the local client")
	}

	fired := 0
	m.On("update", NewObserverHandler(func(...interface{}) { fired++ }))
	st2 := newObject()
	st2.Set("k", "w")
	_ = m.SetLocalState(st2)
	if fired == 0 {
		t.Error("On did not register an observer on the underlying Awareness")
	}

	m.Start()
	defer m.Stop()
	if !m.Running() {
		t.Fatal("Start did not start the timer")
	}
	m.Start() // must be a no-op, not a second goroutine
	if !m.Running() {
		t.Error("second Start stopped the timer")
	}
	m.Stop()
	if m.Running() {
		t.Error("Stop did not stop the timer")
	}
	// A Start/Stop cycle must be repeatable — stopOnce is reset for exactly this.
	m.Start()
	if !m.Running() {
		t.Error("the value could not be restarted after Stop")
	}
}

// ToDelta with a snapshot must annotate content the snapshot cannot see as "removed", and content
// invisible in the PREVIOUS snapshot as "added" — the y-change machinery editors render as track
// changes. Untested, this silently degrades to a plain delta.
func TestToDeltaWithSnapshotsMarksChanges(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "abc", Object{})
	before := NewSnapshotByDoc(doc)

	txt.Insert(3, "XY", Object{}) // present now, absent in `before`
	txt.Delete(0, 1)              // absent now, present in `before`
	after := NewSnapshotByDoc(doc)

	ops := txt.ToDelta(after, before, nil)
	if len(ops) == 0 {
		t.Fatal("ToDelta with snapshots produced nothing")
	}
	var sawRemoved, sawAdded bool
	for _, op := range ops {
		if !op.HasAttributes() {
			continue
		}
		yc, _ := op.Attributes.Get("ychange")
		obj, ok := yc.(Object)
		if !ok || obj.IsNil() {
			continue
		}
		switch obj.GetOr("type") {
		case "removed":
			sawRemoved = true
		case "added":
			sawAdded = true
		}
	}
	if !sawRemoved {
		t.Error("deleted content was not marked ychange:removed")
	}
	if !sawAdded {
		t.Error("content absent from the previous snapshot was not marked ychange:added")
	}

	// computeYChange must be consulted when supplied, so an editor can attach its own metadata.
	called := 0
	custom := txt.ToDelta(after, before, func(kind string, id *ID) Object {
		called++
		return MakeObject("type", kind, "mine", true)
	})
	if called == 0 {
		t.Error("computeYChange was never called")
	}

	// Byte-for-byte against the reference. yjs@13.6.31 on the identical op stream with clientID
	// pinned to 1 produces exactly these deltas; "does not panic" is not the bar.
	assertDeltaJSON := func(label string, got []EventOperator, want string) {
		t.Helper()
		b, err := json.Marshal(deltaShape(got))
		if err != nil {
			t.Fatal(err)
		}
		if canonJSON(t, string(b)) != canonJSON(t, want) {
			t.Errorf("%s delta\n got = %s\n ref = %s", label, b, want)
		}
	}
	assertDeltaJSON("plain", txt.ToDelta(nil, nil, nil),
		`[{"insert":"bcXY"}]`)
	assertDeltaJSON("snapshot", ops,
		`[{"insert":"a","attributes":{"ychange":{"type":"removed"}}},`+
			`{"insert":"bc"},`+
			`{"insert":"XY","attributes":{"ychange":{"type":"added"}}}]`)
	assertDeltaJSON("computeYChange", custom,
		`[{"insert":"a","attributes":{"ychange":{"type":"removed","mine":true}}},`+
			`{"insert":"bc"},`+
			`{"insert":"XY","attributes":{"ychange":{"type":"added","mine":true}}}]`)
}

// merge.go guards the internally-produced PendingDs buffer with a "leading 0 structs" header check.
// Both arms — an unreadable header and a non-zero struct count — mean the buffer is corrupt, and
// continuing would replay from a mispositioned decoder and apply the WRONG deletes. A silent
// mis-apply is far worse than a bail, so the guards need tests that prove they bail.
func TestApplyUpdateBailsOnCorruptPendingDeleteSet(t *testing.T) {
	// A well-formed update to apply on top of the corrupt pending buffer.
	src := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	src.GetText("t").Insert(0, "hello", Object{})
	upd, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		pending []uint8
	}{
		// An empty buffer cannot yield the header varint at all.
		{"unreadable header", []uint8{}},
		// A header claiming structs are present: PendingDs encodes deletes ONLY, so any
		// non-zero count means the buffer is not what this code produced.
		{"header claims structs", []uint8{0x05, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			doc.store.pendingDeleteSet = tc.pending

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("a corrupt pending delete set must bail, not panic: %v", r)
					}
				}()
				_ = ApplyUpdate(doc, upd, nil)
			}()

			// The guard bails before consuming the pending buffer, so it must still be there —
			// if it had been cleared, the corrupt deletes would have been treated as applied.
			if doc.store.pendingDeleteSet == nil {
				t.Error("corrupt pending delete set was consumed rather than rejected")
			}
		})
	}

	// Control: with no pending delete set, the same update applies normally. Without this the
	// assertions above could pass simply because ApplyUpdate never does anything.
	clean := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	_ = ApplyUpdate(clean, upd, nil)
	if got := clean.GetText("t").ToString(); got != "hello" {
		t.Errorf("control document = %q, want %q", got, "hello")
	}
}
