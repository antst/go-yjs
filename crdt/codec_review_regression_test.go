package crdt

import (
	"bytes"
	"testing"
	"time"
)

// codec_review_regression_test.go covers the code-review findings on the V2
// codec. Each test maps to a numbered finding from that review and fails on the
// pre-fix code.

// --- Finding #1 / #2: MergeUpdatesWith honored the wrong wire format on the
// pending-update / out-of-order-apply path, so EncodeStateAsUpdateV2 silently
// emitted V1 bytes. Build a doc that carries PendingStructs (via a partial
// out-of-order apply) and assert the re-encode is genuinely V2-framed and round
// trips. ---

// buildPendingV2Doc returns (dst, fullV2) where dst has PendingStructs set
// because only the *second* of two sequential inserts was applied to it, and
// fullV2 is the complete V2 update that supplies the missing first insert.
func buildPendingV2Doc(t *testing.T) (dst *Doc, fullV2 []byte) {
	t.Helper()
	src := newDoc("g", true, defaultGCFilter, nil, false)
	tx := src.GetText("t")
	tx.Insert(0, "AAAA", Object{})
	svAfterFirst := encodeStateVectorWith(src, nil, newUpdateEncoderV1())
	tx.Insert(4, "BBBB", Object{})

	fullV2 = mustBytes(EncodeStateAsUpdateV2(src, nil))
	// diffV2 holds only the second insert, which causally depends on the first.
	diffV2 := mustBytes(DiffUpdateV2(fullV2, svAfterFirst))

	dst = newDoc("g", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(dst, diffV2, nil)
	if dst.store.pendingStructs == nil {
		t.Fatal("setup: expected PendingStructs after partial out-of-order apply")
	}
	return dst, fullV2
}

func TestRegressionPendingUpdateEncodesV2(t *testing.T) {
	dst, fullV2 := buildPendingV2Doc(t)

	v2out := mustBytes(EncodeStateAsUpdateV2(dst, nil))
	v1out := mustBytes(EncodeStateAsUpdate(dst, nil))

	// Pre-fix, MergeUpdatesWith hardcoded a V1 encoder, so the "V2" output was
	// byte-identical to the V1 output. They must differ now.
	if bytes.Equal(v2out, v1out) {
		t.Fatalf("EncodeStateAsUpdateV2 on a pending doc emitted V1 bytes (finding #1)\n  bytes=%v", v2out)
	}

	// The V2 output must actually decode as V2: feeding it to a V2 decoder and
	// then supplying the missing first insert must converge to the full text.
	conv := newDoc("g", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(conv, v2out, nil)
	_ = ApplyUpdateV2(conv, fullV2, nil)
	if got := conv.GetText("t").ToString(); got != "AAAABBBB" {
		t.Fatalf("pending V2 update did not round-trip: want %q got %q", "AAAABBBB", got)
	}

	// And the V2 frame must be V2-decodable in isolation (no panic, decodes the
	// feature flag + columns).
	if applyV2NoPanic(v2out) {
		t.Fatalf("pending V2 update panicked under V2 decode")
	}
}

func TestRegressionPendingDsRoundTripV2(t *testing.T) {
	// store.PendingDs is V2-written but V2-read; with the V1 hardcode the merged
	// re-encode of two pending delete sets diverged. Exercise a delete that lands
	// out of order so PendingDs is populated, then re-encode V2 and converge.
	src := newDoc("g", true, defaultGCFilter, nil, false)
	arr := src.GetArray("a")
	src.Transact(func(trans *Transaction) {
		for i := 0; i < 6; i++ {
			arr.Push(ArrayAny{i})
		}
	}, nil)
	svEarly := encodeStateVectorWith(src, nil, newUpdateEncoderV1())
	// delete in a later transaction so the delete set references structs the
	// receiver (synced only to svEarly) may not have yet.
	src.Transact(func(trans *Transaction) {
		arr.Delete(0, 2)
	}, nil)

	fullV2 := mustBytes(EncodeStateAsUpdateV2(src, nil))
	diffV2 := mustBytes(DiffUpdateV2(fullV2, svEarly))

	dst := newDoc("g", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(dst, diffV2, nil)

	// Re-encode dst as V2 (pending path) and converge a third doc.
	out := mustBytes(EncodeStateAsUpdateV2(dst, nil))
	if applyV2NoPanic(out) {
		t.Fatalf("re-encoded pending V2 update panicked under V2 decode")
	}
	conv := newDoc("g", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(conv, fullV2, nil)
	_ = ApplyUpdateV2(conv, out, nil)
	if got := conv.GetArray("a").GetLength(); got != 4 {
		t.Fatalf("pending DS V2 round-trip: want array length 4 got %d", got)
	}
}

// --- Finding #2 / #3: UpdateDecoderV2.ReadKey must reject a negative or
// out-of-range key clock instead of panicking on v2.keys[-1]. ---

func TestRegressionReadKeyNegativeNoPanic(t *testing.T) {
	dec := &updateDecoderV2{}
	// Seed a keyClock column that decodes to a negative running value. An
	// IntDiffOptRle single value of -1 encodes via writeVarIntSigned.
	keyClock := new(bytes.Buffer)
	writeVarIntSigned(keyClock, -1*2 /*diff*2, no count*/)
	dec.keyClockDecoder = newIntDiffOptRLEDecoder(keyClock.Bytes())
	dec.stringDecoder = newStringDecoder([]byte{0}) // empty string column

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReadKey panicked on negative key clock (finding #2): %v", r)
		}
	}()
	_, err := dec.readKey()
	if err == nil {
		t.Fatalf("ReadKey: expected error on negative key clock, got nil")
	}
}

func TestRegressionReadKeyOutOfRangeErrors(t *testing.T) {
	dec := &updateDecoderV2{}
	// keyClock = 5 with an empty cache: in-range-but-wrong index must error, not
	// silently mis-pair (finding #3).
	keyClock := new(bytes.Buffer)
	writeVarIntSigned(keyClock, 5*2) // diff 5, single value
	dec.keyClockDecoder = newIntDiffOptRLEDecoder(keyClock.Bytes())
	dec.stringDecoder = newStringDecoder([]byte{0})

	if _, err := dec.readKey(); err == nil {
		t.Fatalf("ReadKey: expected error on out-of-range key clock (5 with empty cache)")
	}
}

func TestRegressionReadKeyHappyPath(t *testing.T) {
	// keyClock 0 then 0 again must return the same cached key; a real document
	// encodes keys this way, so the guard must not break the valid path.
	enc := newDefaultUpdateEncoderV2()
	_ = enc.writeKey("alpha")
	_ = enc.writeKey("beta")
	_ = enc.writeKey("alpha") // Yjs disables the cache, so this writes a 3rd key

	full := enc.toBytes()
	dec := newUpdateDecoderV2(full)
	got := []string{}
	for i := 0; i < 3; i++ {
		k, err := dec.readKey()
		if err != nil {
			t.Fatalf("ReadKey valid path errored at %d: %v", i, err)
		}
		got = append(got, k)
	}
	want := []string{"alpha", "beta", "alpha"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReadKey valid path: want %v got %v", want, got)
		}
	}
}

// --- Finding #4: UintOptRle must preserve the full uint64 range (values >= 2^63
// were corrupted by the int64 cast). ---

func TestRegressionUintOptRleFullRange(t *testing.T) {
	values := []uint64{0, 1, 1 << 31, 1 << 62, 1 << 63, (1 << 63) + 12345, 1<<64 - 1}
	enc := newDefaultUintOptRLEEncoder()
	for _, v := range values {
		enc.writeValue(v)
	}
	dec := newUintOptRLEDecoder(enc.bytes())
	for i, want := range values {
		got, err := dec.readValue()
		if err != nil {
			t.Fatalf("UintOptRle read %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("UintOptRle value %d: want %d got %d (finding #4)", i, want, got)
		}
	}
}

func TestRegressionUintOptRleHighRun(t *testing.T) {
	// A run of a >= 2^63 value must survive (the run path also cast through int64).
	v := uint64(1)<<63 + 99
	enc := newDefaultUintOptRLEEncoder()
	for i := 0; i < 5; i++ {
		enc.writeValue(v)
	}
	dec := newUintOptRLEDecoder(enc.bytes())
	for i := 0; i < 5; i++ {
		got, err := dec.readValue()
		if err != nil {
			t.Fatalf("UintOptRle run read %d: %v", i, err)
		}
		if got != v {
			t.Fatalf("UintOptRle run value %d: want %d got %d", i, v, got)
		}
	}
}

// --- Finding #5: IntDiffOptRle must not silently wrap on a diff that overflows
// the diff*2 framing. The threading-completion fix turns the former panic into a
// returned error (surfaced through ToUint8Array and the V2 encode entry points),
// so an out-of-range clock diff fails the encode gracefully rather than crashing. ---

func TestRegressionIntDiffOptRleOverflowErrors(t *testing.T) {
	enc := newDefaultIntDiffOptRLEEncoder()
	if err := enc.writeValue(0); err != nil {
		t.Fatalf("unexpected error writing 0: %v", err)
	}
	// diff 2^62 -> diff*2 overflows int64. Write flushes the previous (diff=0) run,
	// so it returns nil; the overflowing diff is only flushed at ToUint8Array.
	if err := enc.writeValue(int64(1) << 62); err != nil {
		t.Fatalf("unexpected error staging overflowing diff: %v", err)
	}

	out, err := enc.bytes()
	if err == nil {
		t.Fatalf("IntDiffOptRle: expected error on overflowing diff (finding #5), got nil")
	}
	if out != nil {
		t.Fatalf("IntDiffOptRle: expected nil bytes on error, got %v", out)
	}
}

func TestRegressionIntDiffOptRleNormalRange(t *testing.T) {
	// Clock-sized diffs must still round-trip exactly.
	values := []int64{0, 1, 2, 3, 100, 99, 98, -5, 1 << 30}
	enc := newDefaultIntDiffOptRLEEncoder()
	for _, v := range values {
		if err := enc.writeValue(v); err != nil {
			t.Fatalf("unexpected error writing %d: %v", v, err)
		}
	}
	data, err := enc.bytes()
	if err != nil {
		t.Fatalf("unexpected ToUint8Array error: %v", err)
	}
	dec := newIntDiffOptRLEDecoder(data)
	for i, want := range values {
		got, err := dec.readValue()
		if err != nil {
			t.Fatalf("IntDiffOptRle read %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("IntDiffOptRle value %d: want %d got %d", i, want, got)
		}
	}
}

// --- Finding #7: a nil *DeleteSet (from a truncated ReadDeleteSet) must not
// panic MergeDeleteSets. ---

func TestRegressionMergeDeleteSetsNilSafe(t *testing.T) {
	a := newDeleteSet()
	addToDeleteSet(a, 1, 0, 3)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MergeDeleteSets panicked on nil entry (finding #7): %v", r)
		}
	}()
	merged := mergeDeleteSets([]*deleteSet{nil, a, nil})
	if merged == nil {
		t.Fatalf("MergeDeleteSets returned nil")
	}
	if len(merged.clients[1]) == 0 {
		t.Fatalf("MergeDeleteSets dropped the non-nil delete set")
	}
}

// --- Finding #8 / #9: a corrupt struct count combined with an RleDecoder
// infinite-run sentinel must not spin the CPU. We bound the work and assert it
// returns promptly rather than hanging. ---

func TestRegressionCorruptStructCountTerminates(t *testing.T) {
	// Hand-craft a V1 update: numOfStateUpdates=1, numberOfStructs=huge,
	// client=1, clock=0, then a single GC info byte (0) and nothing else. The GC
	// branch reads a length that underflows to 0 and would otherwise loop the
	// huge count.
	buf := new(bytes.Buffer)
	writeVarUint(buf, 1)     // numOfStateUpdates
	writeVarUint(buf, 1<<31) // numberOfStructs (corrupt)
	writeVarUint(buf, 1)     // client
	writeVarUint(buf, 0)     // clock
	writeByte(buf, 0)        // one GC info byte; len column then empty

	done := make(chan struct{})
	go func() {
		dec := newUpdateDecoderV1(buf.Bytes())
		doc := newDoc("g", true, defaultGCFilter, nil, false)
		_, _ = readClientsStructRefs(dec, doc)
		close(done)
	}()
	select {
	case <-done:
		// returned promptly: the watchdog fired.
	case <-time.After(10 * time.Second):
		t.Fatalf("readClientsStructRefs spun on corrupt struct count (finding #8)")
	}
}

func TestRegressionCorruptLazyReaderTerminates(t *testing.T) {
	// Same corrupt update through the lazy struct reader generator path used by
	// MergeUpdates / ConvertUpdateFormat.
	buf := new(bytes.Buffer)
	writeVarUint(buf, 1)
	writeVarUint(buf, 1<<31)
	writeVarUint(buf, 1)
	writeVarUint(buf, 0)
	writeByte(buf, 0) // GC info, empty len

	done := make(chan struct{})
	go func() {
		// Result/err ignored: this test only asserts the corrupt-input decode
		// terminates promptly (the stall watchdog), not the conversion outcome.
		_, _ = ConvertUpdateFormatV1ToV2(buf.Bytes())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("lazy struct reader spun on corrupt struct count (finding #9)")
	}
}

// --- Finding #10: the string column decoder must surface an overrun error
// rather than silently truncating. ---

func TestRegressionStringDecoderOverrunErrors(t *testing.T) {
	// Concatenated string "ab" (2 units) but a length column claiming 5.
	concat := new(bytes.Buffer)
	_ = writeString(concat, "ab")
	lens := newDefaultUintOptRLEEncoder()
	lens.writeValue(5) // claim 5 units, only 2 exist
	writeUint8Array(concat, lens.bytes())

	dec := newStringDecoder(concat.Bytes())
	if _, err := dec.readValue(); err == nil {
		t.Fatalf("StringDecoder.Read: expected overrun error (finding #10), got nil")
	}
}

func TestRegressionStringDecoderHappyPath(t *testing.T) {
	concat := new(bytes.Buffer)
	_ = writeString(concat, "abcdef")
	lens := newDefaultUintOptRLEEncoder()
	lens.writeValue(3)
	lens.writeValue(3)
	writeUint8Array(concat, lens.bytes())

	dec := newStringDecoder(concat.Bytes())
	a, err := dec.readValue()
	if err != nil {
		t.Fatalf("StringDecoder.Read 1: %v", err)
	}
	b, err := dec.readValue()
	if err != nil {
		t.Fatalf("StringDecoder.Read 2: %v", err)
	}
	if a != "abc" || b != "def" {
		t.Fatalf("StringDecoder happy path: want abc/def got %q/%q", a, b)
	}
}

// --- Finding #14: WriteAny tag selection must mirror lib0. ---

func TestRegressionWriteAnyTags(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		wantTag byte
	}{
		{"small int -> varint(125)", Number(42), 125},
		{"float64 integer -> varint(125)", float64(7), 125},
		// 2^40 is a power of two and thus float32-exact -> lib0 emits float32(124).
		{"large float32-exact int -> float32(124)", Number(1) << 40, 124},
		// 2^40 + 1 is not float32-representable -> float64(123).
		{"large non-float32 int -> float64(123)", (Number(1) << 40) + 1, 123},
		{"float32-representable float64 -> 124", float64(0.5), 124},
		{"non-float32 float64 -> 123", float64(0.1), 123},
		{"int64 bigint -> 122", int64(5), 122},
		{"string -> 119", "x", 119},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			if err := writeAny(buf, c.value); err != nil {
				t.Fatalf("WriteAny(%v): %v", c.value, err)
			}
			got := buf.Bytes()[0]
			if got != c.wantTag {
				t.Fatalf("WriteAny(%v): want tag %d got %d", c.value, c.wantTag, got)
			}
		})
	}
}

func TestRegressionWriteAnyUnknownIsUndefined(t *testing.T) {
	// An unknown Go type must encode as undefined (127), not null (126).
	type weird struct{ X int }
	buf := new(bytes.Buffer)
	if err := writeAny(buf, weird{1}); err != nil {
		t.Fatalf("WriteAny(unknown): %v", err)
	}
	if got := buf.Bytes()[0]; got != 127 {
		t.Fatalf("WriteAny(unknown): want undefined tag 127 got %d (finding #14)", got)
	}
}

func TestRegressionWriteAnyFloat32RoundTrip(t *testing.T) {
	// A float32-representable non-integer round-trips via tag 124.
	buf := new(bytes.Buffer)
	if err := writeAny(buf, float64(0.5)); err != nil {
		t.Fatalf("WriteAny: %v", err)
	}
	v, err := readAny(bytes.NewBuffer(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAny: %v", err)
	}
	if f, ok := v.(float32); !ok || f != 0.5 {
		t.Fatalf("float32 round-trip: got %v (%T)", v, v)
	}
}
