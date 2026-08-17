package crdt

// surgical_inject_review_test.go — reviewer's OWN position-agnostic surgical
// re-fuzz for the negative-wrap class. Random/coverage fuzzing has a blind spot
// for this class (it missed H#1 across 81M execs) because the crash needs a
// structurally-valid update with a SPECIFIC field rewritten to a 2^63 varuint.
// This sweep takes a valid update that contains BOTH structs and a delete set,
// and at EVERY byte position splices in the 10-byte 2^63 varuint — covering the
// block clock, skip length, DS clock, and DS client fields (and every other)
// without needing their offsets. Every public decode entry point must REJECT or
// clamp each variant cleanly; a panic at any position is a finding.

import (
	"encoding/binary"
	"testing"
)

func TestReview7_Surgical2p63Injection_NoPanic(t *testing.T) {
	// A doc with text content AND a delete (so the encoded update carries both a
	// struct section and a non-empty delete set).
	src := newDoc("surg", false, nil, nil, false)
	txt := src.GetText("t")
	txt.Insert(0, "abcdefghij", Object{})
	txt.Delete(2, 3) // delete "cde" -> produces a delete set
	upd, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	updV2, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encodeV2: %v", err)
	}
	sv, err := encodeStateVectorFromUpdate(upd)
	if err != nil {
		t.Fatalf("sv: %v", err)
	}

	var huge [binary.MaxVarintLen64]byte
	hn := binary.PutUvarint(huge[:], uint64(1)<<63) // 2^63

	splice := func(b []byte, pos int) []byte {
		out := make([]byte, 0, len(b)+hn)
		out = append(out, b[:pos]...)
		out = append(out, huge[:hn]...)
		out = append(out, b[pos+1:]...)
		return out
	}

	// entry points fed the V1-shaped poison
	v1targets := map[string]func([]byte){
		"ApplyUpdate":        func(p []byte) { _ = ApplyUpdate(newDoc("d", false, nil, nil, false), p, nil) },
		"ParseUpdateMeta":    func(p []byte) { _, _, _ = parseUpdateMeta(p) },
		"EncodeSVFromUpdate": func(p []byte) { _, _ = encodeStateVectorFromUpdate(p) },
		"DiffUpdate":         func(p []byte) { _, _ = DiffUpdate(p, sv) },
		"MergeUpdates":       func(p []byte) { _, _ = mergeUpdatesWith([][]uint8{p}, newDecoderV1, newEncoderV1) },
		"ConvertV1ToV2":      func(p []byte) { _, _ = ConvertUpdateFormatV1ToV2(p) },
		"ReadSyncMessage": func(p []byte) {
			readSyncMessageForTest(newDecoderV1(p), newEncoderV1(), newDoc("d", false, nil, nil, false), nil)
		},
		"decodeStateVector": func(p []byte) { _, _ = decodeStateVector(p) },
	}
	v2targets := map[string]func([]byte){
		"ApplyUpdateV2":     func(p []byte) { _ = ApplyUpdateV2(newDoc("d", false, nil, nil, false), p, nil) },
		"ConvertV2ToV1":     func(p []byte) { _, _ = ConvertUpdateFormatV2ToV1(p) },
		"EncodeSVFromUpdV2": func(p []byte) { _, _ = encodeStateVectorFromUpdateV2(p) },
	}

	sweep := func(base []byte, targets map[string]func([]byte)) (positions, calls int) {
		for pos := 0; pos < len(base); pos++ {
			p := splice(base, pos)
			positions++
			for name, fn := range targets {
				calls++
				if panicked, val := recovers(func() { fn(p) }); panicked {
					t.Errorf("PANIC: target=%s pos=%d (of %d) val=%v\n  poison=%x", name, pos, len(base), val, p)
				}
			}
		}
		return
	}

	p1, c1 := sweep(upd, v1targets)
	p2, c2 := sweep(updV2, v2targets)
	t.Logf("surgical 2^63 injection clean: V1 %d positions × %d targets (%d calls), V2 %d positions (%d calls), 0 panics",
		p1, len(v1targets), c1, p2, c2)
}
