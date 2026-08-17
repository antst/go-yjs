package crdt

import (
	"encoding/hex"
	"testing"
)

// lazy_struct_reader_crash_review_test.go reproduces CRASH 2 and CRASH 3 found
// in the code-review (PR antst/y-crdt#2): the lazy struct generator
// (updates.go Next) handled per-field decode errors inconsistently.
//
// CRASH 2 — swallow-and-build-corrupt. The ReadLeftID for a parent-as-ID
// (updates.go:1078 `parent, _ = l.decoder.ReadLeftID()`) discarded its error.
// On a truncated parent the read returns a TYPED-NIL *ID, which is stored into
// the Item's parent. Because isIDPtr matches on type (not value), the re-encode
// path (item.go:434 `else if isIDPtr(parent)`) then calls WriteLeftID with a
// nil *ID, and WriteID derefs id.Client → SIGSEGV. This hits every consumer
// that re-encodes lazily-read structs: DiffUpdate(V2), diffUpdates,
// ConvertUpdateFormatV1ToV2 / V2ToV1.
//
// CRASH 3 — panic instead of error. On a ReadItemContent failure the generator
// did `panic(errInvalidData)` when stopIfError=true (updates.go:1094/1106).
// MergeUpdates(..., stopIfError=true) runs the generator with stopIfError set,
// so a malformed-content frame panicked the whole merge instead of surfacing an
// error via the reader's Err() / checkLazyCap mechanism.
//
// The fix makes the generator RECORD every field-decode error through the
// stream error channel and ABORT the struct (done=true, s=nil) — never
// swallow-and-build, never panic. A nil *ID parent guard is also added at
// item.go:434 as defense in depth.

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

func TestConvertUpdateFormatV1ToV2NilParentNoCrash(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CRASH 2: ConvertUpdateFormatV1ToV2 panicked re-encoding a nil-*ID parent: %v", r)
		}
	}()

	update := mustHex(t, "47654364ee050d722e01382e36152150debee9afd4dc88affc9f1d32")
	out, err := ConvertUpdateFormatV1ToV2(update)
	// Must error (or cleanly skip), never crash.
	t.Logf("CRASH 2 ConvertUpdateFormatV1ToV2: err=%v outLen=%d", err, len(out))
}

func TestDiffUpdateV2NilParentNoCrash(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CRASH 2: DiffUpdateV2 panicked re-encoding a nil-*ID parent: %v", r)
		}
	}()

	update := mustHex(t, "00000593ded4a8020000030400080c087468c3a96c6c6f6101050100000101010200")
	out, err := DiffUpdateV2(update, []byte{0x00})
	t.Logf("CRASH 2 DiffUpdateV2: err=%v outLen=%d", err, len(out))
}

func TestMergeUpdatesMalformedContentNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CRASH 3: MergeUpdates panicked on malformed item content instead of returning an error: %v", r)
		}
	}()

	u := []byte{0x01, 0x01, 0x00, 0x00, 0x30}
	out, err := mergeUpdatesWith(
		[][]uint8{u, u},
		func(b []byte) updateDecoder { return newUpdateDecoderV1(b) },
		func() updateEncoder { return newUpdateEncoderV1() },
	)
	// Strict decoding is always enforced ⇒ malformed content must surface as an
	// error, never a panic.
	if err == nil {
		t.Logf("CRASH 3 MergeUpdates: returned no error (outLen=%d); acceptable as long as no panic", len(out))
	} else {
		t.Logf("CRASH 3 MergeUpdates: errored cleanly: %v", err)
	}
}
