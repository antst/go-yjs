package crdt

// h_accum_overflow_review_test.go — reviewer's OWN reproduction attempt for the
// 7th-review angle-H finding: the per-read clamp (toNumber/readVarUintAsNumber)
// bounds each decoded length to [0, MaxInt], but the running `clock += length`
// accumulator in the struct readers is UNGUARDED. Two GC structs each of length
// ~2^62 individually pass the clamp, but their sum wraps `clock` NEGATIVE, and a
// following item at that negative clock re-opens the per-block-clock crash via a
// different boundary. Single-field fuzz injection cannot synthesize this (it needs
// TWO fields at 2^62). If this panics, the accumulation boundary is a real gap.

import (
	"encoding/binary"
	"testing"
)

func TestReviewH_AccumOverflow_ApplyUpdate(t *testing.T) {
	// Extract the item bytes from a valid single-char insert.
	src := newDoc("accsrc", false, nil, nil, false)
	src.GetText("t").Insert(0, "x", Object{})
	upd, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// [numClients=1][numStructs=1][client varint][clock=0][item bytes...][DS=0x00]
	if len(upd) < 5 || upd[0] != 0x01 || upd[1] != 0x01 {
		t.Fatalf("unexpected single-struct update shape: %x", upd)
	}
	clientVal, clientLen := binary.Uvarint(upd[2:])
	clockPos := 2 + clientLen
	if upd[clockPos] != 0x00 {
		t.Fatalf("expected block clock 0 at %d, got %x (%x)", clockPos, upd[clockPos], upd)
	}
	itemStart := clockPos + 1
	itemBytes := upd[itemStart : len(upd)-1] // strip the trailing empty DS (0x00)

	// 2^62 as a varuint: two of these summed = 2^63 -> wraps negative.
	var lenbuf [binary.MaxVarintLen64]byte
	ln := binary.PutUvarint(lenbuf[:], uint64(1)<<62)
	gc := func() []byte { return append([]byte{0x00}, lenbuf[:ln]...) } // info=GC(0x00) + length

	// Reassemble: 3 structs for the same client: GC(2^62), GC(2^62), then the item.
	// The reader does clock += length per GC: 0 -> 2^62 -> 2^63(neg); the item is
	// then created at GenID(client, negativeClock).
	var clientEnc [binary.MaxVarintLen64]byte
	cn := binary.PutUvarint(clientEnc[:], clientVal)
	poison := []byte{0x01, 0x03} // numClients=1, numStructs=3
	poison = append(poison, clientEnc[:cn]...)
	poison = append(poison, 0x00) // block clock 0
	poison = append(poison, gc()...)
	poison = append(poison, gc()...)
	poison = append(poison, itemBytes...)
	poison = append(poison, 0x00) // empty delete set

	dst := newDoc("accdst", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(dst, poison, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on clock-accumulation overflow (clock += length): %v", val)
	}

	// The `clock += length` accumulation must be guarded: two lengths each < 2^63
	// that SUM past 2^63 wrap `clock` negative, and that negative clock reaches the
	// decode output (ParseUpdateMeta / EncodeStateVectorFromUpdate) silently —
	// poisoning sync. After the accumulation guard, this must ERROR, not return a
	// negative clock with err=nil.
	_, to, err := parseUpdateMeta(poison)
	if err == nil {
		for _, clk := range to {
			if clk < 0 {
				t.Fatalf("clock-accumulation overflow reached decode output as a NEGATIVE clock %d with err=nil (silent sync corruption; accumulation guard missing)", clk)
			}
		}
	}
}
