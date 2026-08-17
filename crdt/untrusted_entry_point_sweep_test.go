package crdt

import (
	"fmt"
	"testing"
)

// Every public entry point that consumes bytes from a peer must answer with a
// value or an error, never by taking the process down.
//
// WHY THIS EXISTS, GIVEN THE DIFFERENTIAL GATE ALREADY RUNS MILLIONS OF SEEDS.
// The gate compares this library against yjs on VALID encodings: direction A
// replays what yjs produced, direction B re-encodes what this library built.
// Neither direction ever presents bytes that no correct encoder would emit, so
// malformed-input handling has no differential coverage at all — the oracle
// cannot diverge on a frame it never constructs.
//
// That gap is measurable rather than theoretical. A sweep of surviving original
// code found nine defects the gate had been green over, and two were exactly this
// shape: DecodeRelativePosition panicked on 16,000 of 65,792 one- and two-byte
// inputs, and the fixed-width scalar readers accepted truncated payloads and
// zero-padded them into plausible values. Both are reachable from the network
// with no malicious intent — a truncated frame is enough — and both survived
// 1.1M oracle seeds because the oracle only ever fed them well-formed bytes.
//
// So this is deliberately NOT a differential test. It asserts one property the
// reference cannot help with, because a panic is a Go-level failure with no yjs
// counterpart: whatever the bytes, the process survives and the caller is told.
func TestUntrustedEntryPointsNeverPanic(t *testing.T) {
	seeds := untrustedSeedCorpus(t)

	type entry struct {
		name string
		call func([]byte)
	}
	entries := []entry{
		{"decodeUpdate", func(b []byte) { _, _ = decodeUpdate(b) }},
		{"decodeUpdateV2", func(b []byte) { _, _ = decodeUpdateV2(b) }},
		{"decodeStateVector", func(b []byte) { _, _ = decodeStateVector(b) }},
		{"DecodeSnapshot", func(b []byte) { _, _ = DecodeSnapshot(b) }},
		{"DecodeSnapshotV2", func(b []byte) { _, _ = DecodeSnapshotV2(b) }},
		{"DecodeRelativePosition", func(b []byte) { _, _ = DecodeRelativePosition(b) }},
		{"EncodeStateVectorFromUpdate", func(b []byte) { _, _ = encodeStateVectorFromUpdate(b) }},
		{"EncodeStateVectorFromUpdateV2", func(b []byte) { _, _ = encodeStateVectorFromUpdateV2(b) }},
		{"DiffUpdate", func(b []byte) { _, _ = DiffUpdate(b, b) }},
		{"DiffUpdateV2", func(b []byte) { _, _ = DiffUpdateV2(b, b) }},
		{"MergeUpdates", func(b []byte) {
			_, _ = mergeUpdatesWith([][]uint8{b, b},
				func(x []byte) updateDecoder { return newUpdateDecoderV1(x) },
				func() updateEncoder { return newUpdateEncoderV1() })
		}},
		{"ApplyUpdate", func(b []byte) {
			doc := newDoc("sweep", false, defaultGCFilter, nil, false, WithClientID(9))
			_ = ApplyUpdate(doc, b, nil)
		}},
		{"ApplyUpdateV2", func(b []byte) {
			doc := newDoc("sweep", false, defaultGCFilter, nil, false, WithClientID(9))
			_ = ApplyUpdateV2(doc, b, nil)
		}},
		{"ApplyAwarenessUpdate", func(b []byte) {
			doc := newDoc("sweep", false, defaultGCFilter, nil, false, WithClientID(9))
			_ = ApplyAwarenessUpdate(NewAwareness(doc), b, nil)
		}},
	}

	cases := 0
	perEntry := map[string]int{}
	panics := map[string]string{}

	run := func(name string, call func([]byte), raw []byte) {
		defer func() {
			if r := recover(); r != nil {
				if _, seen := panics[name]; !seen {
					panics[name] = fmt.Sprintf("%x -> %v", raw, r)
				}
			}
		}()
		call(raw)
	}

	for _, e := range entries {
		for _, seed := range seeds {
			// Every truncation. A short frame is the commonest real-world corruption
			// (a closed socket, a partial read) and needs no adversary.
			for cut := 0; cut <= len(seed); cut++ {
				run(e.name, e.call, seed[:cut])
				cases++
				perEntry[e.name]++
			}
			// Single-byte substitutions, including the values most likely to be read
			// as a length or a tag. Whole-buffer mutation would be slower without
			// reaching a different class of failure.
			for pos := 0; pos < len(seed); pos++ {
				for _, v := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff} {
					if seed[pos] == v {
						continue
					}
					mutated := append([]byte(nil), seed...)
					mutated[pos] = v
					run(e.name, e.call, mutated)
					cases++
					perEntry[e.name]++
				}
			}
		}
	}

	if len(panics) > 0 {
		for name, first := range panics {
			t.Errorf("%s panicked on malformed input: %s", name, first)
		}
		t.Fatalf("%d entry points panic on bytes a peer can send", len(panics))
	}

	// A floor chosen to match whatever happened to run would not be a guard. Derive
	// it: every entry point must see every truncation of every seed plus the
	// substitutions, so anything materially below that means a seed went missing or
	// an entry point was skipped.
	want := 0
	for _, seed := range seeds {
		want += len(seed) + 1
		want += len(seed) * 4 // 5 substitution values, minus the ones equal to the original byte
	}
	for _, e := range entries {
		if got := perEntry[e.name]; got < want {
			t.Fatalf("%s ran %d cases, want at least %d; the sweep is not reaching it", e.name, got, want)
		}
	}
	t.Logf("UNTRUSTED SWEEP: %d malformed inputs across %d entry points, no panics", cases, len(entries))
}

// untrustedSeedCorpus returns short but structurally complete encodings, so that
// truncating and mutating them reaches real decoder branches rather than failing
// immediately on a header. Small on purpose: the sweep is quadratic in seed size
// and the interesting failures live near the framing, not deep in the payload.
func untrustedSeedCorpus(t *testing.T) [][]byte {
	t.Helper()
	doc := newDoc("seed", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "ab", Object{})
	arr := doc.GetArray("a")
	arr.Push(ArrayAny{1})
	m := doc.GetMap("m")
	m.Set("k", 1)
	txt.Delete(0, 1)

	v1, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	sv := encodeStateVectorWith(doc, getStateVector(doc.store), newUpdateEncoderV1())
	itemID := GenID(1, 0)
	rp := EncodeRelativePosition(&RelativePosition{Item: &itemID, Assoc: 0})
	snap, err := EncodeSnapshot(newSnapshot(newDeleteSetFromStructStore(doc.store), getStateVector(doc.store)))
	if err != nil {
		t.Fatal(err)
	}

	aw := NewAwareness(doc)
	_ = aw.SetLocalState(MakeObject("name", "a"))
	awUpdate := EncodeAwarenessUpdate(aw, []Number{doc.ClientID}, nil)

	seeds := [][]byte{v1, v2, sv, rp, snap, awUpdate}
	for i, s := range seeds {
		if len(s) == 0 {
			t.Fatalf("seed %d is empty", i)
		}
	}
	return seeds
}
