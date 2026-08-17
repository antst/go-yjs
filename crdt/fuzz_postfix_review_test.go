package crdt

// fuzz_postfix_review_test.go — reviewer's OWN post-fix re-fuzz of every public
// decode entry point (the fix agent skipped the 9M-exec gate). Confirms the 3
// crash roots (K1 item.go:90 / K2 snapshot.go:175 / K3 merge.go:171) are CLOSED
// and that the fix introduced NO new crash and NO false-reject on valid input.
//
// Deterministic (fixed RNG seed). Seeds = valid corpora built from a live doc
// PLUS the four known minimal crash payloads, so the mutator explores the exact
// neighborhoods that crashed pre-fix. Every entry point is invoked under
// recover(); any panic fails the test with the target + hex payload.

import (
	"encoding/hex"
	"math/rand"
	"testing"
)

// runRecover invokes fn, reporting any panic (incl. SIGSEGV) without aborting.
func runRecover(name string, payload []byte) (crashedName string, crashedHex string) {
	defer func() {
		if r := recover(); r != nil {
			crashedName = name
			crashedHex = hex.EncodeToString(payload)
		}
	}()
	postfixTargets[name](payload)
	return "", ""
}

// postfixTargets is the set of public decode entry points, each fed raw bytes.
var postfixTargets map[string]func([]byte)

func buildPostfixSeeds(t *testing.T) (seeds [][]byte, validUpdV1, validSV, validSnap []byte) {
	// A live doc with text + array + map content → realistic non-empty corpora.
	// Build via srcSeedDoc() so the snapshot origin used by the restore targets
	// (which also call srcSeedDoc()) has the SAME store as the doc that produced
	// validSnap — otherwise CreateDocFromSnapshot reads structs from a mismatched
	// (text-only) origin and passes/fails for the wrong reason.
	src := srcSeedDoc()

	updV1, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("seed EncodeStateAsUpdate: %v", err)
	}
	updV2, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("seed EncodeStateAsUpdateV2: %v", err)
	}
	sv, err := encodeStateVectorFromUpdate(updV1)
	if err != nil {
		t.Fatalf("seed EncodeStateVectorFromUpdate: %v", err)
	}
	snap, err := EncodeSnapshot(NewSnapshotByDoc(src))
	if err != nil {
		t.Fatalf("seed EncodeSnapshot: %v", err)
	}
	aw := EncodeAwarenessUpdateFor(t, 7, 1, `{"cursor":1}`)

	seeds = [][]byte{updV1, updV2, sv, snap, aw}
	// The four known minimal crash payloads (closed by K1/K2/K3) as seeds, so the
	// mutator explores around the exact pre-fix crash sites.
	for _, h := range []string{
		"0102000000000100ffffffffffffffffffff00",     // K1 ApplyUpdate
		"02130102000000000100ffffffffffffffffffff00", // K1 ReadSyncMessage
		"00010101",                 // K2 snapshot absent client
		"010080808080808080808001", // K3 negative-clock SV
	} {
		if b, e := hex.DecodeString(h); e == nil {
			seeds = append(seeds, b)
		}
	}
	return seeds, updV1, sv, snap
}

func TestReview4_Refuzz_AllEntryPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fuzz sweep in -short")
	}
	seeds, validUpdV1, validSV, validSnap := buildPostfixSeeds(t)

	// Every public decode entry point, each tolerant of arbitrary bytes (must
	// error/no-op, never panic). Apply paths use a fresh doc per call (they mutate).
	postfixTargets = map[string]func([]byte){
		"ApplyUpdate":   func(b []byte) { _ = ApplyUpdate(newDoc("d", false, nil, nil, false), b, nil) },
		"ApplyUpdateV2": func(b []byte) { _ = ApplyUpdateV2(newDoc("d", false, nil, nil, false), b, nil) },
		"ReadSyncMessage": func(b []byte) {
			readSyncMessageForTest(newDecoderV1(b), newEncoderV1(), newDoc("d", false, nil, nil, false), nil)
		},
		"DiffUpdate":               func(b []byte) { _, _ = DiffUpdate(b, validSV) },
		"DiffUpdateV2":             func(b []byte) { _, _ = DiffUpdateV2(b, validSV) },
		"diffUpdates":              func(b []byte) { _, _ = diffUpdates(b, validSV, 0) },
		"DiffUpdate_hostileSV":     func(b []byte) { _, _ = DiffUpdate(validUpdV1, b) },
		"MergeUpdatesV1":           func(b []byte) { _, _ = mergeUpdatesWith([][]uint8{b, validUpdV1}, newDecoderV1, newEncoderV1) },
		"ConvertV1ToV2":            func(b []byte) { _, _ = ConvertUpdateFormatV1ToV2(b) },
		"ConvertV2ToV1":            func(b []byte) { _, _ = ConvertUpdateFormatV2ToV1(b) },
		"decodeStateVector":        func(b []byte) { _, _ = decodeStateVector(b) },
		"EncodeSVFromUpdate":       func(b []byte) { _, _ = encodeStateVectorFromUpdate(b) },
		"EncodeSVFromUpdateV2":     func(b []byte) { _, _ = encodeStateVectorFromUpdateV2(b) },
		"ParseUpdateMeta":          func(b []byte) { _, _, _ = parseUpdateMeta(b) },
		"EncodeStateAsUpdate_SV":   func(b []byte) { _, _ = EncodeStateAsUpdate(srcSeedDoc(), b) },
		"EncodeStateAsUpdateV2_SV": func(b []byte) { _, _ = EncodeStateAsUpdateV2(srcSeedDoc(), b) },
		"DecodeSnapshot": func(b []byte) {
			if s, e := DecodeSnapshot(b); e == nil {
				_, _ = CreateDocFromSnapshot(srcSeedDoc(), s, newDoc("new", false, nil, nil, false))
			}
		},
		"DecodeSnapshotV2": func(b []byte) {
			if s, e := DecodeSnapshotV2(b); e == nil {
				_, _ = CreateDocFromSnapshot(srcSeedDoc(), s, newDoc("new", false, nil, nil, false))
			}
		},
		// NewAwareness starts a presence-reaper goroutine (US5/1.7A); these fuzz
		// targets create a transient Awareness per input, so Destroy it (signals the
		// reaper to stop) or thousands of idle reapers accumulate and starve the
		// -race scheduler.
		"ApplyAwareness": func(b []byte) {
			aw := NewAwareness(newDoc("d", false, nil, nil, false))
			defer aw.Destroy()
			_ = ApplyAwarenessUpdate(aw, b, nil)
		},
		"VenusAwareness": func(b []byte) {
			aw := NewAwareness(newDoc("d", false, nil, nil, false))
			defer aw.Destroy()
			_ = applyAwarenessUpdateWithoutEvents(aw, b)
		},
		"ModifyAwareness": func(b []byte) { _, _ = ModifyAwarenessUpdate(b, func(v interface{}) interface{} { return v }) },
	}

	rng := rand.New(rand.NewSource(0xC0FFEE))
	const iterations = 300000
	crashes := map[string]string{}
	for it := 0; it < iterations; it++ {
		seed := seeds[rng.Intn(len(seeds))]
		payload := mutate(rng, seed)
		for name := range postfixTargets {
			if cn, ch := runRecover(name, payload); cn != "" {
				if _, seen := crashes[cn]; !seen {
					crashes[cn] = ch
					t.Errorf("PANIC in %s on payload %s", cn, ch)
				}
			}
		}
	}
	if len(crashes) == 0 {
		t.Logf("re-fuzz clean: %d iterations × %d targets, 0 panics", iterations, len(postfixTargets))
	}

	// False-reject guard: the valid corpora must still decode/apply cleanly.
	dst := newDoc("dst", false, nil, nil, false)
	if cn, _ := func() (string, string) {
		return runRecover2("apply-valid", func() { _ = ApplyUpdate(dst, validUpdV1, nil) })
	}(); cn != "" {
		t.Errorf("valid update panicked on apply: regression")
	}
	if dst.GetText("t").ToString() != "abcdefghij" {
		t.Errorf("valid update did not apply correctly (false-reject?): got %q", dst.GetText("t").ToString())
	}
	if _, err := decodeStateVector(validSV); err != nil {
		t.Errorf("valid SV rejected (false-reject): %v", err)
	}
	if s, err := DecodeSnapshot(validSnap); err != nil {
		t.Errorf("valid snapshot rejected (false-reject): %v", err)
	} else if _, err := CreateDocFromSnapshot(srcSeedDoc(), s, newDoc("n", false, nil, nil, false)); err != nil {
		t.Errorf("valid snapshot restore failed (false-reject): %v", err)
	}
}

// srcSeedDoc rebuilds the seed content doc (no shared mutable state). Single source of
// truth for the seed content: buildPostfixSeeds uses it too, so the snapshot origin
// passed to CreateDocFromSnapshot restore targets matches the doc that produced the
// snapshot (text + array + map), not a text-only subset.
func srcSeedDoc() *Doc {
	d := newDoc("seed", false, nil, nil, false)
	d.GetText("t").Insert(0, "abcdefghij", Object{})
	d.GetArray("arr").Push(ArrayAny{"a", "b", int64(3)})
	if m := d.GetMap("m"); m != nil {
		m.Set("k", "v")
	}
	return d
}

func runRecover2(name string, fn func()) (crashed string, _ string) {
	defer func() {
		if r := recover(); r != nil {
			crashed = name
		}
	}()
	fn()
	return "", ""
}

// mutate returns a mutated copy of seed (bit flips, truncation, splice, append).
func mutate(rng *rand.Rand, seed []byte) []byte {
	b := make([]byte, len(seed))
	copy(b, seed)
	switch rng.Intn(6) {
	case 0: // bit flips
		for n := rng.Intn(8) + 1; n > 0 && len(b) > 0; n-- {
			b[rng.Intn(len(b))] ^= byte(1 << rng.Intn(8))
		}
	case 1: // truncate
		if len(b) > 0 {
			b = b[:rng.Intn(len(b))]
		}
	case 2: // append random tail (drive lengths/varints high)
		tail := make([]byte, rng.Intn(12))
		for i := range tail {
			tail[i] = byte(rng.Intn(256))
		}
		b = append(b, tail...)
	case 3: // set a byte to 0xff (overflow varints)
		if len(b) > 0 {
			b[rng.Intn(len(b))] = 0xff
		}
	case 4: // zero a byte
		if len(b) > 0 {
			b[rng.Intn(len(b))] = 0
		}
	case 5: // splice insert
		if len(b) > 0 {
			pos := rng.Intn(len(b))
			b = append(b[:pos], append([]byte{byte(rng.Intn(256))}, b[pos:]...)...)
		}
	}
	return b
}
