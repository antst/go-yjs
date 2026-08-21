package crdt

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// ---------------------------------------------------------------- from read_cache_option_test.go
func TestWithReadCacheDisabledRetainsNoDerivedProjections(t *testing.T) {
	doc := newDoc("no-read-cache", false, defaultGCFilter, nil, false,
		WithClientID(1), WithReadCache(false))

	text := doc.GetText("t")
	text.Insert(0, "abcdef", Object{})
	attrs := newObject()
	attrs.Set("bold", true)
	text.Format(1, 3, attrs)
	for i := 0; i < yMapEntriesCacheThreshold+2; i++ {
		_ = text.ToString()
		_ = text.ToDelta(nil, nil, nil)
	}
	if text.stringCache.Load() != nil || text.deltaCache.Load() != nil || text.deltaCachePrimed.Load() {
		t.Fatal("disabled document retained a text read cache")
	}

	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	for i := 0; i < yMapEntriesCacheThreshold+2; i++ {
		_ = ym.Keys()
		_ = ym.Entries()
		_ = ym.ToJSON()
	}
	if ym.keysCache.Load() != nil || ym.entriesCache.Load() != nil || ym.jsonCache.Load() != nil {
		t.Fatal("disabled document retained a map read cache")
	}
	if ym.keysPrimed.Load() || ym.entriesReads.Load() != 0 || ym.jsonReads.Load() != 0 {
		t.Fatal("disabled document retained map cache priming state")
	}

	xml := doc.GetXMLFragment("x")
	xml.Insert(0, ArrayAny{NewYXmlElement("a"), NewYXmlElement("b")})
	_, _ = xml.Slice(0, 2), xml.Slice(0, 2)
	if xml.sliceCache.Load() != nil || xml.slicePrimed.Load() {
		t.Fatal("disabled document retained an XML slice cache")
	}
}

// ---------------------------------------------------------------- from read_cache_stress_test.go
// Read-cache invalidation stress.
//
// WHY THE GATE CANNOT SEE THIS. The differential oracle builds a document, reads it, compares, and
// moves on. A read cache only becomes dangerous on the SECOND read of a document that changed in
// between, and it is primed only by repeated reads — so a per-seed read-once gate never primes the
// cache at all, let alone catches a stale one. No seed count fixes that: the shape of the check is
// wrong, not its volume.
//
// THE INVARIANT. Two documents receive an identical operation sequence. One is read after every
// single step, so its caches are primed and re-primed constantly; the other is read exactly once,
// at the very end, so it can never serve a cached value. Their observable content must be equal at
// every point. Any divergence is a stale cache, because the operation sequences are identical by
// construction.
//
// This is the same shape as the batched-transaction gate: hold the work constant, vary only the
// thing under suspicion, and require the results to agree.
//
// WHAT IS VARIED, chosen because each reaches the cache by a different route:
//
//   local mutation      — the obvious invalidation path
//   REMOTE update       — arrives through ApplyUpdate rather than a public setter, and is the
//                         invalidation people forget, because the type's own methods are never called
//   formatting          — changes rendering without changing the character sequence, so a cache
//                         keyed only on text content would wrongly survive it
//   nested type edits   — mutate a child; the PARENT's rendering changes
//   deletion            — content vanishes rather than appearing
//   snapshot reads      — must bypass the cache entirely; a snapshot read is a read of a DIFFERENT
//                         document state and must never be served from, or poison, the live cache

func cacheStressRounds() int {
	if v := os.Getenv("CACHE_STRESS_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2000
}

// deltaSemantic renders a delta by VALUE. fmt.Sprintf("%v", delta) must not be used: EventOperator
// holds Attributes as an Object whose backing store is a pointer, so %v prints an allocation
// ADDRESS. Since ToDelta correctly clones attributes for each caller, two semantically identical
// deltas print differently every time -- comparing them compares the allocator, not the document.
// The first version of this file did exactly that and reported twenty concurrent readers
// "diverging" on byte-identical content.
func deltaSemantic(ops []EventOperator) string {
	out := newObject()
	for i, op := range ops {
		e := newObject()
		if op.IsInsert() {
			e.Set("insert", op.InsertValue())
		}
		if op.Kind == EventOperatorRetain {
			e.Set("retain", op.Length)
		}
		if op.Kind == EventOperatorDelete {
			e.Set("delete", op.Length)
		}
		if op.HasAttributes() {
			e.Set("attrs", op.Attributes)
		}
		out.Set(strconv.Itoa(i), e)
	}
	cn, err := fuzzCanon(out)
	if err != nil {
		return "ERR:" + err.Error()
	}
	return cn
}

// readAll touches every cached read path. Returned as a canonical string so a divergence anywhere
// shows up as a string mismatch with both sides printable.
func readAll(doc *Doc) string {
	txt := doc.GetText("t")
	shape := newObject()
	shape.Set("toString", txt.ToString())
	shape.Set("toJson", txt.ToJSON())
	shape.Set("toDelta", deltaSemantic(txt.ToDelta(nil, nil, nil)))
	shape.Set("arr", doc.GetArray("a").ToJSON())
	// All three map read caches, because they are cross-wired: Keys and Entries may be served from
	// the JSON projection when it is active, and the JSON and Entries caches are mutually
	// exclusive. Reading only one of them would leave the reuse paths unobserved.
	m := doc.GetMap("m")
	shape.Set("map", m.ToJSON())
	// SORTED: Keys() returns Go map-iteration order, which the runtime randomises per call, so
	// comparing it raw compares the iterator rather than the document. This is the third variant of
	// the same mistake in this file -- first pointer addresses, then insertion-ordered JSON, now
	// map order -- so the rule is simply that anything order-unstable gets normalised before it is
	// compared.
	keys := append([]string(nil), m.Keys()...)
	sort.Strings(keys)
	shape.Set("mapKeys", fmt.Sprintf("%v", keys))
	shape.Set("mapEntries", fmt.Sprintf("%d", len(m.Entries())))
	shape.Set("mapSize", strconv.Itoa(m.GetSize()))
	// Root/type-level ATTRIBUTES. Skipping their invalidation is only safe if no cached read
	// projection depends on them, which is a claim about every projection below rather than about
	// the attribute itself — so every projection that could conceivably carry one is read here.
	// A child element's attribute is the interesting case: it is reachable through the PARENT's
	// rendering and slice cache, not only through the child's own accessors.
	f := doc.GetXMLFragment("x")
	shape.Set("xmlToString", f.ToString())
	shape.Set("xmlSliceLen", strconv.Itoa(len(f.Slice(0, f.GetLength()))))
	if f.GetLength() > 0 {
		if el, ok := f.Get(0).(*YXmlElement); ok {
			shape.Set("elToString", el.ToString())
			shape.Set("elAttrs", canonOf(el.GetAttributes()))
		}
	}
	shape.Set("textAttrs", canonOf(txt.GetAttributes(nil)))
	cn, err := fuzzCanon(shape)
	if err != nil {
		return "ERR:" + err.Error()
	}
	return cn
}

// applyCacheStep performs one deterministic mutation on `doc`. `remote`, when non-nil, receives the
// same change as an ENCODED UPDATE instead — the path a cache is most likely to miss.
func applyCacheStep(doc *Doc, step, seed int) {
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	m := doc.GetMap("m")

	switch step % 8 {
	case 0:
		txt.Insert(txt.Length(), fmt.Sprintf("%d", seed%10), Object{})
	case 1:
		if txt.Length() > 2 {
			txt.Delete(seed%(txt.Length()-1), 1)
		}
	case 2: // formatting: rendering changes, characters do not
		if txt.Length() > 4 {
			attr := newObject()
			attr.Set("bold", seed%2 == 0)
			txt.Format(seed%(txt.Length()-3), 3, attr)
		}
	case 3:
		arr.Insert(arr.GetLength(), ArrayAny{seed % 100})
	case 4:
		if m != nil {
			m.Set(fmt.Sprintf("k%d", seed%9), seed%50)
		}
	case 5: // nested type: the child changes, the PARENT's rendering must follow
		nested := NewYMap(nil)
		nested.Set("n", seed%17)
		arr.Insert(arr.GetLength(), ArrayAny{nested})
		// A nested type INSIDE THE MAP, and an Undefined value: the JSON projection cache declines
		// to cache either, so these drive the decline path and the transition between a cacheable
		// and an uncacheable map. An exclusion condition that is never exercised is an exclusion
		// condition that is never checked.
		if m != nil {
			if seed%3 == 0 {
				inner := NewYMap(nil)
				inner.Set("deep", seed%5)
				m.Set("nested", inner)
			}
			if seed%5 == 0 {
				m.Set("undef", Undefined)
			}
		}
	case 6: // type-level attributes: the paths whose invalidation was narrowed
		f := doc.GetXMLFragment("x")
		if f.GetLength() == 0 {
			f.Insert(0, ArrayAny{NewYXmlElement("div")})
		}
		if el, ok := f.Get(0).(*YXmlElement); ok {
			el.SetAttribute("id", fmt.Sprintf("v%d", seed%7))
			if seed%3 == 0 {
				el.SetAttribute("class", fmt.Sprintf("c%d", seed%5))
			}
			if seed%5 == 0 {
				el.RemoveAttribute("class")
			}
		}
		txt.SetAttribute("lang", fmt.Sprintf("l%d", seed%4))
	default:
		if txt.Length() > 1 {
			txt.Insert(seed%txt.Length(), "z", Object{})
		}
	}
}

// TestReadCacheMatchesUncached is the invalidation gate.
func TestReadCacheMatchesUncached(t *testing.T) {
	rounds := cacheStressRounds()
	var diverged int
	var first []int

	for s := 0; s < rounds; s++ {
		hot := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		cold := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))

		// Force both to materialise their root types identically before any divergence is possible.
		_, _ = hot.GetText("t"), cold.GetText("t")

		steps := 4 + s%9
		for step := 0; step < steps; step++ {
			applyCacheStep(hot, step, s+step)
			applyCacheStep(cold, step, s+step)

			// hot is read TWICE after every step: the first read primes, the second is the one a
			// deferred cache would serve. cold is never read inside the loop.
			_ = readAll(hot)
			got := readAll(hot)
			want := readAll(cold)
			if got != want {
				diverged++
				if len(first) < 5 {
					first = append(first, s)
				}
				if diverged <= 3 {
					t.Errorf("seed %d step %d: cached read differs from uncached\n want %s\n got  %s",
						s, step, want, got)
				}
				break
			}
		}
	}
	t.Logf("READ_CACHE_DIFF total=%d div=%d first=%v", rounds, diverged, first)
}

// TestReadCacheInvalidatedByRemoteUpdate isolates the route a cache is most likely to miss: content
// arriving through ApplyUpdate, where none of the type's own mutating methods are ever called.
func TestReadCacheInvalidatedByRemoteUpdate(t *testing.T) {
	rounds := cacheStressRounds() / 4
	for s := 0; s < rounds; s++ {
		recv := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		sender := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))

		recv.GetText("t").Insert(0, "base", Object{})
		u, err := EncodeStateAsUpdateV2(recv, nil)
		if err != nil {
			t.Fatalf("seed %d encode base: %v", s, err)
		}
		_ = ApplyUpdateV2(sender, u, nil)

		// Prime the receiver's caches HARD before the remote change arrives.
		for i := 0; i < 3; i++ {
			_ = readAll(recv)
		}
		before := readAll(recv)

		// Mutate on the sender and deliver as an update. The receiver's own methods are untouched.
		applyCacheStep(sender, s%7, s)
		if s%3 == 0 {
			sender.GetText("t").Insert(0, "R", Object{})
		}
		u2, err := EncodeStateAsUpdateV2(sender, nil)
		if err != nil {
			t.Fatalf("seed %d encode delta: %v", s, err)
		}
		_ = ApplyUpdateV2(recv, u2, nil)

		got := readAll(recv)

		// Compare against a document that reached the same state without ever holding a cache.
		fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(3))
		_ = ApplyUpdateV2(fresh, u2, nil)
		want := readAll(fresh)

		if got != want {
			t.Fatalf("seed %d: remote update did not invalidate the read cache\n"+
				" before  %s\n after   %s\n uncached %s", s, before, got, want)
		}
	}
	t.Logf("READ_CACHE_REMOTE rounds=%d", rounds)
}

// TestReadCacheSnapshotDoesNotPoison checks that reading a document AS OF a snapshot neither serves
// the live cached value nor leaves the snapshot's rendering behind for the next live read. A
// snapshot read is a read of a different document state; conflating the two in either direction
// silently returns history as if it were current.
func TestReadCacheSnapshotDoesNotPoison(t *testing.T) {
	rounds := cacheStressRounds() / 10
	for s := 0; s < rounds; s++ {
		doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		txt.Insert(0, "alpha", Object{})

		for i := 0; i < 3; i++ {
			_ = txt.ToString()
		}
		snap := NewSnapshotByDoc(doc)

		txt.Insert(txt.Length(), "-beta", Object{})
		liveBefore := txt.ToString()

		// Read as of the snapshot, repeatedly, so any cache it populates is well established.
		for i := 0; i < 3; i++ {
			_ = txt.ToDelta(snap, nil, nil)
		}

		if liveAfter := txt.ToString(); liveAfter != liveBefore {
			t.Fatalf("seed %d: snapshot read poisoned the live cache\n before %q\n after  %q",
				s, liveBefore, liveAfter)
		}
		if liveAfter := txt.ToString(); liveAfter != liveBefore {
			t.Fatalf("seed %d: second live read after snapshot differs\n before %q\n after  %q",
				s, liveBefore, liveAfter)
		}
	}
	t.Logf("READ_CACHE_SNAPSHOT rounds=%d", rounds)
}

// ---------------------------------------------------------------- from read_index_all_paths_test.go
// Exhaustive validation of the read-index invalidation invariant, one mutation family at a time.
//
// WHY PER-PATH AND NOT ONE RANDOM SOAK. The staleness defect was argued away by a per-path claim —
// "a tail append moves no existing start index" — that was true of the append and false of the
// transaction that contained it. Every remaining path carries a claim of the same shape: splitting
// preserves earlier starts, GC only touches already-deleted items, undo routes through AddChanged.
// Each may well be right. But a single mixed soak that passes tells you nothing about WHICH claim it
// exercised, and a soak that fails tells you nothing about which one broke. One test per family
// means a failure names its own cause.
//
// THE INVARIANT, which is stronger than comparing reads. Every published position must reference an
// item still in the live list, at exactly the visible start index it claims. A stale entry only
// returns wrong data when walk direction and spacing happen to align, so a Get-versus-ToArray
// comparison masks it most of the time — that is precisely how the first defect survived two of my
// own probes. Both checks run here; the invariant is the one that bites.

var idxInspections int

func idxValidate(t *testing.T, arr *YArray, label string) {
	t.Helper()
	idx := arr.readIndex.Load()
	if idx == nil || idx == buildingListReadIndex {
		return
	}
	if len(idx.positions) > 0 {
		idxInspections++
	}
	truePos := map[*itemStruct]Number{}
	visible := Number(0)
	for p := arr.startItem(); p != nil; p = p.right {
		if p.isDeleted() || !p.countable() {
			continue
		}
		truePos[p] = visible
		visible += p.length
	}
	for i, pos := range idx.positions {
		tp, ok := truePos[pos.p]
		if !ok {
			t.Fatalf("%s: position %d references an item NOT in the live list (claims visible "+
				"index %d) — a path removed it without invalidating", label, i, pos.index)
		}
		if tp != pos.index {
			t.Fatalf("%s: position %d claims visible index %d, actual %d", label, i, pos.index, tp)
		}
	}
	if visible != arr.GetLength() {
		t.Fatalf("%s: walked visible length %d != GetLength %d", label, visible, arr.GetLength())
	}
}

func idxGetAgrees(t *testing.T, arr *YArray, label string) {
	t.Helper()
	want := arr.ToArray()
	for i := range want {
		if got := arr.Get(i); fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want[i]) {
			t.Fatalf("%s: Get(%d)=%v, want %v", label, i, got, want[i])
		}
	}
}

// idxFixture builds a FRAGMENTED array and publishes an index. Fragmentation matters: a list that
// collapses to one item is skipped by listReadPositions (StartItem().Right == nil), which masks
// every defect in this file.
func idxFixture(seed int, n int) (*Doc, *YArray) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(uint32(seed*2654435761 + 7))
	for i := 0; i < n; i++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
	}
	return doc, arr
}

// SplitItem. The claim is that splitting preserves every pre-existing item's visible start: the
// left half keeps its identity and start, the new right half is not in any published index, and
// later items are unmoved. A mid-list insert into a coalesced run is what forces the split.
func TestReadIndexSplitPath(t *testing.T) {
	for seed := 0; seed < 150; seed++ {
		doc, arr := idxFixture(seed, 300)
		// Append a coalesced run so there is a multi-element item to split.
		for i := 0; i < 40; i++ {
			arr.Insert(arr.GetLength(), ArrayAny{5000 + i})
		}
		_ = arr.Get(arr.GetLength() / 2)
		idxValidate(t, arr, fmt.Sprintf("seed %d after publish", seed))

		rng := markerLCG(uint32(seed + 99))
		for k := 0; k < 8; k++ {
			at := arr.GetLength() - 20 + rng(19)
			arr.Insert(at, ArrayAny{7000 + k}) // splits the run
			_ = arr.Get(rng(arr.GetLength()))
			idxValidate(t, arr, fmt.Sprintf("seed %d split %d", seed, k))
			idxGetAgrees(t, arr, fmt.Sprintf("seed %d split %d", seed, k))
		}
		_ = doc
	}
}

// GC. The claim is that GC only reaches already-deleted items, which are never indexed. The doc is
// created WITH gc enabled so deletion actually collects.
func TestReadIndexGcPath(t *testing.T) {
	for seed := 0; seed < 150; seed++ {
		doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		rng := markerLCG(uint32(seed*7919 + 3))
		for i := 0; i < 300; i++ {
			arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
		}
		_ = arr.Get(150)
		idxValidate(t, arr, fmt.Sprintf("seed %d before delete", seed))

		for k := 0; k < 6; k++ {
			if arr.GetLength() > 10 {
				arr.Delete(rng(arr.GetLength()-5), 3)
			}
			_ = arr.Get(rng(arr.GetLength()))
			idxValidate(t, arr, fmt.Sprintf("seed %d gc %d", seed, k))
			idxGetAgrees(t, arr, fmt.Sprintf("seed %d gc %d", seed, k))
		}
	}
}

// Nested type deletion drives ContentType.Delete, which must invalidate the child's list index
// before GC resets Start.
func TestReadIndexNestedTypeDeletePath(t *testing.T) {
	for seed := 0; seed < 150; seed++ {
		doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
		outer := doc.GetArray("a")
		rng := markerLCG(uint32(seed*31337 + 11))

		inner := NewYArray()
		outer.Insert(0, ArrayAny{inner})
		for i := 0; i < 200; i++ {
			inner.Insert(rng(inner.GetLength()+1), ArrayAny{i})
		}
		_ = inner.Get(100)
		idxValidate(t, inner, fmt.Sprintf("seed %d inner published", seed))

		for i := 0; i < 60; i++ {
			outer.Insert(rng(outer.GetLength()+1), ArrayAny{i})
		}
		_ = outer.Get(30)
		idxValidate(t, outer, fmt.Sprintf("seed %d outer published", seed))

		outer.Delete(0, 1) // deletes the nested type
		_ = outer.Get(rng(outer.GetLength()))
		idxValidate(t, outer, fmt.Sprintf("seed %d outer after nested delete", seed))
		idxGetAgrees(t, outer, fmt.Sprintf("seed %d outer after nested delete", seed))
	}
}

// Undo and redo. The claim is that their structural integration and deletion route through
// addChangedTypeToTransaction, and that their splits share the position-preserving property.
func TestReadIndexUndoRedoPath(t *testing.T) {
	for seed := 0; seed < 100; seed++ {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		rng := markerLCG(uint32(seed*104729 + 5))
		for i := 0; i < 250; i++ {
			arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
		}
		um := newUndoManager(arr, 500, func(_ *itemStruct) bool { return true }, defaultTrackedOrigins())
		_ = arr.Get(120)
		idxValidate(t, arr, fmt.Sprintf("seed %d before undo", seed))

		for k := 0; k < 5; k++ {
			arr.Insert(rng(arr.GetLength()+1), ArrayAny{8000 + k})
			if arr.GetLength() > 6 {
				arr.Delete(rng(arr.GetLength()-3), 2)
			}
			_ = arr.Get(rng(arr.GetLength()))

			um.Undo()
			_ = arr.Get(rng(arr.GetLength()))
			idxValidate(t, arr, fmt.Sprintf("seed %d undo %d", seed, k))
			idxGetAgrees(t, arr, fmt.Sprintf("seed %d undo %d", seed, k))

			um.Redo()
			_ = arr.Get(rng(arr.GetLength()))
			idxValidate(t, arr, fmt.Sprintf("seed %d redo %d", seed, k))
			idxGetAgrees(t, arr, fmt.Sprintf("seed %d redo %d", seed, k))
		}
	}
}

// Remote integration: content arrives through ApplyUpdate and the type's own methods are never
// called, which is the invalidation people forget.
func TestReadIndexRemoteApplyPath(t *testing.T) {
	for seed := 0; seed < 150; seed++ {
		doc, arr := idxFixture(seed, 250)
		peer := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("seed %d encode: %v", seed, err)
		}
		_ = ApplyUpdateV2(peer, enc, nil)

		_ = arr.Get(120) // publish BEFORE the remote change arrives
		idxValidate(t, arr, fmt.Sprintf("seed %d published", seed))

		rng := markerLCG(uint32(seed + 4242))
		pa := peer.GetArray("a")
		for k := 0; k < 5; k++ {
			pa.Insert(rng(pa.GetLength()+1), ArrayAny{6000 + k})
		}
		if pa.GetLength() > 8 {
			pa.Delete(rng(pa.GetLength()-4), 3)
		}
		back, err := EncodeStateAsUpdateV2(peer, nil)
		if err != nil {
			t.Fatalf("seed %d re-encode: %v", seed, err)
		}
		_ = ApplyUpdateV2(doc, back, nil)

		_ = arr.Get(rng(arr.GetLength()))
		idxValidate(t, arr, fmt.Sprintf("seed %d after remote", seed))
		idxGetAgrees(t, arr, fmt.Sprintf("seed %d after remote", seed))
	}
}

// XmlFragment carries its own readIndex and reaches typeListGet by a different route, so it is a
// separate surface rather than a duplicate of the array cases.
func TestReadIndexXmlFragmentPath(t *testing.T) {
	for seed := 0; seed < 100; seed++ {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		f := doc.GetXMLFragment("x")
		rng := markerLCG(uint32(seed*15485863 + 17))
		for i := 0; i < 200; i++ {
			el := NewYXmlElement("div")
			f.Insert(rng(f.GetLength()+1), ArrayAny{el})
		}
		_ = f.Get(100)

		for k := 0; k < 6; k++ {
			f.Insert(rng(f.GetLength()+1), ArrayAny{NewYXmlElement("span")})
			if f.GetLength() > 6 {
				f.Delete(rng(f.GetLength()-3), 2)
			}
			_ = f.Get(rng(f.GetLength()))

			idx := f.readIndex.Load()
			if idx == nil || idx == buildingListReadIndex {
				continue
			}
			truePos := map[*itemStruct]Number{}
			visible := Number(0)
			for p := f.startItem(); p != nil; p = p.right {
				if p.isDeleted() || !p.countable() {
					continue
				}
				truePos[p] = visible
				visible += p.length
			}
			for i, pos := range idx.positions {
				tp, ok := truePos[pos.p]
				if !ok {
					t.Fatalf("seed %d step %d: xml position %d references a removed item", seed, k, i)
				}
				if tp != pos.index {
					t.Fatalf("seed %d step %d: xml position %d claims %d, actual %d",
						seed, k, i, pos.index, tp)
				}
			}
		}
	}
}

// Guard against the whole file being vacuous. Every test above returns early when no index is
// published, so a change that stopped publishing — a guard condition, a disabled cache, a fixture
// that collapses to one item — would leave all six passing while inspecting nothing.
func TestReadIndexPathsAreNotVacuous(t *testing.T) {
	if idxInspections == 0 {
		t.Fatal("no non-empty read index was ever inspected; the per-path tests validated nothing")
	}
	t.Logf("READ_INDEX_PATHS non-empty index inspections=%d", idxInspections)
}

// ---------------------------------------------------------------- from read_index_density_test.go
func TestNearestReadPositionUsesSortedBoundaries(t *testing.T) {
	a, b, c := &itemStruct{}, &itemStruct{}, &itemStruct{}
	positions := []listReadPosition{
		{p: a, index: 10},
		{p: b, index: 20},
		{p: c, index: 40},
	}
	tests := []struct {
		index Number
		want  *itemStruct
	}{
		{index: 0, want: a},
		{index: 10, want: a},
		{index: 15, want: a}, // ties prefer the predecessor
		{index: 16, want: b},
		{index: 20, want: b},
		{index: 30, want: b}, // ties prefer the predecessor
		{index: 31, want: c},
		{index: 100, want: c},
	}
	for _, test := range tests {
		got, ok := nearestReadPosition(positions, test.index)
		if !ok {
			t.Fatalf("nearestReadPosition(%d) did not find a position", test.index)
		}
		if got.p != test.want {
			t.Fatalf("nearestReadPosition(%d) returned index %d, want item at another boundary",
				test.index, got.index)
		}
	}
	if _, ok := nearestReadPosition(nil, 10); ok {
		t.Fatal("nearestReadPosition on an empty index reported a match")
	}
}

func TestNearestReadPositionMatchesLinearForUnevenContent(t *testing.T) {
	positions := make([]listReadPosition, 256)
	nextIndex := Number(3)
	for i := range positions {
		positions[i] = listReadPosition{p: &itemStruct{}, index: nextIndex}
		// Deliberately alternate tiny and large visible spans. The interpolation probe is only a
		// predictor; uneven ContentAny and ContentString runs must still get the exact answer.
		nextIndex += 1 + (i*97)%211
	}
	for index := Number(0); index < nextIndex+100; index++ {
		want := positions[0]
		wantDistance := index - want.index
		if wantDistance < 0 {
			wantDistance = -wantDistance
		}
		for _, candidate := range positions[1:] {
			distance := index - candidate.index
			if distance < 0 {
				distance = -distance
			}
			if distance < wantDistance {
				want = candidate
				wantDistance = distance
			}
		}

		got, ok := nearestReadPosition(positions, index)
		if !ok || got.p != want.p {
			t.Fatalf("index %d returned boundary %d, want %d", index, got.index, want.index)
		}
	}
}

func TestListReadIndexSamplesPhysicalItems(t *testing.T) {
	_, arr := idxFixture(17, 1024)
	itemOrdinals := make(map[*itemStruct]int)
	itemCount := 0
	for p := arr.startItem(); p != nil; p = p.right {
		if p.isDeleted() || !p.countable() {
			t.Fatal("fixture unexpectedly contains an unindexable item")
		}
		itemOrdinals[p] = itemCount
		itemCount++
	}
	if itemCount < 64 {
		t.Fatalf("fixture collapsed to %d items and does not exercise a dense index", itemCount)
	}

	index := buildListReadIndex(arr)
	wantPositions := (itemCount - 1) / listReadIndexStride
	if len(index.positions) != wantPositions {
		t.Fatalf("built %d positions for %d physical items, want %d",
			len(index.positions), itemCount, wantPositions)
	}
	for i, position := range index.positions {
		wantOrdinal := (i + 1) * listReadIndexStride
		if got := itemOrdinals[position.p]; got != wantOrdinal {
			t.Fatalf("position %d samples physical item %d, want %d", i, got, wantOrdinal)
		}
	}
}

func TestLargeListReadIndexDefersBuildUntilReuse(t *testing.T) {
	_, arr := idxFixture(29, deferListReadIndexBuildItems+128)
	if index := arr.readIndex.Load(); index != nil {
		t.Fatalf("new list already has read index %p", index)
	}

	_ = arr.Get(arr.GetLength() / 2)
	if index := arr.readIndex.Load(); index != primedListReadIndex {
		t.Fatalf("first indexed read published %p, want priming sentinel", index)
	}

	_ = arr.Get(arr.GetLength() / 3)
	index := arr.readIndex.Load()
	if index == nil || index == primedListReadIndex || index == buildingListReadIndex || len(index.positions) == 0 {
		t.Fatalf("second indexed read did not publish a dense index: %#v", index)
	}

	arr.Insert(arr.GetLength()/4, ArrayAny{"invalidate"})
	if index := arr.readIndex.Load(); index != nil {
		t.Fatalf("mutation retained read-index state %p", index)
	}

	want := arr.ToArray()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for read := 0; read < 128; read++ {
				at := (worker*997 + read*31) % len(want)
				if got := arr.Get(at); got != want[at] {
					t.Errorf("worker %d: Get(%d) = %v, want %v", worker, at, got, want[at])
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	index = arr.readIndex.Load()
	if index == nil || index == primedListReadIndex || index == buildingListReadIndex || len(index.positions) == 0 {
		t.Fatalf("concurrent reuse did not publish a dense index: %#v", index)
	}
}

// ---------------------------------------------------------------- from read_index_rebuild_budget_test.go
// Allocation ceiling for the read-after-write cycle.
//
// WHY THIS SHAPE OF TEST. Every mutation invalidates the immutable read index, so a read that
// follows a write cannot be served from it. What that read then costs is a design choice: it may
// rebuild the whole snapshot, or defer and walk uncached. This test does not care which — it
// guards how much the cycle ALLOCATES, because that is set by sampling density and deferral policy,
// which are exactly the knobs most likely to be turned for a steady-state benchmark win.
//
// The name says "rebuild" for the cycle it measures, not for a mechanism it requires. Under the
// current design a large list's post-mutation read only PRIMES — it publishes a sentinel and walks
// uncached, so no snapshot is allocated for the next write to discard, and this cycle costs 215
// bytes at both 20k and 100k items. A design that rebuilt eagerly would also be permitted, provided
// it stayed under the ceiling.
//
// That already happened once. A densification to one position per four items made quiescent repeated
// reads about 40x faster and simultaneously took this cycle from 5,929 to 401,647 bytes at 100k
// items — a 68x increase on the pattern a collaborative editor actually runs, apply-update then
// re-render, which never collects the steady-state win because it rebuilds every time. The
// steady-state benchmark showed only the win; nothing in the suite showed the cost.
//
// WHY IT IS RELIABLE. TotalAlloc is cumulative bytes allocated and is unaffected by when the
// collector runs, so this measures the same number on a busy laptop and an idle server. It needs no
// quiet host, which matters because the finding it encodes was originally invisible to timing on
// the machine available at the time.
//
// WHY A CEILING RATHER THAN A COMPARISON. There is no second implementation to diff against at run
// time, and pinning the exact figure would fail on every legitimate tuning change. The ceiling is
// set well above any reasonable capped design and well below the uncapped one that was rejected, so
// it permits tuning and refuses the specific regression that was measured.

// readIndexRebuildCeiling is deliberately loose. The current deferring design costs 215 B per
// cycle; a position-capped design would cost roughly 33KB regardless of document size; the rejected
// uncapped one cost 82KB at 20k items and 402KB at 100k. Anything under this bound is a design
// whose post-write read cost does not grow with the document, which is the property being defended
// — not a particular constant.
const readIndexRebuildCeiling = 64 * 1024

func measureRebuildBytesPerCycle(t *testing.T, items int) float64 {
	t.Helper()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(7)
	for i := 0; i < items; i++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
	}
	// Read once before measuring, so the loop below measures steady post-mutation reads rather
	// than the one-off cost of a cold cache.
	_ = arr.Get(items / 2)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const cycles = 100
	for c := 0; c < cycles; c++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{c}) // invalidates the index
		_ = arr.Get(rng(arr.GetLength()))               // cannot use the index
	}
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / cycles
}

func TestReadIndexRebuildStaysWithinAllocationBudget(t *testing.T) {
	for _, items := range []int{20_000, 100_000} {
		perCycle := measureRebuildBytesPerCycle(t, items)
		if perCycle > readIndexRebuildCeiling {
			t.Errorf("read-after-write allocates %.0f B/cycle at %d items, ceiling %d B.\n"+
				"The post-write read is scaling with document size. A steady-state read benchmark "+
				"will not show this: that path is served from the cache, while this one cannot be "+
				"and is what an editor applying updates and re-rendering actually runs.",
				perCycle, items, readIndexRebuildCeiling)
		}
		t.Logf("READ_INDEX_REBUILD items=%d alloc=%.0f B/cycle (ceiling %d)",
			items, perCycle, readIndexRebuildCeiling)
	}
}

// TestReadIndexRebuildDoesNotScaleWithDocumentSize states the property directly rather than through
// a constant. A design whose rebuild allocation is proportional to item count passes the ceiling at
// a small size and fails it at a large one; this fails at any size, and says why.
func TestReadIndexRebuildDoesNotScaleWithDocumentSize(t *testing.T) {
	small := measureRebuildBytesPerCycle(t, 20_000)
	large := measureRebuildBytesPerCycle(t, 100_000)

	// Five times the items must not mean five times the post-write read allocation.
	//
	// The bound is 3.0 rather than something tight around the current measurement. The deferring
	// design measures 1.00 here, and a bound just above that would forbid perfectly reasonable
	// alternatives: an eager design under sqrt sizing measured 1.92, because the position count
	// genuinely doubles across this range. Calibrating to whatever the present implementation
	// happens to achieve turns a property test into a change detector, and the first person to make
	// a legitimate tuning change loosens or deletes it. 3.0 admits every bounded design and still
	// calls the rejected uncapped one, which was 4.9, without ambiguity.
	if small > 0 && large/small > 3.0 {
		t.Errorf("rebuild allocation grew %.1fx (%.0f B -> %.0f B) for a 5x larger document; "+
			"the read index is sampling proportionally to item count, so every read that follows "+
			"a write pays for the whole document", large/small, small, large)
	}
	t.Logf("READ_INDEX_REBUILD_SCALING 20k=%.0f B 100k=%.0f B ratio=%.2f", small, large, large/small)
}

// ---------------------------------------------------------------- from read_index_stale_regression_test.go
// A published read index must never outlive the item layout it describes. A read inside a
// transaction can publish after the mutation boundary has already invalidated the old index; when
// cleanup later merges two Items, that merge must invalidate the newly-published snapshot itself.
func TestReadIndexNeverOutlivesItemLayout(t *testing.T) {
	for seed := 0; seed < 200; seed++ {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		rng := markerLCG(uint32(seed*2654435761 + 1))

		for i := 0; i < 400+seed; i++ {
			arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
		}
		_ = arr.Get(arr.GetLength() / 2)

		Transact(doc, func(*Transaction) {
			for k := 0; k < 6; k++ {
				arr.Insert(arr.GetLength(), ArrayAny{9000 + k})
				if arr.GetLength() > 3 {
					_ = arr.Get(arr.GetLength() - 2)
				}
			}
		}, nil, true)

		assertReadIndexMatchesLayout(t, arr)
		// ToArray walks from Start and never consults the index, so it is an independent public
		// oracle for every indexed result.
		want := arr.ToArray()
		for i := 0; i < len(want); i++ {
			if got := arr.Get(i); fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want[i]) {
				t.Fatalf("seed %d: Get(%d) = %v, want %v (list length %d)",
					seed, i, got, want[i], len(want))
			}
		}
	}
}

// assertReadIndexMatchesLayout checks the cache's own invariant directly. Value comparisons can
// miss an orphaned entry until a later lookup happens to approach it from the revealing direction.
func assertReadIndexMatchesLayout(t *testing.T, typ abstractType) {
	t.Helper()
	cache := listReadIndexPointer(typ)
	if cache == nil {
		return
	}
	index := cache.Load()
	if index == nil {
		return
	}
	if index == buildingListReadIndex {
		t.Fatal("read-index build sentinel survived synchronous transaction cleanup")
	}
	positions := make(map[*itemStruct]Number, len(index.positions))
	visible := Number(0)
	for item := typ.startItem(); item != nil; item = item.right {
		if !item.isDeleted() && item.countable() {
			positions[item] = visible
			visible += item.length
		}
	}
	for i, position := range index.positions {
		actual, live := positions[position.p]
		if !live || actual != position.index {
			t.Fatalf("read position %d points to live=%v index=%d, claimed=%d",
				i, live, actual, position.index)
		}
	}
}
