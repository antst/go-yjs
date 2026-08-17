package crdt

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// Concurrency stress for process-global encoder state.
//
// WHY THIS SHAPE. The differential gate is a correctness oracle, but it is single-threaded: it
// builds a document, encodes it, compares bytes, and moves on. Nothing in it can observe state
// shared BETWEEN encodes. The V2 full-state encoder now keeps a package-level sync.Pool, so a
// buffer that is incompletely reset, or a returned slice that still aliases pooled memory, is
// invisible to every seed the gate will ever run — however many seeds that is.
//
// This is the difference between sampling the generators harder and widening what is checked.
// Raising the tier from full to ultimate multiplies seeds by ten and would not touch this at all.
//
// WHAT IS ASSERTED, and why each part earns its place:
//
//   1. Every goroutine encodes documents whose content it knows, and decodes its own bytes back,
//      asserting the round-trip matches. A pooled buffer carrying stale bytes from another
//      goroutine's document produces a mismatch here and nowhere else.
//   2. The bytes returned by an encode are retained and re-checked AFTER later encodes have run.
//      ToUint8Array currently allocates fresh and copies, so the returned slice does not alias the
//      pool — but that is a property of the current implementation, not a guarantee, and it is
//      exactly the property a future "avoid the final copy" optimization would break. This makes
//      that breakage a test failure instead of a corrupted wire message.
//   3. Documents differ in size and content kind across goroutines, so pooled buffers are handed
//      between workloads with different column shapes rather than being reused for identical work,
//      which is the reuse pattern most likely to hide an incomplete reset.
//
// Run under -race for the data-race dimension; the assertions above hold without it and catch
// logical corruption that -race cannot see.

func stressWorkers() int {
	if v := os.Getenv("POOL_STRESS_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU(); n > 4 {
		return n
	}
	return 4
}

func stressRounds() int {
	if v := os.Getenv("POOL_STRESS_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 200
}

// buildStressDoc makes a document whose shape depends on `kind`, so different workers drive
// different column encoders through the same pool.
func buildStressDoc(kind, seed int) (*Doc, string) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(Number(seed%64+1)))
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	n := 8 + seed%40
	switch kind % 4 {
	case 0: // text-heavy: exercises the string column
		for i := 0; i < n; i++ {
			txt.Insert(txt.Length(), fmt.Sprintf("s%d-", (seed+i)%97), Object{})
		}
	case 1: // map-heavy: exercises the key column and keyMap reset
		for i := 0; i < n; i++ {
			m.Set(fmt.Sprintf("k%d", (seed+i)%53), (seed*i)%911)
		}
	case 2: // array-heavy: exercises the len/typeRef columns
		for i := 0; i < n; i++ {
			arr.Insert(arr.GetLength(), ArrayAny{(seed + i) % 733})
		}
	default: // mixed, with deletions so the delete-set path is populated
		for i := 0; i < n; i++ {
			txt.Insert(txt.Length(), "x", Object{})
			arr.Insert(arr.GetLength(), ArrayAny{i})
			m.Set(fmt.Sprintf("k%d", i%7), i)
		}
		if txt.Length() > 3 {
			txt.Delete(1, 2)
		}
		if arr.GetLength() > 3 {
			arr.Delete(1, 2)
		}
	}
	return doc, stressShape(doc)
}

// stressShape uses fuzzCanon, the same canonical form the direction-B verifier builds, because it
// is ORDER-INDEPENDENT. marshalJSONOrdered is not: it preserves insertion order, and a Y.Map that
// round-trips through an encode legitimately comes back with its keys in a different order. The
// first version of this test used it and reported "reuse corrupted the encoding" on identical
// content whose keys had merely been reordered -- a false positive that would have sent someone
// hunting a pool bug that did not exist.
func stressShape(doc *Doc) string {
	shape := newObject()
	shape.Set("t", doc.GetText("t").ToString())
	shape.Set("a", doc.GetArray("a").ToJson())
	shape.Set("m", doc.GetMap("m").ToJson())
	cn, err := fuzzCanon(shape)
	if err != nil {
		return "ERR:" + err.Error()
	}
	return cn
}

func TestEncoderPoolConcurrentStress(t *testing.T) {
	workers, rounds := stressWorkers(), stressRounds()
	var wg sync.WaitGroup
	errs := make(chan string, workers*4)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Retained from an early round and re-verified at the end, so a later encode that
			// scribbled on pooled memory this slice still points into is caught.
			var firstBytes []byte
			var firstShape string

			for r := 0; r < rounds; r++ {
				seed := w*1000 + r
				doc, want := buildStressDoc(w+r, seed)

				enc, err := EncodeStateAsUpdateV2(doc, nil)
				if err != nil {
					errs <- fmt.Sprintf("worker %d round %d: encode: %v", w, r, err)
					return
				}
				// Copy immediately: the assertion below is about what the encoder RETURNED, and a
				// later round must not be able to change it retroactively.
				mine := append([]byte(nil), enc...)
				if !bytes.Equal(mine, enc) {
					errs <- fmt.Sprintf("worker %d round %d: copy mismatch", w, r)
					return
				}

				fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(999))
				_ = ApplyUpdateV2(fresh, enc, nil)
				if got := stressShape(fresh); got != want {
					errs <- fmt.Sprintf("worker %d round %d: round-trip differs\n want %s\n got  %s",
						w, r, want, got)
					return
				}

				if r == 1 {
					firstBytes, firstShape = enc, want // deliberately NOT copied
				}
			}

			// The retained slice must still decode to what it decoded to originally. If encoding
			// ever returns pooled memory, hundreds of later encodes will have overwritten it.
			if firstBytes != nil {
				fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(998))
				_ = ApplyUpdateV2(fresh, firstBytes, nil)
				if got := stressShape(fresh); got != firstShape {
					errs <- fmt.Sprintf("worker %d: RETAINED buffer was mutated by later encodes"+
						" — the encoder returned pooled memory\n want %s\n got  %s",
						w, firstShape, got)
				}
			}
		}(w)
	}

	wg.Wait()
	close(errs)
	var n int
	for e := range errs {
		n++
		if n <= 5 {
			t.Error(e)
		}
	}
	if n > 5 {
		t.Errorf("... and %d further failures", n-5)
	}
	t.Logf("POOL_STRESS workers=%d rounds=%d encodes=%d failures=%d",
		workers, rounds, workers*rounds, n)
}

// TestEncoderPoolSequentialReuse is the single-goroutine counterpart. Pool reuse is far more
// LIKELY here than under contention -- with one goroutine the same encoder comes back from the
// pool nearly every time -- so an incomplete reset shows up faster than in the concurrent test.
func TestEncoderPoolSequentialReuse(t *testing.T) {
	rounds := stressRounds() * 5
	for r := 0; r < rounds; r++ {
		doc, want := buildStressDoc(r, r)
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("round %d encode: %v", r, err)
		}
		fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(997))
		_ = ApplyUpdateV2(fresh, enc, nil)
		if got := stressShape(fresh); got != want {
			t.Fatalf("round %d: reuse corrupted the encoding\n want %s\n got  %s", r, want, got)
		}
	}
	t.Logf("POOL_SEQUENTIAL rounds=%d", rounds)
}
