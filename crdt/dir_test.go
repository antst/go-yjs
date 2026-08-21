package crdt

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// ---------------------------------------------------------------- from dir_b_batched_test.go
// Batched-transaction correctness.
//
// WHY THIS EXISTS. Every differential cell drives this library one operation at a time: direction B
// calls txt.Insert / arr.Insert / m.Set directly, each in its own implicit transaction, and
// dir_b_diff_test.go contains no Transact call at all. The reference generator DOES batch (it wraps
// opsPer operations in one doc.transact), but that only covers direction A, where this library
// merely replays bytes and never runs the batched path itself.
//
// That left a real hole. Inside a single transaction nothing merges until cleanup, so a long run of
// same-client items stays unmerged and every subsequent operation traverses it. That state — which
// the per-operation shape never reaches, because each commit collapses the run — is where the
// search-marker walk in findMarker used to matter, where the compactState tail-append fast paths in
// typeListInsertGenericsAfter and YText.Insert fire, and where mergeTransactionClientStructs does
// its work at commit. All of it was being changed for performance while no differential test could
// observe it.
//
// WHAT IS ASSERTED. The same operation sequence, applied per-operation and applied inside one
// transaction, must produce the same document. Batching is an encoding and scheduling concern: it
// legitimately changes which items merge and therefore the exact bytes, so byte equality is NOT the
// invariant and asserting it would produce a false failure. Observable content is the invariant,
// and it must hold exactly.
//
// This runs without Node, so it can be enforced at full seed volume in the fast tier rather than
// only where the reference harness is available.

// dirBOps applies the direction-B operation sequence. Extracted from buildDirBDoc so the batched
// and per-operation builders provably run the SAME sequence — a copy would silently drift and the
// comparison would stop meaning anything.
func dirBOps(rng func() int, txt *YText, arr *YArray, m *YMap) {
	letters := "abcde"
	for i := 0; i < 12; i++ {
		switch rng() % 8 {
		case 0:
			idx := 0
			if txt.Length() > 0 {
				idx = rng() % (txt.Length() + 1)
			}
			txt.Insert(idx, string(letters[rng()%len(letters)]), Object{})
		case 1:
			if txt.Length() > 0 {
				txt.Delete(rng()%txt.Length(), 1)
			}
		case 2:
			idx := 0
			if arr.GetLength() > 0 {
				idx = rng() % (arr.GetLength() + 1)
			}
			arr.Insert(idx, ArrayAny{rng() % 100})
		case 3:
			if m != nil {
				m.Set(string(letters[rng()%len(letters)]), rng()%50)
			}
		case 6:
			// FORMAT BURST. The rich-text format path uses a bounded arena that only activates
			// above 8 format items in ONE transaction, so a single Format per step would never
			// reach it -- and the batched-vs-per-op comparison is the only differential that can
			// see arena behaviour at all, because the arena is per-transaction by construction.
			// Twelve crosses the threshold in the batched build while the per-op build stays on
			// the original path, which is exactly the divergence worth testing.
			if txt.Length() > 6 {
				for f := 0; f < 12; f++ {
					attr := newObject()
					switch f % 3 {
					case 0:
						attr.Set("bold", true)
					case 1:
						attr.Set("italic", f%2 == 0)
					default:
						attr.Set("bold", Null)
					}
					start := rng() % (txt.Length() - 3)
					txt.Format(start, 3, attr)
				}
			}
		case 7:
			// ApplyDelta with several attributed inserts: the same arena, reached through the
			// entry point rich-text bindings actually drive.
			delta := make([]EventOperator, 0, 10)
			for d := 0; d < 10; d++ {
				a := newObject()
				a.Set("bold", d%2 == 0)
				delta = append(delta, NewTextDeltaOp(string(letters[rng()%len(letters)]), a))
			}
			txt.ApplyDelta(delta, true)
		case 5:
			switch rng() % 3 {
			case 0:
				arr.Unshift(ArrayAny{rng() % 100})
			case 1:
				if arr.GetLength() > 1 {
					_ = arr.Splice(0, arr.GetLength()-1)
				}
			case 2:
				if m != nil && m.GetSize() > 0 {
					m.Clear()
				}
			}
		case 4:
			nested := NewYMap(nil)
			nested.Set("k0", rng()%10)
			nested.Set("k1", rng()%10)
			nested.Set("k2", rng()%10)
			idx := 0
			if arr.GetLength() > 0 {
				idx = rng() % (arr.GetLength() + 1)
			}
			arr.Insert(idx, ArrayAny{nested})
		}
	}
}

// dirBShape is the observable content of a direction-B document: the same three types the
// reference verifier canonicalises, so a divergence here is a divergence the oracle would report.
func dirBShape(t *testing.T, doc *Doc, label string, seed int) string {
	t.Helper()
	shape := newObject()
	txt := doc.GetText("t")
	shape.Set("t", txt.ToString())
	// ToDelta, not just ToString. ToString discards formatting entirely, so a comparison built on
	// it alone would GENERATE format operations and then be structurally unable to observe them --
	// the format burst above would have been decorative. Coverage is a property of what is
	// compared, not only of what is produced.
	shape.Set("d", deltaSemantic(txt.ToDelta(nil, nil, nil)))
	shape.Set("a", doc.GetArray("a").ToJSON())
	shape.Set("m", doc.GetMap("m").ToJSON())
	cn, err := fuzzCanon(shape)
	if err != nil {
		t.Fatalf("seed %d %s canon: %v", seed, label, err)
	}
	return cn
}

// buildDirBBatchedDoc runs the direction-B sequence inside ONE transaction. The document is
// deliberately observer-free, which is what puts the transaction on the compactState path — the
// same path the tail-append fast paths are gated on.
func buildDirBBatchedDoc(seed int) *Doc {
	rng := newDirBRand(seed)
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	Transact(doc, func(trans *Transaction) {
		dirBOps(rng, txt, arr, m)
	}, nil, true)
	return doc
}

// buildDirBPerOpDoc runs the identical sequence with one implicit transaction per operation.
func buildDirBPerOpDoc(seed int) *Doc {
	rng := newDirBRand(seed)
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	dirBOps(rng, txt, arr, m)
	return doc
}

func dirBBatchedSeeds() int {
	if v := os.Getenv("BATCHED_SEEDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 20000
}

// TestDirBBatchedMatchesPerOp is the batched-transaction gate. Same operations, two transaction
// shapes, one required outcome.
func TestDirBBatchedMatchesPerOp(t *testing.T) {
	seeds := dirBBatchedSeeds()
	var diverged int
	var first []int

	for s := 0; s < seeds; s++ {
		perOp := buildDirBPerOpDoc(s)
		batched := buildDirBBatchedDoc(s)

		want := dirBShape(t, perOp, "per-op", s)
		got := dirBShape(t, batched, "batched", s)
		if want != got {
			diverged++
			if len(first) < 5 {
				first = append(first, s)
			}
			if diverged <= 3 {
				t.Errorf("seed %d: batched document differs from per-operation\n per-op  %s\n batched %s", s, want, got)
			}
			continue
		}

		// The batched document must also survive its OWN encoding. A batched build that merges
		// items wrongly can still render correctly in memory while encoding to bytes that rebuild
		// a different document, and only a round-trip catches that.
		enc, err := EncodeStateAsUpdateV2(batched, nil)
		if err != nil {
			t.Fatalf("seed %d batched encode: %v", s, err)
		}
		fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		_ = ApplyUpdateV2(fresh, enc, nil)
		if rt := dirBShape(t, fresh, "round-trip", s); rt != want {
			diverged++
			if len(first) < 5 {
				first = append(first, s)
			}
			if diverged <= 3 {
				t.Errorf("seed %d: batched encoding round-trips to a different document\n want %s\n got  %s", s, want, rt)
			}
		}
	}

	t.Logf("BATCHED_DIFF total=%d div=%d first=%v", seeds, diverged, first)
}

// ---------------------------------------------------------------- from dir_b_diff_test.go
// dirBCase is what the reference reports back for one library-built document.
type dirBCase struct {
	Seed        int    `json:"seed"`
	ReencodedV2 string `json:"reencodedV2"`
	ReencodedV1 string `json:"reencodedV1"`
	Canon       string `json:"canon"`
	// T031 — snapshot re-encode plus the restored document's shape, so a snapshot that
	// re-encodes correctly but restores wrongly still fails.
	SnapReencodedV2 string `json:"snapReencodedV2"`
	SnapCanon       string `json:"snapCanon"`
	// T032 — a gc-ENABLED document built by this library, re-encoded by the reference.
	GCReencodedV2 string `json:"gcReencodedV2"`
	GCCanon       string `json:"gcCanon"`
	Error         string `json:"error"`
}

var dirBSuccessFields = []string{
	"reencodedV2",
	"reencodedV1",
	"canon",
	"snapReencodedV2",
	"snapCanon",
	"gcReencodedV2",
	"gcCanon",
}

// decodeDirBCase rejects response-schema drift instead of letting encoding/json silently ignore
// a new verifier field. On successful records it also requires every direction-B cell to be
// present. Canonical semantic strings are checked for presence separately from their value so an
// empty-but-valid semantic result could not be mistaken for an omitted comparison.
func decodeDirBCase(data []byte) (dirBCase, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return dirBCase{}, err
	}
	allowed := map[string]struct{}{"seed": {}, "error": {}}
	for _, name := range dirBSuccessFields {
		allowed[name] = struct{}{}
	}
	for name := range raw {
		if _, ok := allowed[name]; !ok {
			return dirBCase{}, fmt.Errorf("unknown direction-B response field %q", name)
		}
	}
	if _, ok := raw["seed"]; !ok {
		return dirBCase{}, fmt.Errorf("missing direction-B response field %q", "seed")
	}

	var c dirBCase
	if err := json.Unmarshal(data, &c); err != nil {
		return dirBCase{}, err
	}
	if c.Error != "" {
		return c, nil
	}
	for _, name := range dirBSuccessFields {
		if _, ok := raw[name]; !ok {
			return dirBCase{}, fmt.Errorf("missing direction-B response field %q", name)
		}
	}
	return c, nil
}

// validateDirBSuccessValues makes a present-but-empty response fail as the missing V1 response
// should always have failed. Every encoded update and every canonical shape in this fixture is
// non-empty. Keeping the checks explicit is intentional: the response audit verifies that each
// field is both checked here and compared against Go's independently produced value below.
func validateDirBSuccessValues(c dirBCase) error {
	values := []struct {
		name  string
		value string
	}{
		{"reencodedV2", c.ReencodedV2},
		{"reencodedV1", c.ReencodedV1},
		{"canon", c.Canon},
		{"snapReencodedV2", c.SnapReencodedV2},
		{"snapCanon", c.SnapCanon},
		{"gcReencodedV2", c.GCReencodedV2},
		{"gcCanon", c.GCCanon},
	}
	for _, value := range values {
		if value.value == "" {
			return fmt.Errorf("the verifier returned an empty %s field — that direction-B cell would compare NOTHING",
				value.name)
		}
	}
	return nil
}

// buildDirBGCDoc constructs a gc-ENABLED document natively, with enough deletion that garbage
// collection actually runs — a build that never deletes would encode identically to a gc=false one
// and the cell would prove nothing.
//
// This is the gap T032 closes. In direction A the reference collects before Go ever sees the bytes,
// so Go's OWN gc decisions — which items become GC structs and how they encode — were never
// compared against the reference at all.
func buildDirBGCDoc(seed int) *Doc {
	rng := newDirBRand(seed)
	doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	letters := "abcde"
	// Build up, then delete large spans, so whole runs of items become collectable rather than
	// leaving single-item tombstones that merge trivially.
	for round := 0; round < 4; round++ {
		for i := 0; i < 8; i++ {
			txt.Insert(txt.Length(), string(letters[rng()%len(letters)]), Object{})
			arr.Insert(arr.GetLength(), ArrayAny{rng() % 100})
			if m != nil {
				m.Set(string(letters[rng()%len(letters)]), rng()%50)
			}
		}
		if txt.Length() > 4 {
			txt.Delete(rng()%(txt.Length()-3), 3)
		}
		if arr.GetLength() > 4 {
			arr.Delete(rng()%(arr.GetLength()-3), 3)
		}
		if m != nil && m.GetSize() > 2 {
			m.Delete(string(letters[rng()%len(letters)]))
		}
	}
	return doc
}

// buildDirBDoc constructs a document NATIVELY with this library. Deterministic in `seed`, and
// deliberately exercises the construction paths direction A cannot reach — notably containers
// populated BEFORE attachment (US2 AS3), which is where the prelim-ordering defect lived.
func buildDirBDoc(seed int) (*Doc, error) {
	rng := newDirBRand(seed)
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	letters := "abcde"
	for i := 0; i < 12; i++ {
		switch rng() % 7 {
		case 0:
			idx := 0
			if txt.Length() > 0 {
				idx = rng() % (txt.Length() + 1)
			}
			txt.Insert(idx, string(letters[rng()%len(letters)]), Object{})
		case 1:
			if txt.Length() > 0 {
				txt.Delete(rng()%txt.Length(), 1)
			}
		case 2:
			idx := 0
			if arr.GetLength() > 0 {
				idx = rng() % (arr.GetLength() + 1)
			}
			arr.Insert(idx, ArrayAny{rng() % 100})
		case 3:
			if m != nil {
				m.Set(string(letters[rng()%len(letters)]), rng()%50)
			}
		case 5:
			// Public operations no direction-A generator can reach, because in direction A the
			// reference drives and these are this library's own API shapes (FR-005).
			switch rng() % 3 {
			case 0:
				arr.Unshift(ArrayAny{rng() % 100})
			case 1:
				// Splice here is a READ (it returns typeListSlice — yjs has no Array.splice, only
				// slice). Exercised so the read path is covered; the result is intentionally
				// discarded, the point being that it must not panic or mutate.
				if arr.GetLength() > 1 {
					_ = arr.Splice(0, arr.GetLength()-1)
				}
			case 2:
				if m != nil && m.GetSize() > 0 {
					m.Clear()
				}
			}
		case 6:
			// Format, INCLUDING ranges that run past the end of the document.
			//
			// Neither direction covered this before. The direction-A generator computes
			// `flen = 1 + rng()*(len - idx)`, so it can never overrun; and direction B had no
			// Format at all, so this library's own formatText was never byte-compared against the
			// reference. The overrun branch is the interesting half: yjs pads the remainder with
			// newlines in a single ContentString (YText.js:341-349 in the pinned 13.6.31) and this
			// port must produce the same count at the same position, or the documents diverge in a
			// way only a byte comparison reveals.
			if txt.Length() > 0 {
				attr := newObject()
				switch rng() % 3 {
				case 0:
					attr.Set("bold", true)
				case 1:
					attr.Set("italic", true)
				default:
					attr.Set("bold", Null)
				}
				start := rng() % txt.Length()
				length := 1 + rng()%3
				if rng()%3 == 0 {
					length = txt.Length() - start + 1 + rng()%3 // past the end
				}
				txt.Format(start, length, attr)
			}
		case 4:
			// Container populated BEFORE attachment — the prelim path. Direction A never reaches
			// it, because in direction A this library only ever applies encoded updates.
			nested := NewYMap(nil)
			nested.Set("k0", rng()%10)
			nested.Set("k1", rng()%10)
			nested.Set("k2", rng()%10)
			idx := 0
			if arr.GetLength() > 0 {
				idx = rng() % (arr.GetLength() + 1)
			}
			arr.Insert(idx, ArrayAny{nested})
		}
	}
	if err := exerciseClientStructTreeDifferentialLifecycle(seed, doc); err != nil {
		return nil, err
	}
	// Read/serialization paths, exercised on the built document. FR-005 puts these in scope
	// because two known defects sit in read paths (a delta rendering that omitted its
	// attribute-presence flag; a text rendering that dropped children of unexpected kinds).
	// Asserting only that they run and do not mutate: their VALUES are compared against the
	// reference by the canon() check in TestDirBDiff.
	before, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		return nil, err
	}
	_ = txt.ToDelta(nil, nil, nil)
	_ = txt.ToString()
	_ = txt.ToJSON()
	_ = txt.GetAttributes(nil)
	_ = arr.ToArray()
	_ = arr.ToJSON()
	if arr.GetLength() > 0 {
		_ = arr.Get(0)
	}
	if m != nil {
		_ = m.ToJSON()
		_ = m.Keys()
		_ = m.Values()
		_ = m.Entries()
		_ = m.GetSize()
		_ = m.Has("k0")
	}
	_ = doc.GetXMLFragment("x").ToJSON()
	after, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		return nil, err
	}
	if string(before) != string(after) {
		return nil, fmt.Errorf("a READ operation mutated the document")
	}

	return doc, nil
}

// newDirBRand is a tiny deterministic LCG. Not shared with the JS harness on purpose: direction B's
// op stream is generated on the GO side, so its randomness lives here.
func newDirBRand(seed int) func() int {
	state := uint64(seed)*6364136223846793005 + 1442695040888963407
	return func() int {
		state = state*6364136223846793005 + 1442695040888963407
		return int((state >> 33) & 0x7fffffff)
	}
}

// requireOracleNode reports the two ways the reference side can be absent, as
// themselves. Without this, a missing fuzz/node_modules surfaces only when the
// verifier subprocess exits non-zero, which prints as "direction-B verifier
// failed" — a message that reads like a divergence in the code under test. Three
// separate fresh worktrees cost a diagnosis that way before it was worth a check;
// a fresh worktree does not inherit the symlink, so this is the normal state of a
// new checkout rather than an exotic one.
//
// Fatal under ORACLE_REQUIRED and skip otherwise, matching how a missing node is
// already treated: the gate must never report success when the reference never ran.
func requireOracleNode(t *testing.T) {
	t.Helper()
	required := os.Getenv("ORACLE_REQUIRED") == "1"
	if _, err := exec.LookPath("node"); err != nil {
		if required {
			t.Fatalf("node not on PATH but ORACLE_REQUIRED=1: %v", err)
		}
		t.Skip("node not on PATH")
	}
	if _, err := os.Stat(repoPath(t, "fuzz", "node_modules", "yjs")); err != nil {
		msg := "fuzz/node_modules/yjs is missing, so the reference implementation cannot run. " +
			"In a fresh worktree, link the pinned install rather than reinstalling, so the oracle " +
			"keeps comparing against the same yjs version:\n" +
			"    ln -sfn <main-checkout>/fuzz/node_modules fuzz/node_modules\n" +
			"Otherwise run `cd fuzz && npm ci`."
		if required {
			t.Fatalf("%s\n(ORACLE_REQUIRED=1: %v)", msg, err)
		}
		t.Skipf("%s\n(%v)", msg, err)
	}
}

// TestDirBDiff is direction B (FR-002): this library builds and encodes, the reference decodes and
// re-encodes, bytes compared. It is the only way to detect a NON-CANONICAL encoding — direction A
// cannot, because there the bytes originate from the reference.
func TestDirBDiff(t *testing.T) {
	requireOracleNode(t)
	resetClientStructTreeGateLifecycle()

	seeds := 300
	if v := os.Getenv("DIRB_SEEDS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &seeds); err != nil {
			t.Fatalf("bad DIRB_SEEDS: %v", err)
		}
	}

	// Counters accumulate ACROSS batches.
	var total, byteDiv, v1ByteDiv, semDiv int
	var snapByteDiv, snapSemDiv, gcByteDiv, gcSemDiv int
	var firstByte, firstV1, firstSem, firstSnap, firstGC []int

	// Batched so peak memory is bounded by the BATCH, not by DIRB_SEEDS.
	//
	// This harness used to build every case up front: one bytes.Buffer holding the whole node
	// input, a second holding the whole node output, and six seed-keyed maps holding the same hex
	// again. At ~1.1 KB per record that is ~1.7 GB of input alone at 1.5M seeds, and a nightly run
	// on a 16 GiB host was OOM-killed — reported as a bare "FAIL" with no divergence summary,
	// because the process died before reaching its own report. Memory must not scale with the seed
	// count when the whole point of the ultimate tier is a large seed count.
	//
	// Batches are independent: each is built, verified and compared before the next is allocated,
	// so the comparison semantics are identical and only the peak footprint changes.
	batch := dirBBatchSize()
	for batchStart := 1; batchStart <= seeds; batchStart += batch {
		batchEnd := batchStart + batch - 1
		if batchEnd > seeds {
			batchEnd = seeds
		}
		n := batchEnd - batchStart + 1

		var in bytes.Buffer
		goV2 := make(map[int]string, n)
		goV1 := make(map[int]string, n)
		goJSON := make(map[int]string, n)
		goSnapV2 := make(map[int]string, n)
		goSnapJSON := make(map[int]string, n)
		goGCV2 := make(map[int]string, n)
		goGCJSON := make(map[int]string, n)
		for s := batchStart; s <= batchEnd; s++ {
			doc, err := buildDirBDoc(s)
			if err != nil {
				t.Fatalf("seed %d build: %v", s, err)
			}
			enc, err := EncodeStateAsUpdateV2(doc, nil)
			if err != nil {
				t.Fatalf("seed %d encode: %v", s, err)
			}
			goV2[s] = hex.EncodeToString(enc)
			encV1, err := EncodeStateAsUpdate(doc, nil)
			if err != nil {
				t.Fatalf("seed %d encode V1: %v", s, err)
			}
			goV1[s] = hex.EncodeToString(encV1)
			// Same canonical shape the verifier builds, so the comparison is order-independent and
			// covers the same three types on both sides.
			shape := newObject()
			shape.Set("t", doc.GetText("t").ToString())
			shape.Set("a", doc.GetArray("a").ToJSON())
			shape.Set("m", doc.GetMap("m").ToJSON())
			cn, err := fuzzCanon(shape)
			if err != nil {
				t.Fatalf("seed %d canon: %v", s, err)
			}
			goJSON[s] = cn

			// T031 — snapshot of the SAME document, encoded by this library. Also compute what this
			// library restores from it, so the reference's restored shape can be compared against
			// ours rather than only against itself.
			snap := NewSnapshotByDoc(doc)
			snapBytes, err := EncodeSnapshotV2(snap)
			if err != nil {
				t.Fatalf("seed %d snapshot encode: %v", s, err)
			}
			goSnapV2[s] = hex.EncodeToString(snapBytes)
			restored, err := CreateDocFromSnapshot(doc, snap,
				newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1)))
			if err != nil {
				t.Fatalf("seed %d restore: %v", s, err)
			}
			rshape := newObject()
			rshape.Set("t", restored.GetText("t").ToString())
			rshape.Set("a", restored.GetArray("a").ToJSON())
			rshape.Set("m", restored.GetMap("m").ToJSON())
			if goSnapJSON[s], err = fuzzCanon(rshape); err != nil {
				t.Fatalf("seed %d restored canon: %v", s, err)
			}

			// T032 — a gc-ENABLED build. Direction A never checks this library's OWN gc decisions,
			// because there the reference has already collected before Go sees any bytes.
			gdoc := buildDirBGCDoc(s)
			genc, err := EncodeStateAsUpdateV2(gdoc, nil)
			if err != nil {
				t.Fatalf("seed %d gc encode: %v", s, err)
			}
			goGCV2[s] = hex.EncodeToString(genc)
			gshape := newObject()
			gshape.Set("t", gdoc.GetText("t").ToString())
			gshape.Set("a", gdoc.GetArray("a").ToJSON())
			gshape.Set("m", gdoc.GetMap("m").ToJSON())
			if goGCJSON[s], err = fuzzCanon(gshape); err != nil {
				t.Fatalf("seed %d gc canon: %v", s, err)
			}

			fmt.Fprintf(&in, `{"seed":%d,"updateV2":%q,"snapshotV2":%q,"gcUpdateV2":%q}`+"\n",
				s, goV2[s], goSnapV2[s], goGCV2[s])
		}

		cmd := exec.Command("node", repoPath(t, "fuzz/dir_b_verify.mjs"))
		cmd.Dir = repoRoot(t)
		cmd.Stdin = &in
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("direction-B verifier failed: %v\nstderr: %s", err, errBuf.String())
		}

		sc := bufio.NewScanner(&out)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		seenSeeds := make(map[int]struct{}, n)

		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				continue
			}
			c, err := decodeDirBCase(sc.Bytes())
			if err != nil {
				t.Fatalf("bad direction-B record: %v", err)
			}
			if c.Seed < batchStart || c.Seed > batchEnd {
				t.Fatalf("direction-B verifier returned seed %d outside requested batch [%d,%d]",
					c.Seed, batchStart, batchEnd)
			}
			if _, duplicate := seenSeeds[c.Seed]; duplicate {
				t.Fatalf("direction-B verifier returned seed %d more than once; another seed could be missing while the count still passes", c.Seed)
			}
			seenSeeds[c.Seed] = struct{}{}
			total++
			if c.Error != "" {
				t.Errorf("seed %d: the reference could not decode this library's update: %s", c.Seed, c.Error)
				byteDiv++
				// Record the seed here too. Without this, a decode failure counted toward byteDiv but
				// left firstByte empty, so the summary said "N/300 diverged (first [])" and named
				// nothing — the same actionability problem the corpus guard's surface name solves.
				if len(firstByte) < 8 {
					firstByte = append(firstByte, c.Seed)
				}
				continue
			}
			if err := validateDirBSuccessValues(c); err != nil {
				t.Fatalf("seed %d: %v", c.Seed, err)
			}
			// The library's bytes must BE the reference's canonical encoding, not merely decodable
			// into something equivalent.
			if c.ReencodedV2 != goV2[c.Seed] {
				byteDiv++
				if len(firstByte) < 8 {
					firstByte = append(firstByte, c.Seed)
				}
			}
			if c.ReencodedV1 != goV1[c.Seed] {
				v1ByteDiv++
				if len(firstV1) < 8 {
					firstV1 = append(firstV1, c.Seed)
				}
			}
			if c.Canon != goJSON[c.Seed] {
				semDiv++
				if len(firstSem) < 8 {
					firstSem = append(firstSem, c.Seed)
				}
			}
			// T031 — the snapshot this library encoded must BE the reference's canonical snapshot,
			// and must restore to the same document on both sides.
			if c.SnapReencodedV2 != goSnapV2[c.Seed] {
				snapByteDiv++
				if len(firstSnap) < 8 {
					firstSnap = append(firstSnap, c.Seed)
				}
			}
			if c.SnapCanon != goSnapJSON[c.Seed] {
				snapSemDiv++
				if len(firstSnap) < 8 {
					firstSnap = append(firstSnap, c.Seed)
				}
			}
			// T032 — this library's gc-enabled encoding must be canonical to the reference too.
			if c.GCReencodedV2 != goGCV2[c.Seed] {
				gcByteDiv++
				if len(firstGC) < 8 {
					firstGC = append(firstGC, c.Seed)
				}
			}
			if c.GCCanon != goGCJSON[c.Seed] {
				gcSemDiv++
				if len(firstGC) < 8 {
					firstGC = append(firstGC, c.Seed)
				}
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		for seed := batchStart; seed <= batchEnd; seed++ {
			if _, ok := seenSeeds[seed]; !ok {
				t.Fatalf("direction-B verifier returned no response for seed %d", seed)
			}
		}
	}

	if err := oracle.CheckCorpus("dirB", "<generated in-process>", total); err != nil {
		t.Fatal(err)
	}

	t.Logf("DIRB_DIFF total=%d byteDiv=%d semDiv=%d firstByte=%v firstSem=%v",
		total, byteDiv, semDiv, firstByte, firstSem)
	t.Logf("DIRB_V1 total=%d byteDiv=%d first=%v", total, v1ByteDiv, firstV1)
	t.Logf("DIRB_SNAPSHOT total=%d byteDiv=%d semDiv=%d first=%v", total, snapByteDiv, snapSemDiv, firstSnap)
	t.Logf("DIRB_GC total=%d byteDiv=%d semDiv=%d first=%v", total, gcByteDiv, gcSemDiv, firstGC)
	if snapByteDiv > 0 {
		t.Errorf("direction B snapshot: this library's snapshot encoding is NOT canonical for %d/%d seeds (first %v)",
			snapByteDiv, total, firstSnap)
	}
	if snapSemDiv > 0 {
		t.Errorf("direction B snapshot: restoring diverged for %d/%d seeds (first %v)",
			snapSemDiv, total, firstSnap)
	}
	if gcByteDiv > 0 {
		t.Errorf("direction B gc: this library's gc-enabled encoding is NOT canonical for %d/%d seeds (first %v)",
			gcByteDiv, total, firstGC)
	}
	if gcSemDiv > 0 {
		t.Errorf("direction B gc: the gc-enabled document diverged for %d/%d seeds (first %v)",
			gcSemDiv, total, firstGC)
	}
	if byteDiv > 0 {
		t.Errorf("direction B: this library's encoding is NOT canonical for %d/%d seeds (first %v)",
			byteDiv, total, firstByte)
	}
	if v1ByteDiv > 0 {
		t.Errorf("direction B: this library's V1 encoding is NOT canonical for %d/%d seeds (first %v)",
			v1ByteDiv, total, firstV1)
	}
	if semDiv > 0 {
		t.Errorf("direction B: semantic mismatch %d/%d (first %v)", semDiv, total, firstSem)
	}
	requireClientStructTreeGateLifecycle(t)
}

// SC-005: constructing the same logical document repeatedly must produce ONE encoding. Before the
// prelim-ordering fix the equivalent produced four distinct encodings across forty builds.
func TestDirBBuildDeterminism(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		doc, err := buildDirBDoc(7)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[string(enc)] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("100 identical builds produced %d distinct encodings, want 1 (SC-005)", len(seen))
	}
}

// dirBBatchSize is how many seeds direction B builds, verifies and compares at a time.
//
// 20,000 keeps peak memory in the tens of MB at any DIRB_SEEDS while still amortising the node
// process launch. Override with DIRB_BATCH when profiling the harness itself.
func dirBBatchSize() int {
	if v := os.Getenv("DIRB_BATCH"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 20000
}

// ---------------------------------------------------------------- from dir_b_response_audit_test.go
// TestDirectionBResponseSchemaIsConsumed guards the exact failure that left canonical V1 bytes
// on the wire but unexamined in Go. Every field the Node verifier emits must be declared by Go;
// every declared response field must be emitted; and every successful cell must have two explicit
// reads in the harness — one non-vacuity check and one comparison with Go's independent value.
func TestDirectionBResponseSchemaIsConsumed(t *testing.T) {
	js, err := os.ReadFile(repoPath(t, "fuzz/dir_b_verify.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	emitted := map[string]bool{"seed": strings.Contains(string(js), "const out = { seed: rec.seed }")}
	assignment := regexp.MustCompile(`out\.([A-Za-z][A-Za-z0-9]*)\s*=`)
	for _, match := range assignment.FindAllSubmatch(js, -1) {
		emitted[string(match[1])] = true
	}

	declared := make(map[string]string)
	typ := reflect.TypeOf(dirBCase{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			declared[name] = field.Name
		}
	}
	for name := range emitted {
		if _, ok := declared[name]; !ok {
			t.Errorf("Node verifier emits %q but dirBCase does not declare it; encoding/json would ignore the cell", name)
		}
	}
	for name := range declared {
		if !emitted[name] {
			t.Errorf("dirBCase declares %q but the Node verifier never emits it", name)
		}
	}

	// Parse THIS file rather than a hard-coded name. The guard needs the source
	// that actually reads the response fields, and naming it as a string breaks
	// silently the moment the file is renamed or merged — which is exactly what
	// happened when the per-subject test files were consolidated. runtime.Caller
	// keeps the reference correct through any future move.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's source file")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, thisFile, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	selectorReads := make(map[string]int)
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			selectorReads[selector.Sel.Name]++
		}
		return true
	})
	for jsonName, goName := range declared {
		minimum := 1
		if jsonName != "seed" && jsonName != "error" {
			minimum = 2
		}
		if selectorReads[goName] < minimum {
			t.Errorf("direction-B field %q has %d explicit reads, want at least %d (presence and comparison)",
				jsonName, selectorReads[goName], minimum)
		}
	}
}

func TestDecodeDirBCaseRequiresCompleteKnownResponse(t *testing.T) {
	complete := map[string]any{
		"seed":            1,
		"reencodedV2":     "01",
		"reencodedV1":     "01",
		"canon":           `{}`,
		"snapReencodedV2": "01",
		"snapCanon":       `{}`,
		"gcReencodedV2":   "01",
		"gcCanon":         `{}`,
	}
	encode := func(fields map[string]any) []byte {
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, err := decodeDirBCase(encode(complete)); err != nil {
		t.Fatalf("complete response rejected: %v", err)
	}
	for _, empty := range dirBSuccessFields {
		fields := make(map[string]any, len(complete))
		for name, value := range complete {
			fields[name] = value
		}
		fields[empty] = ""
		decoded, err := decodeDirBCase(encode(fields))
		if err != nil {
			t.Fatalf("present empty %q was mistaken for a missing field: %v", empty, err)
		}
		if err := validateDirBSuccessValues(decoded); err == nil || !strings.Contains(err.Error(), empty) {
			t.Errorf("empty %q: error=%v, want the empty cell named", empty, err)
		}
	}
	for _, missing := range append([]string{"seed"}, dirBSuccessFields...) {
		fields := make(map[string]any, len(complete)-1)
		for name, value := range complete {
			if name != missing {
				fields[name] = value
			}
		}
		if _, err := decodeDirBCase(encode(fields)); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %q: error=%v, want the omitted field named", missing, err)
		}
	}
	withUnknown := make(map[string]any, len(complete)+1)
	for name, value := range complete {
		withUnknown[name] = value
	}
	withUnknown["futureCell"] = "unexamined"
	if _, err := decodeDirBCase(encode(withUnknown)); err == nil || !strings.Contains(err.Error(), "futureCell") {
		t.Errorf("unknown response field: error=%v, want futureCell named", err)
	}
	if _, err := decodeDirBCase([]byte(`{"seed":1,"error":"decode failed"}`)); err != nil {
		t.Errorf("reference error response rejected for lacking success fields: %v", err)
	}
}
