package crdt

import (
	"bytes"
	"container/heap"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// ---------------------------------------------------------------- from merge_delete_sets_bench_test.go
var mergeDeleteSetBenchmarkSink *deleteSet

func BenchmarkMergeDeleteSets(b *testing.B) {
	benchReleaseSinks(b)
	b.Cleanup(func() { mergeDeleteSetBenchmarkSink = nil })
	b.Run("dense", func(b *testing.B) {
		sets := make([]*deleteSet, 16)
		for setIndex := range sets {
			sets[setIndex] = newDeleteSet()
			for client := Number(0); client < 16; client++ {
				for item := Number(0); item < 4; item++ {
					addToDeleteSet(sets[setIndex], client, setIndex*64+item*2, 1)
				}
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			mergeDeleteSetBenchmarkSink = mergeDeleteSets(sets)
		}
	})

	for _, setCount := range []int{256, 1024, 4096, 8192} {
		b.Run(fmt.Sprintf("sparse-first-seen-%d", setCount), func(b *testing.B) {
			sets := make([]*deleteSet, setCount)
			for setIndex := range sets {
				sets[setIndex] = newDeleteSet()
				addToDeleteSet(sets[setIndex], setIndex, 0, 1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				mergeDeleteSetBenchmarkSink = mergeDeleteSets(sets)
			}
		})
	}
}

// ---------------------------------------------------------------- from merge_delete_sets_ownership_test.go
type deleteItemValue struct {
	present bool
	clock   Number
	length  Number
}

type deleteSetValue struct {
	order   []Number
	clients map[Number][]deleteItemValue
}

func snapshotDeleteSetValue(ds *deleteSet) deleteSetValue {
	snapshot := deleteSetValue{
		order:   append([]Number(nil), ds.orderedClients()...),
		clients: make(map[Number][]deleteItemValue, len(ds.clients)),
	}
	for client, items := range ds.clients {
		values := make([]deleteItemValue, len(items))
		for i, item := range items {
			if item != nil {
				values[i] = deleteItemValue{present: true, clock: item.clock, length: item.length}
			}
		}
		snapshot.clients[client] = values
	}
	return snapshot
}

func TestMergeDeleteSetsOwnsRangesAndPreservesInputs(t *testing.T) {
	first := newDeleteSet()
	addToDeleteSet(first, 1, 10, 2)
	addToDeleteSet(first, 7, 5, 1)

	second := newDeleteSet()
	addToDeleteSet(second, 2, 20, 1)
	addToDeleteSet(second, 1, 0, 11)

	third := newDeleteSet()
	addToDeleteSet(third, 1, 10, 3)

	inputs := []*deleteSet{first, second, third}
	before := make([]deleteSetValue, len(inputs))
	for i, ds := range inputs {
		before[i] = snapshotDeleteSetValue(ds)
	}

	merged := mergeDeleteSets([]*deleteSet{first, nil, second, third})

	for i, ds := range inputs {
		if after := snapshotDeleteSetValue(ds); !reflect.DeepEqual(after, before[i]) {
			t.Fatalf("input %d mutated:\n got  %#v\n want %#v", i, after, before[i])
		}
	}
	if got, want := merged.orderedClients(), []Number{1, 7, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged client order=%v, want first-seen order %v", got, want)
	}
	wantRanges := map[Number][]deleteItemValue{
		1: {{present: true, clock: 0, length: 13}},
		7: {{present: true, clock: 5, length: 1}},
		2: {{present: true, clock: 20, length: 1}},
	}
	if got := snapshotDeleteSetValue(merged).clients; !reflect.DeepEqual(got, wantRanges) {
		t.Fatalf("merged ranges=%#v, want %#v", got, wantRanges)
	}
}

var mergeDeleteSetAllocationSink *deleteSet

func TestMergeDeleteSetsAvoidsReflectiveRangeCopies(t *testing.T) {
	const clients = 256
	sets := make([]*deleteSet, clients)
	for i := range sets {
		sets[i] = newDeleteSet()
		addToDeleteSet(sets[i], i, 0, 1)
	}
	if len(sets) != clients || len(sets[clients-1].clients) != 1 {
		t.Fatal("allocation fixture did not create one distinct client per set")
	}

	allocs := testing.AllocsPerRun(20, func() {
		mergeDeleteSetAllocationSink = mergeDeleteSets(sets)
	})
	// The old copystructure path used about 9,750 allocations for this shape.
	// Directly copying the two scalar fields stays below 800 on the measured
	// runtime; retain generous headroom while refusing reflective per-range work.
	if allocs > 2000 {
		t.Fatalf("MergeDeleteSets allocations=%.0f, want <=2000 for %d distinct clients", allocs, clients)
	}
}

// ---------------------------------------------------------------- from merge_dos_review_test.go
// merge_dos_review_test.go reproduces the two readClientsStructRefs DoS bugs
// found in the full code-review of the Go Yjs v2 codec (PR antst/y-crdt#2):
//
//   - BUG 1 (OOM): the INNER per-struct loop appends a struct each iteration up
//     to an attacker-controlled numberOfStructs (2^64), and the stall guard does
//     NOT fire because a V2 GC struct decodes from RLE columns that consume zero
//     rest bytes while the clock advances. A ~23-byte update grows the heap to GBs.
//   - BUG 2 (CPU): the OUTER per-client loop reads its counts via the
//     error-SWALLOWING ReadVarUint, so an exhausted buffer returns 0 and the loop
//     spins up to 2^64 times. A 10-byte V1 payload hangs ApplyUpdate.
//
// Each test FAILS on the unpatched tree (hang / unbounded heap) and PASSES after
// the fix returns QUICKLY with an error and a bounded heap.

// runWithin runs fn in a goroutine and fails the test if it does not return
// within d. It is the watchdog for the CPU/OOM-DoS repros across this review:
// an unbounded loop never returns, so the timeout firing IS the bug. (Shared by
// the awareness DoS repro too.)
func runWithin(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: did not return within %s (DoS: unbounded loop / allocation)", name, d)
	}
}

// itoa is a tiny base-10 int->string (keeps strconv out of this test's imports);
// used to mint distinct, non-merging map keys for the legitimate-update test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --- BUG 1: readClientsStructRefs inner per-struct loop OOM ---------------------
//
// buildV2OOMUpdate builds the exact attack shape:
//
//	feature flag = 0
//	keyClock col = empty
//	client col   = UintOptRle[0]          (single client id 0)
//	leftClock col= empty
//	rightClock col=empty
//	info col     = Rle[0x00]              (single GC info byte -> repeat forever)
//	str col      = empty
//	parentInfo col=empty
//	typeRef col  = empty
//	len col      = UintOptRle run: value=1, count=numberOfStructs (length=1 forever)
//	rest         = VarUint(numOfStateUpdates=1) VarUint(numberOfStructs) VarUint(clock=0)
//
// The info RLE column has a single byte, so after one Read the RleDecoder sets
// count = -1 ("repeat forever") and yields 0x00 (GC) without consuming. The len
// UintOptRle column is one run, so it yields 1 for numberOfStructs reads without
// consuming. Each iteration thus advances the clock (length=1>0) but consumes no
// bytes, so the stall guard's progressed() stays true while Refs grows.
func buildV2OOMUpdate(numberOfStructs uint64) []byte {
	col := func(payload []byte) []byte {
		return append(uvarintBytes(uint64(len(payload))), payload...)
	}

	clientCol := new(bytes.Buffer)
	writeVarIntMag(clientCol, 0, false) // single client id 0

	infoCol := []byte{0x00} // one GC info byte -> repeat forever after first read

	lenCol := new(bytes.Buffer)
	writeVarIntMag(lenCol, 1, true)         // negated value 1 => a run follows
	writeVarUint(lenCol, numberOfStructs-2) // count-2 => length=1 for numberOfStructs reads

	rest := new(bytes.Buffer)
	writeVarUint(rest, 1)               // numOfStateUpdates
	writeVarUint(rest, numberOfStructs) // numberOfStructs
	writeVarUint(rest, 0)               // clock

	out := new(bytes.Buffer)
	writeVarUint(out, 0)              // feature flag
	out.Write(col(nil))               // keyClock (empty)
	out.Write(col(clientCol.Bytes())) // client
	out.Write(col(nil))               // leftClock
	out.Write(col(nil))               // rightClock
	out.Write(col(infoCol))           // info
	out.Write(col(nil))               // str
	out.Write(col(nil))               // parentInfo
	out.Write(col(nil))               // typeRef
	out.Write(col(lenCol.Bytes()))    // len
	out.Write(rest.Bytes())           // rest
	return out.Bytes()
}

func TestReadClientsStructRefsBoundsStructCountOOM(t *testing.T) {
	const numberOfStructs = 60_000_000
	update := buildV2OOMUpdate(numberOfStructs)
	t.Logf("BUG1 update length = %d bytes, encoded numberOfStructs = %d", len(update), numberOfStructs)

	var heapBefore, heapAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&heapBefore)

	// Decode directly so we can both watchdog the time AND isolate the decoder's
	// allocation bound; ApplyUpdateV2 now returns the same error to its caller.
	runWithin(t, 5*time.Second, "BUG1 readClientsStructRefs", func() {
		doc := newDoc("", false, nil, nil, false)
		dec := newUpdateDecoderV2(update)
		_, err := readClientsStructRefs(dec, doc)
		if err == nil {
			t.Errorf("BUG1: expected an error from a 60M-struct / 23-byte update, got nil")
		}
	})

	runtime.ReadMemStats(&heapAfter)
	grew := int64(heapAfter.HeapAlloc) - int64(heapBefore.HeapAlloc)
	t.Logf("BUG1 heap delta during decode = %d bytes", grew)
	// 60M IAbstractStruct values + their GC backing would be hundreds of MB to
	// >1 GB. A bounded decode allocates a tiny fraction of that.
	const maxAllowedGrowth = 64 << 20 // 64 MiB
	if grew > maxAllowedGrowth {
		t.Errorf("BUG1: heap grew %d bytes (> %d) — struct count is not bounded", grew, maxAllowedGrowth)
	}
}

// A legitimately large update with MANY genuinely-distinct structs must STILL
// decode after the cap is added — the cap bounds amplification, it must not
// false-reject real input. A Y.Map with N distinct keys yields N non-mergeable
// items (each carries a different parentSub), so the decoded struct count is a
// genuine N. We assert it decodes, round-trips, and log the real structs/byte
// ratio as evidence the cap (K x len) has ample headroom over reality.
func TestReadClientsStructRefsAcceptsLargeLegitimateUpdate(t *testing.T) {
	src := newDoc("", false, nil, nil, false)
	m := src.GetMap("m")
	const n = 50000
	Transact(src, func(trans *Transaction) {
		for i := 0; i < n; i++ {
			m.Set(itoa(i), i)
		}
	}, nil, false)

	updateV2, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encode V2 failed: %v", err)
	}

	dst := newDoc("", false, nil, nil, false)
	dec := newUpdateDecoderV2(updateV2)
	refs, err := readClientsStructRefs(dec, dst)
	if err != nil {
		t.Fatalf("BUG1 cap false-rejected a legitimate %d-key map update (%d bytes): %v", n, len(updateV2), err)
	}
	total := 0
	for _, r := range refs {
		total += len(r.refs)
	}
	ratio := float64(total) / float64(len(updateV2))
	t.Logf("legitimate update: decoded %d structs from %d bytes (%.3f structs/byte)", total, len(updateV2), ratio)
	if total < n {
		t.Fatalf("expected at least %d decoded structs, got %d", n, total)
	}

	// And the full apply round-trips to the same map contents. (Map size is
	// observed via ToJSON; AbstractType.Length is not maintained for maps.)
	rt := newDoc("", false, nil, nil, false)
	_ = ApplyUpdateV2(rt, updateV2, nil)
	// ToJSON() returns the insertion-ordered Object type (previously a
	// map[string]any alias). Assert against it via the Object API.
	got, ok := rt.GetMap("m").ToJSON().(Object)
	if !ok {
		t.Fatalf("round-trip map ToJSON not an Object: %T", rt.GetMap("m").ToJSON())
	}
	if got.Len() != n {
		t.Fatalf("round-trip map size = %d, want %d", got.Len(), n)
	}
}

// --- BUG 2: readClientsStructRefs OUTER per-client loop spins -------------------
//
// numOfStateUpdates and the per-iteration numberOfStructs/client/clock are read
// via the error-SWALLOWING ReadVarUint, so on an exhausted buffer they return 0
// and the outer loop spins up to 2^64 times. A 10-byte V1 payload of all-1s
// VarUint (`ff ff ff ff ff ff ff ff ff 01`) hangs ApplyUpdate.
func TestReadClientsStructRefsOuterLoopBounded(t *testing.T) {
	payload := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	t.Logf("BUG2 payload = % x (%d bytes)", payload, len(payload))

	// The point is that ApplyUpdate RETURNS quickly instead of spinning 2^64
	// iterations; the returned error is covered separately.
	runWithin(t, 5*time.Second, "BUG2 ApplyUpdate", func() {
		doc := newDoc("", false, nil, nil, false)
		_ = ApplyUpdate(doc, payload, nil)
	})

	// And the decoder itself must surface the truncation error (not just return).
	runWithin(t, 5*time.Second, "BUG2 readClientsStructRefs(V1)", func() {
		doc := newDoc("", false, nil, nil, false)
		dec := newUpdateDecoderV1(payload)
		_, err := readClientsStructRefs(dec, doc)
		if err == nil {
			t.Errorf("BUG2: expected a truncation error from the exhausted outer header, got nil")
		}
	})
}

// ---------------------------------------------------------------- from merge_oom_ceiling_review_test.go
// merge_oom_ceiling_review_test.go is the proper close of BUG 1 (FINDING 4 from
// the full code-review of the Go Yjs v2 codec, PR antst/y-crdt#2). The prior cap
// `structCountCap = totalInput*512 + 512` was purely input-PROPORTIONAL with NO
// absolute ceiling: a large input still amplified (~512x input bytes -> a 1MB
// update could decode ~512M structs -> ~170GB). lib0's RleEncoder omits the final
// run's count (rle_codec.go), so a LEGITIMATE update can encode unboundedly many
// trailing uniform GC structs from O(1) bytes — indistinguishable from the attack
// except by numberOfStructs, which the format does not self-validate. A cap is
// therefore fundamental, but it must bound ABSOLUTE memory.
//
// These tests assert:
//   - the OOM attack heap stays BOUNDED (clamped to the absolute ceiling), NOT
//     proportional to the input, for input sizes 23B / 1KB / 100KB / 1MB;
//   - a large legitimate full-state-sync update and a regular-spaced-GC update
//     both still decode fully (no false-reject);
//   - the absolute ceiling clamps a giant numberOfStructs regardless of input.

// buildV2OOMUpdatePadded builds the same GC-repeat-forever OOM shape as
// buildV2OOMUpdate (merge_dos_review_test.go) but pads the UNUSED `str` column
// with `pad` filler bytes so the total update length can be driven to any target
// size. GC decode reads only the info + len columns, so the padding is never
// consumed per-struct — yet it counts toward decoder.RemainingLen() (the budget
// the proportional cap is measured against), exactly modelling a large-INPUT
// amplification attack.
func buildV2OOMUpdatePadded(numberOfStructs uint64, pad int) []byte {
	col := func(payload []byte) []byte {
		return append(uvarintBytes(uint64(len(payload))), payload...)
	}

	clientCol := new(bytes.Buffer)
	writeVarIntMag(clientCol, 0, false) // single client id 0

	infoCol := []byte{0x00} // one GC info byte -> repeat forever after first read

	lenCol := new(bytes.Buffer)
	writeVarIntMag(lenCol, 1, true)         // negated value 1 => a run follows
	writeVarUint(lenCol, numberOfStructs-2) // count-2 => length=1 for numberOfStructs reads

	strCol := make([]byte, pad) // filler; GC decode never reads the str column

	rest := new(bytes.Buffer)
	writeVarUint(rest, 1)               // numOfStateUpdates
	writeVarUint(rest, numberOfStructs) // numberOfStructs
	writeVarUint(rest, 0)               // clock

	out := new(bytes.Buffer)
	writeVarUint(out, 0)              // feature flag
	out.Write(col(nil))               // keyClock (empty)
	out.Write(col(clientCol.Bytes())) // client
	out.Write(col(nil))               // leftClock
	out.Write(col(nil))               // rightClock
	out.Write(col(infoCol))           // info
	out.Write(col(strCol))            // str (padded)
	out.Write(col(nil))               // parentInfo
	out.Write(col(nil))               // typeRef
	out.Write(col(lenCol.Bytes()))    // len
	out.Write(rest.Bytes())           // rest
	return out.Bytes()
}

// decodeHeapDelta decodes update via readClientsStructRefs and returns the heap
// growth during the decode and whether it errored. The decode is watchdogged so a
// regression to an unbounded loop fails loudly instead of hanging.
func decodeHeapDelta(t *testing.T, update []byte) (grew int64, err error) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	runWithin(t, 20*time.Second, "decodeHeapDelta", func() {
		doc := newDoc("", false, nil, nil, false)
		dec := newUpdateDecoderV2(update)
		_, err = readClientsStructRefs(dec, doc)
	})
	runtime.ReadMemStats(&after)
	grew = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	return
}

// FINDING 4: the OOM attack must be bounded to the ceiling — heap stays bounded
// (NOT proportional to the input) across input sizes spanning ~5 orders of
// magnitude, and the decode rejects the corrupt struct count.
func TestOOMAttackHeapBoundedAcrossInputSizes(t *testing.T) {
	// numberOfStructs is the attacker's claim: enormous, far above any ceiling.
	const numberOfStructs = 200_000_000

	// The hard memory bound: the heap delta must never exceed the absolute
	// ceiling's worth of structs (~340 B/struct measured) plus slack — and must
	// NOT scale with the input. ceilingBytes is generous slack above
	// structCountAbsoluteCeiling * perStruct.
	const perStructBytes = 400
	ceilingHeap := int64(structCountAbsoluteCeiling)*perStructBytes + (256 << 20) // + 256 MiB slack

	sizes := []struct {
		name string
		pad  int
	}{
		{"~23B", 0},
		{"~1KB", 1 << 10},
		{"~100KB", 100 << 10},
		{"~1MB", 1 << 20},
	}

	deltas := make([]int64, 0, len(sizes))
	for _, s := range sizes {
		update := buildV2OOMUpdatePadded(numberOfStructs, s.pad)
		grew, err := decodeHeapDelta(t, update)
		t.Logf("OOM %-7s: input=%d bytes, heapDelta=%d bytes, err=%v", s.name, len(update), grew, err != nil)
		if err == nil {
			t.Errorf("OOM %s: expected a cap error from a %d-struct claim, got nil", s.name, numberOfStructs)
		}
		if grew > ceilingHeap {
			t.Errorf("OOM %s: heap grew %d bytes (> ceiling bound %d) — not bounded by the absolute ceiling", s.name, grew, ceilingHeap)
		}
		deltas = append(deltas, grew)
	}

	// NOT-proportional-to-input check. The OLD cap was purely proportional
	// (structCountCap = bytes*512 + 512) with no ceiling, so the 1MB decode would
	// have materialized ~512M structs (~70-170 GB) — heap scaling linearly with
	// input without bound. The absolute ceiling clamps every input to at most A
	// structs, so the heap is bounded by a CONSTANT regardless of input size. Each
	// per-size assertion above already bounds the heap to ceilingHeap; here we
	// additionally assert the largest input's heap is nowhere near the old
	// proportional projection — i.e. the ceiling, not the input length, governs it.
	maxD := deltas[0]
	for _, d := range deltas {
		if d > maxD {
			maxD = d
		}
	}
	// Old proportional-without-ceiling projection for the 1MB input: bytes*512
	// structs. Even at a conservative ~100 B/struct that is many tens of GB; the
	// ceiling-bounded heap must be a small fraction of it.
	oldProportionalProjection := int64(sizes[len(sizes)-1].pad) * maxStructsPerInputByteOld * 100
	if maxD >= oldProportionalProjection {
		t.Errorf("OOM heap %d is not bounded below the old proportional projection %d — ceiling not governing", maxD, oldProportionalProjection)
	}
	t.Logf("max OOM heap delta=%d bytes, bounded well below old proportional projection=%d bytes", maxD, oldProportionalProjection)
}

// maxStructsPerInputByteOld is the slope of the pre-fix purely-proportional cap,
// used only to express the regression bound in TestOOMAttackHeapBoundedAcrossInputSizes.
const maxStructsPerInputByteOld = 512

// FINDING 4: a corrupt struct count is rejected and bounded even with a HUGE
// claimed numberOfStructs on a tiny input (the canonical 23-byte OOM), returning
// quickly with an error.
func TestOOMAttackAbsoluteCeilingClampsGiantCount(t *testing.T) {
	// 2^40 structs claimed from a tiny input.
	update := buildV2OOMUpdatePadded(1<<40, 0)
	grew, err := decodeHeapDelta(t, update)
	t.Logf("giant-count OOM: input=%d bytes, heapDelta=%d, err=%v", len(update), grew, err != nil)
	if err == nil {
		t.Fatalf("expected a cap error for a 2^40-struct claim, got nil")
	}
	const perStructBytes = 400
	ceilingHeap := int64(structCountAbsoluteCeiling)*perStructBytes + (256 << 20)
	if grew > ceilingHeap {
		t.Fatalf("giant-count OOM: heap grew %d bytes (> %d) — absolute ceiling not enforced", grew, ceilingHeap)
	}
}

// FINDING 4 (no false-reject): a regular-spaced-GC document — the FINDING's
// gc(c,0,1),gc(c,5,1),gc(c,10,1)... shape, here a Text with every other char
// deleted under GC — must decode fully. These GC structs are spaced by live
// content, so the update is NOT O(1) bytes, but it exercises the same
// RLE-uniform-column path the attack abuses.
func TestRegularSpacedGCUpdateDecodesFully(t *testing.T) {
	src := newDoc("", true, defaultGCFilter, nil, false) // gc=true
	const n = 40000
	txt := src.GetText("t")
	Transact(src, func(trans *Transaction) {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + i%26)
		}
		txt.Insert(0, string(b), newObject())
	}, nil, false)
	Transact(src, func(trans *Transaction) {
		for i := n - 2; i >= 0; i -= 2 {
			txt.Delete(i, 1) // delete every other char -> regular-spaced GCs
		}
	}, nil, false)

	update, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	dst := newDoc("", false, nil, nil, false)
	dec := newUpdateDecoderV2(update)
	refs, err := readClientsStructRefs(dec, dst)
	if err != nil {
		t.Fatalf("FINDING 4: regular-spaced-GC update (%d bytes) was false-rejected: %v", len(update), err)
	}
	total := 0
	for _, r := range refs {
		total += len(r.refs)
	}
	t.Logf("regular-spaced GC: decoded %d structs from %d bytes (%.3f structs/byte)", total, len(update), float64(total)/float64(len(update)))
	if total < n/2 {
		t.Fatalf("expected >= %d structs (every-other GC + live), got %d", n/2, total)
	}

	// And it round-trips through a full apply.
	rt := newDoc("", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(rt, update, nil)
	if got := rt.GetText("t").ToString(); len(got) != n/2 {
		t.Fatalf("round-trip text length = %d, want %d", len(got), n/2)
	}
}

// FINDING 4 (no false-reject): a large legitimate full-state-sync update — a
// 50k-key map (50k genuinely-distinct, non-mergeable structs) — must decode
// fully. This is the realistic "big document" case the cap must never reject.
func TestLargeFullStateSyncDecodesFully(t *testing.T) {
	src := newDoc("", false, nil, nil, false)
	m := src.GetMap("m")
	const n = 50000
	Transact(src, func(trans *Transaction) {
		for i := 0; i < n; i++ {
			m.Set(itoa(i), i)
		}
	}, nil, false)

	update, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dst := newDoc("", false, nil, nil, false)
	dec := newUpdateDecoderV2(update)
	refs, err := readClientsStructRefs(dec, dst)
	if err != nil {
		t.Fatalf("FINDING 4: large full-state-sync (%d keys, %d bytes) false-rejected: %v", n, len(update), err)
	}
	total := 0
	for _, r := range refs {
		total += len(r.refs)
	}
	t.Logf("full-state-sync: decoded %d structs from %d bytes (%.3f structs/byte)", total, len(update), float64(total)/float64(len(update)))
	if total < n {
		t.Fatalf("expected >= %d structs, got %d", n, total)
	}
}

// ---------------------------------------------------------------- from merge_updates_scheduler_bench_test.go
var mergeUpdatesSchedulerBenchmarkSink []uint8

func BenchmarkMergeUpdatesReaderCount(b *testing.B) {
	benchReleaseSinks(b)
	for _, readers := range []int{16, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("readers-%d", readers), func(b *testing.B) {
			updates := make([][]uint8, readers)
			for i := range updates {
				doc := newDoc(fmt.Sprintf("merge-reader-%d", i), false, nil, nil, false, WithClientID(i+1))
				doc.GetText("t").Insert(0, "x", Object{})
				var err error
				updates[i], err = EncodeStateAsUpdate(doc, nil)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				mergeUpdatesSchedulerBenchmarkSink, err = mergeUpdatesWith(updates, newDecoderV1, newEncoderV1)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- from merge_updates_scheduler_test.go
func schedulerTestReader(item abstractStruct) *lazyStructReader {
	return &lazyStructReader{curr: item}
}

func TestLazyStructReaderHeapUsesReferencePriorityAndStableTies(t *testing.T) {
	readers := []*lazyStructReader{
		schedulerTestReader(newGC(GenID(2, 5), 1)),
		schedulerTestReader(newGC(GenID(3, 9), 1)),
		schedulerTestReader(newGC(GenID(3, 1), 1)),
		schedulerTestReader(newSkip(GenID(3, 1), 1)),
		schedulerTestReader(newGC(GenID(3, 1), 1)),
		schedulerTestReader(newGC(GenID(1, 0), 1)),
	}
	labels := map[*lazyStructReader]string{
		readers[0]: "client-2",
		readers[1]: "client-3-clock-9",
		readers[2]: "client-3-clock-1-first",
		readers[3]: "client-3-clock-1-skip",
		readers[4]: "client-3-clock-1-second",
		readers[5]: "client-1",
	}
	entries := make([]lazyStructReaderHeapEntry, len(readers))
	scheduler := make(lazyStructReaderHeap, len(readers))
	for i, reader := range readers {
		entries[i] = lazyStructReaderHeapEntry{reader: reader, tie: int64(i)}
		scheduler[i] = &entries[i]
	}
	heap.Init(&scheduler)

	var got []string
	for scheduler.Len() > 0 {
		got = append(got, labels[heap.Pop(&scheduler).(*lazyStructReaderHeapEntry).reader])
	}
	want := []string{
		"client-3-clock-1-first",
		"client-3-clock-1-second",
		"client-3-clock-1-skip",
		"client-3-clock-9",
		"client-2",
		"client-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduler order=%v, want %v", got, want)
	}
}

func TestLazyStructReaderHeapRequeueStaysAheadAtEqualPosition(t *testing.T) {
	advanced := schedulerTestReader(newGC(GenID(1, 0), 1))
	waiting := schedulerTestReader(newGC(GenID(1, 1), 1))
	entries := []lazyStructReaderHeapEntry{
		{reader: advanced, tie: 0},
		{reader: waiting, tie: 1},
	}
	scheduler := lazyStructReaderHeap{&entries[0], &entries[1]}
	heap.Init(&scheduler)

	entry := heap.Pop(&scheduler).(*lazyStructReaderHeapEntry)
	if entry.reader != advanced {
		t.Fatal("fixture did not select the earlier clock")
	}
	advanced.curr = newGC(GenID(1, 1), 1)
	entry.tie = -1 // MergeUpdates assigns the next decreasing requeue tie.
	heap.Push(&scheduler, entry)

	if got := heap.Pop(&scheduler).(*lazyStructReaderHeapEntry).reader; got != advanced {
		t.Fatal("advanced reader lost stable precedence when it caught an equal position")
	}
}

func TestMergeUpdatesSchedulerRequeuesAcrossClientBlocks(t *testing.T) {
	makeUpdate := func(client Number, value string) []uint8 {
		doc := newDoc("scheduler-source", false, nil, nil, false, WithClientID(client))
		doc.GetText("t").Insert(0, value, Object{})
		update, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		return update
	}

	update3 := makeUpdate(3, "3")
	update2 := makeUpdate(2, "2")
	update1 := makeUpdate(1, "1")
	carrier := newDoc("scheduler-carrier", false, nil, nil, false, WithClientID(99))
	if err := ApplyUpdate(carrier, update3, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdate(carrier, update1, "fixture"); err != nil {
		t.Fatal(err)
	}
	combined31, err := EncodeStateAsUpdate(carrier, nil)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := mergeUpdatesWith([][]uint8{combined31, update2}, newDecoderV1, newEncoderV1)
	if err != nil {
		t.Fatal(err)
	}
	reader := newLazyStructReader(newDecoderV1(merged), false)
	var clients []Number
	var previous Number
	for current := reader.curr; current != nil; current = reader.nextStruct() {
		client := current.getID().Client
		if len(clients) == 0 || client != previous {
			clients = append(clients, client)
			previous = client
		}
	}
	if err := reader.decodeError(); err != nil {
		t.Fatal(err)
	}
	if want := []Number{3, 2, 1}; !reflect.DeepEqual(clients, want) {
		t.Fatalf("merged client blocks=%v, want %v", clients, want)
	}

	wantDoc := newDoc("scheduler-want", false, nil, nil, false, WithClientID(100))
	if err := ApplyUpdate(wantDoc, combined31, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdate(wantDoc, update2, "fixture"); err != nil {
		t.Fatal(err)
	}
	gotDoc := newDoc("scheduler-got", false, nil, nil, false, WithClientID(101))
	if err := ApplyUpdate(gotDoc, merged, "fixture"); err != nil {
		t.Fatal(err)
	}
	if got, want := gotDoc.GetText("t").ToString(), wantDoc.GetText("t").ToString(); got != want {
		t.Fatalf("merged text=%q, want sequential-apply text %q", got, want)
	}
}

func TestMergeUpdatesSchedulerKeepsEveryReader(t *testing.T) {
	const readerCount = 257
	updates := make([][]uint8, readerCount)
	for i := range updates {
		client := i + 1
		doc := newDoc("scheduler-pointer-source", false, nil, nil, false, WithClientID(client))
		doc.GetText("t").Insert(0, "x", Object{})
		var err error
		updates[i], err = EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	merged, err := mergeUpdatesWith(updates, newDecoderV1, newEncoderV1)
	if err != nil {
		t.Fatal(err)
	}
	reader := newLazyStructReader(newDecoderV1(merged), false)
	seen := 0
	for current := reader.curr; current != nil; current = reader.nextStruct() {
		wantClient := readerCount - seen
		if got := current.getID().Client; got != wantClient {
			t.Fatalf("struct %d client=%d, want descending client %d", seen, got, wantClient)
		}
		seen++
	}
	if err := reader.decodeError(); err != nil {
		t.Fatal(err)
	}
	if seen != readerCount {
		t.Fatalf("decoded %d merged structs, want one from each of %d readers", seen, readerCount)
	}
}

var mergeUpdatesSchedulerAllocationSink []uint8

func TestMergeUpdatesSchedulerAvoidsPerReaderEntryAllocations(t *testing.T) {
	const readerCount = 256
	updates := make([][]uint8, readerCount)
	for i := range updates {
		client := i + 1
		doc := newDoc("scheduler-allocation-source", false, nil, nil, false, WithClientID(client))
		doc.GetText("t").Insert(0, "x", Object{})
		var err error
		updates[i], err = EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(updates) != readerCount || len(updates[readerCount-1]) == 0 {
		t.Fatal("allocation fixture did not create one non-empty update per reader")
	}

	var mergeErr error
	allocs := testing.AllocsPerRun(10, func() {
		mergeUpdatesSchedulerAllocationSink, mergeErr = mergeUpdatesWith(updates, newDecoderV1, newEncoderV1)
	})
	if mergeErr != nil {
		t.Fatal(mergeErr)
	}
	// The stream cursor measures about 3,870 allocations for this whole
	// decode+merge shape. Replacing it with the former captured decoder closure
	// raises that to about 5,660 (roughly seven extra allocations per reader), so
	// retain headroom while refusing either that regression or per-reader heap
	// entries in the scheduler.
	if allocs > 4100 {
		t.Fatalf("MergeUpdates allocations=%.0f, want <=4100 for %d readers", allocs, readerCount)
	}
}
