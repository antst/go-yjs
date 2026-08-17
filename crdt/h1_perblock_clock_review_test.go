package crdt

// h1_perblock_clock_review_test.go — reviewer's OWN independent reproduction
// attempt for the 6th-review angle-H claim: the per-block BASE CLOCK is read via
// a raw readVarUint+Number(uint64) at merge.go:302 (eager) / updates.go:1043
// (lazy), bypassing N1's toNumber. A clock in [2^63,2^64) wraps NEGATIVE; angle H
// claims it then drives a "missing" restStruct re-encode that splices a string
// with a negative slice bound -> SIGSEGV in ApplyUpdate. The angle-I fuzz (~81M
// execs) did NOT hit it. This test constructs angle H's exact scenario to settle
// the conflict. If it panics, H is right (a real crash I's fuzz missed).

import (
	"encoding/binary"
	"testing"
)

// poisonBlockClock builds a single-struct V1 update and rewrites its 1-byte
// base clock (=0) to the 10-byte 2^63 varuint (wraps negative as int64 Number).
func poisonBlockClock(t *testing.T) []byte {
	t.Helper()
	src := newDoc("h1src", false, nil, nil, false)
	src.GetText("t").Insert(0, "hello", Object{})
	upd, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// V1 client-refs header: [numClients][numberOfStructs][client varint][clock]...
	if len(upd) < 4 || upd[0] != 0x01 {
		t.Fatalf("unexpected update shape: %x", upd)
	}
	_, clientLen := binary.Uvarint(upd[2:])
	if clientLen <= 0 {
		t.Fatalf("bad client varint in %x", upd)
	}
	clockPos := 2 + clientLen
	if upd[clockPos] != 0x00 {
		t.Fatalf("expected base clock 0 at pos %d, got %#x (update %x)", clockPos, upd[clockPos], upd)
	}
	var huge [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(huge[:], uint64(1)<<63) // 2^63
	poison := make([]byte, 0, len(upd)+n)
	poison = append(poison, upd[:clockPos]...)
	poison = append(poison, huge[:n]...)
	poison = append(poison, upd[clockPos+1:]...)
	return poison
}

func TestReview6_PerBlockClock_ApplyUpdate(t *testing.T) {
	poison := poisonBlockClock(t)
	dst := newDoc("h1dst", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(dst, poison, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on negative per-block clock: %v", val)
	}
}

func TestReview6_PerBlockClock_ParseUpdateMeta(t *testing.T) {
	poison := poisonBlockClock(t)
	// Lazy path: angle H claims this returns negative-clock meta with NO error.
	from, to, err := parseUpdateMeta(poison)
	if err == nil {
		t.Fatalf("ParseUpdateMeta accepted a negative per-block clock: from=%v to=%v (silent corruption)", from, to)
	}
}
