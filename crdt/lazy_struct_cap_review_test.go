package crdt

import (
	"runtime"
	"testing"
	"time"
)

// lazy_struct_cap_review_test.go reproduces DoS 1 from the SECOND code-review of
// PR antst/y-crdt#2: the eager readClientsStructRefs has the cumulative
// struct-count cap (merge.go, structCountCap), but the LAZY
// the former lazy generator had NONE — every V2 lazy consumer
// (MergeUpdates with V2 decoders, DiffUpdateV2, ConvertUpdateFormatV2ToV1,
// EncodeStateVectorFromUpdateV2, ParseUpdateMeta, logUpdateV2) would
// materialize ~numberOfStructs GC structs from a ~23-byte payload because a V2
// GC struct decodes from RLE columns that consume zero rest bytes while the
// clock advances (so the stall guard's forward-progress check stays satisfied).
//
// The attack shape is identical to BUG1's buildV2OOMUpdate (info RLE = one GC
// byte -> repeat forever; len Opt-RLE = one run -> length 1 forever), and is
// reachable through the lazy path via MergeUpdates(..., NewDecoderV2, ...).
//
// Each test FAILS on the unpatched tree (the lazy reader runs for seconds and
// the heap balloons) and PASSES after the cap is threaded into the generator
// (quick return + a surfaced error, bounded heap). The shared stream now owns
// the same cap for both eager and lazy adapters.

// TestLazyStructReaderBoundsStructCountOOM drives the 60M-struct / ~23-byte V2
// payload through MergeUpdates' lazy path and asserts it returns quickly, with
// an error, and a bounded heap.
func TestLazyStructReaderBoundsStructCountOOM(t *testing.T) {
	const numberOfStructs = 60_000_000
	update := buildV2OOMUpdate(numberOfStructs)
	t.Logf("DoS1 lazy update length = %d bytes, encoded numberOfStructs = %d", len(update), numberOfStructs)

	var heapBefore, heapAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&heapBefore)

	// MergeUpdates always runs the lazy struct reader now (strict decoding is
	// always enforced — even a single update is validated). The merged output must
	// surface an error rather than silently producing a giant decoded struct stream.
	runWithin(t, 5*time.Second, "DoS1 MergeUpdates(V2 lazy)", func() {
		_, err := mergeUpdatesWith([][]uint8{update}, newDecoderV2, newEncoderV2)
		if err == nil {
			t.Errorf("DoS1: expected an error from a 60M-struct / %d-byte V2 lazy merge, got nil", len(update))
		}
	})

	runtime.ReadMemStats(&heapAfter)
	grew := int64(heapAfter.HeapAlloc) - int64(heapBefore.HeapAlloc)
	t.Logf("DoS1 lazy heap delta during merge = %d bytes", grew)
	const maxAllowedGrowth = 64 << 20 // 64 MiB
	if grew > maxAllowedGrowth {
		t.Errorf("DoS1: heap grew %d bytes (> %d) — lazy struct count is not bounded", grew, maxAllowedGrowth)
	}
}

// TestEncodeStateVectorFromUpdateV2BoundsStructCount exercises a second lazy
// entry point (EncodeStateVectorFromUpdateV2) on the same payload; it must also
// return quickly with a bounded heap (the call swallows the error, so the bound
// is observed via time + heap, not a returned error).
func TestEncodeStateVectorFromUpdateV2BoundsStructCount(t *testing.T) {
	const numberOfStructs = 60_000_000
	update := buildV2OOMUpdate(numberOfStructs)

	var heapBefore, heapAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&heapBefore)

	runWithin(t, 5*time.Second, "DoS1 EncodeStateVectorFromUpdateV2", func() {
		// The cap-breaching input must now surface an error (silent-truncation
		// fix) rather than return a partial/empty SV; either way it must be bounded.
		if _, err := encodeStateVectorFromUpdateV2(update); err != nil {
			t.Logf("DoS1 EncodeStateVectorFromUpdateV2 errored as expected: %v", err)
		}
	})

	runtime.ReadMemStats(&heapAfter)
	grew := int64(heapAfter.HeapAlloc) - int64(heapBefore.HeapAlloc)
	t.Logf("DoS1 EncodeStateVectorFromUpdateV2 heap delta = %d bytes", grew)
	const maxAllowedGrowth = 64 << 20
	if grew > maxAllowedGrowth {
		t.Errorf("DoS1: EncodeStateVectorFromUpdateV2 heap grew %d bytes (> %d)", grew, maxAllowedGrowth)
	}
}

// TestLazyStructReaderV1NotBroken guards the carve-out: V1 lazy is NOT affected
// by the attack (ReadInfo consumes >=1 byte per struct, so the stall guard /
// truncation already bounds it). The new cap must not false-reject a legitimate
// V1 update through the lazy merge path. A real two-client merge must still
// produce a valid, non-empty merged update.
func TestLazyStructReaderV1NotBroken(t *testing.T) {
	src := newDoc("", false, nil, nil, false)
	a := src.GetArray("a")
	Transact(src, func(trans *Transaction) {
		for i := 0; i < 200; i++ {
			a.Push([]any{i})
		}
	}, nil, false)
	u1, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode V1 failed: %v", err)
	}

	merged, err := mergeUpdatesWith([][]uint8{u1, u1}, newDecoderV1, newEncoderV1)
	if err != nil {
		t.Fatalf("DoS1 cap false-rejected a legitimate V1 lazy merge: %v", err)
	}
	if len(merged) == 0 {
		t.Fatalf("DoS1: legitimate V1 merge produced empty output")
	}

	// And it applies to a fresh doc reproducing the array.
	dst := newDoc("", false, nil, nil, false)
	_ = ApplyUpdate(dst, merged, nil)
	got := dst.GetArray("a")
	if got.GetLength() != 200 {
		t.Fatalf("DoS1: merged V1 update applied to length %d, want 200", got.GetLength())
	}
}

// TestLazyStructReaderAcceptsLargeLegitimateV2 ensures the lazy V2 cap does not
// false-reject a genuinely large (but real) V2 update: a 50k-distinct-key map
// merged through the lazy V2 path must still decode and round-trip.
func TestLazyStructReaderAcceptsLargeLegitimateV2(t *testing.T) {
	src := newDoc("", false, nil, nil, false)
	m := src.GetMap("m")
	const n = 50000
	Transact(src, func(trans *Transaction) {
		for i := 0; i < n; i++ {
			m.Set(itoa(i), i)
		}
	}, nil, false)
	u, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encode V2 failed: %v", err)
	}

	merged, err := mergeUpdatesWith([][]uint8{u, u}, newDecoderV2, newEncoderV2)
	if err != nil {
		t.Fatalf("DoS1 cap false-rejected a legitimate %d-key V2 lazy merge (%d bytes): %v", n, len(u), err)
	}
	if len(merged) == 0 {
		t.Fatalf("DoS1: legitimate large V2 merge produced empty output")
	}
}
