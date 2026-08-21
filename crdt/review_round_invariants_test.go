package crdt

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Invariants for the code added/changed by the 003 review round. Each test pins a specific
// behaviour that a reviewer flagged (or that the US2 value-rep layer introduced) and that the
// differential oracle cannot see: the oracle compares encoded bytes against Yjs, so it is blind
// to caller-object aliasing, Go-specific slice typing, and copy-source drift. These are the
// invariants that live *outside* the oracle's reach — hence real tests, not coverage padding.

// GetOrNull must coalesce BOTH Go-nil/absent AND the explicit Undefined sentinel, mirroring JS
// `?? null` (which coalesces null and undefined alike). Leaking Undefined through would put a
// non-Null sentinel into a format attribute and diverge the wire.
func TestGetOrNullCoalescesUndefined(t *testing.T) {
	o := newObject()
	o.Set("undef", Undefined)
	o.Set("real", "v")

	if got := o.GetOrNull("undef"); !isNull(got) {
		t.Errorf("GetOrNull(explicit Undefined) = %#v, want Null (JS `?? null` coalesces undefined too)", got)
	}
	if got := o.GetOrNull("real"); got != "v" {
		t.Errorf("GetOrNull(real) = %v, want \"v\" — coalescing must not swallow real values", got)
	}
}

// ShallowClone must share nested value references (Yjs object.assign semantics). A deep copy here
// would give nested attribute values fresh handles, so the reference-strict equalAttrs would call
// them unequal and Go would keep a redundant ContentFormat marker that Yjs drops — a wire divergence.
func TestShallowCloneSharesNestedRefs(t *testing.T) {
	inner := newObject()
	inner.Set("x", 1)
	o := newObject()
	o.Set("nested", inner)
	o.Set("flat", 1)

	c := o.ShallowClone()

	if c.GetOr("nested") != o.GetOr("nested") {
		t.Error("ShallowClone must SHARE the nested reference (object.assign); a deep copy re-introduces the redundant-format-marker divergence")
	}
	// The key slice/map must be independent: mutating the clone must not touch the original.
	c.Set("added", 2)
	if _, ok := o.Get("added"); ok {
		t.Error("ShallowClone shares the backing map — mutating the clone leaked into the original")
	}
}

func TestShallowCloneNilReceiver(t *testing.T) {
	var zero Object // nil backing store
	c := zero.ShallowClone()
	if !c.IsNil() {
		t.Error("ShallowClone of a zero Object must stay nil-ish, not panic or fabricate a store")
	}
}

// ContentDoc.Copy must rebuild from the immutable Opts snapshot captured at NewContentDoc, NOT
// from the live Doc's public fields. Reading the live fields let a later mutation of Doc.GC /
// Doc.AutoLoad drift into the copy.
func TestContentDocCopyUsesOptsSnapshot(t *testing.T) {
	sub := newDoc("sub-guid", false, defaultGCFilter, nil, true) // gc=false, autoLoad=true
	cd := newContentDoc(sub)

	// Mutate the LIVE doc's public fields after the Opts snapshot was taken.
	cd.doc.GC = true
	cd.doc.AutoLoad = false

	cp, ok := cd.copyContent().(*contentDoc)
	if !ok {
		t.Fatal("Copy did not return *ContentDoc")
	}
	if cp.doc.GC {
		t.Error("Copy read the mutated live Doc.GC; it must rebuild from the Opts snapshot (gc=false)")
	}
	if !cp.doc.AutoLoad {
		t.Error("Copy read the mutated live Doc.AutoLoad; it must rebuild from the Opts snapshot (autoLoad=true)")
	}
	if cp.doc.ShouldLoad != cp.doc.AutoLoad {
		t.Errorf("ShouldLoad=%v, want == AutoLoad=%v (createDocFromOpts parity)", cp.doc.ShouldLoad, cp.doc.AutoLoad)
	}
	if cp.doc.GUID != "sub-guid" {
		t.Errorf("GUID = %q, want preserved", cp.doc.GUID)
	}
}

// Go slice invariance: a concrete typed slice ([]*YMap) matches neither []interface{} nor
// []IAbstractType, so without the reflection path it was wrapped as ONE opaque scope and the doc
// never resolved. yjs accepts any Array<AbstractType>; every typed-slice form must work.
func TestUndoManagerAcceptsConcreteTypedSliceScope(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m1 := doc.GetMap("m1")
	m2 := doc.GetMap("m2")
	if m1 == nil || m2 == nil {
		t.Fatal("GetMap did not yield *YMap")
	}

	um := newUndoManager([]*YMap{m1, m2}, 500, nil, nil)
	defer um.Destroy()

	if len(um.scopes) != 2 {
		t.Fatalf("Scopes = %d, want 2 — a concrete []*YMap must expand element-wise, not be boxed as one scope", len(um.scopes))
	}
	if um.doc == nil {
		t.Fatal("doc unresolved from a concrete typed slice scope")
	}

	// And the manager must actually track edits through that scope.
	m1.Set("k", "v")
	if !um.CanUndo() {
		t.Error("edit through a typed-slice scope was not tracked")
	}
}

// insertText runs an in-place negation pre-pass that writes synthetic Null clears. Yjs mutates the
// caller's object too, but JS callers pass a fresh literal per op; a Go Object is a handle over
// shared storage, so a caller reusing one across ops would carry stale Null clears forward.
func TestInsertTextDoesNotMutateCallerAttributes(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")

	// Establish active formatting so the negation pre-pass has something to negate.
	txt.Insert(0, "abc", MakeObject("bold", true, "italic", true))

	attrs := MakeObject("bold", true)
	before := attrs.Len()

	txt.Insert(1, "X", attrs)

	if attrs.Len() != before {
		t.Errorf("caller attributes mutated: Len %d -> %d (negation pre-pass leaked synthetic Null clears)", before, attrs.Len())
	}
	if _, ok := attrs.Get("italic"); ok {
		t.Error("negation pre-pass wrote a synthetic `italic` clear into the CALLER's Object; reusing it for a later op would carry the clear forward")
	}
}

// A structurally-empty tracked transaction must be COMPLETELY inert for the UndoManager. The
// empty-transaction guard exists to stop a phantom StackItem being pushed, but it originally sat
// AFTER the redo-stack clear — so a no-op transaction destroyed the redo stack while adding no
// undo entry, silently losing a redo the user could still perform. Real edits are unaffected
// (they clear redo and push an item, as yjs does).
func TestEmptyTransactionPreservesRedoStack(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	um := newUndoManager(doc, 0, nil, nil) // doc-scoped: every transaction passes trackedByScope
	defer um.Destroy()

	txt.Insert(0, "abc", Object{})
	if !um.CanUndo() {
		t.Fatal("edit was not captured")
	}
	um.Undo()
	if !um.CanRedo() {
		t.Fatal("undo did not produce a redo item")
	}

	// A no-op transaction: nothing inserted, nothing deleted.
	doc.Transact(func(trans *Transaction) {}, nil)

	if !um.CanRedo() {
		t.Error("a no-op transaction cleared the redo stack; an empty transaction must be fully inert")
	}
}

// --- Defects found by the post-round code review, each reproduced before being fixed. ---

// Deleting a subdoc key from a GC-ENABLED (the default) Doc panicked. The chain: ContentDoc.Delete
// — newly implemented by this feature, a `// TODO` before it — populates trans.SubdocsRemoved;
// cleanupTransactions runs tryGcDeleteSet, which rewrites the tombstoned item's Content to
// *ContentDeleted; the SubdocsRemoved loop then calls Doc.Destroy, whose unchecked
// item.Content.(*ContentDoc) assertion blew up. Every pre-existing subdoc test builds its parent
// with gc=false, which is why the suite stayed green over a crash in default configuration.
func TestSubdocDeleteUnderGCDoesNotPanic(t *testing.T) {
	p := newDoc("parent", true, defaultGCFilter, nil, false, WithClientID(1))
	m := p.GetMap("m")
	if m == nil {
		t.Fatal("GetMap did not yield *YMap")
	}
	m.Set("s", newDoc("sub", true, defaultGCFilter, nil, false))
	m.Delete("s") // panicked: interface conversion ... is *ContentDeleted, not *ContentDoc
}

// deleteText dereferenced currPos.Right to resolve the parent when BOTH Left and Right were nil,
// which is exactly what findPosition yields on an empty Y.Text. yjs throws here too, but a JS throw
// is catchable whereas a Go nil deref kills the goroutine — so a stray Delete, or an ApplyDelta
// carrying a delete op against empty text, took down the process.
func TestDeleteOnEmptyTextDoesNotPanic(t *testing.T) {
	d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	d.GetText("t").Delete(0, 1)

	d2 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	d2.GetText("t").ApplyDelta([]EventOperator{NewDeleteDeltaOp(3)}, false)
}

// InsertAfter spliced a LOCAL copy of the slice header, so content added to an unintegrated node
// was silently discarded and the node later integrated empty. The sibling Insert always spliced
// the field itself.
func TestXmlInsertAfterKeepsPrelimContent(t *testing.T) {
	el := NewYXmlElement("div")
	el.InsertAfter(nil, ArrayAny{NewYXmlText()})
	if got := len(el.prelimContent); got != 1 {
		t.Errorf("PrelimContent = %d, want 1 — InsertAfter spliced a local copy and dropped the content", got)
	}
}

// Prelim flush used Go map iteration, whose order is randomised per run, so an identical locally
// built Y.Map / Y.XmlElement produced DIFFERENT encoded bytes run to run (measured: 4 distinct
// streams across 40 builds). Two peers building the same structure emitted different CRDT ids for
// the same logical edit. yjs is deterministic here because a JS Map is insertion-ordered.
func TestPrelimFlushIsDeterministic(t *testing.T) {
	encodings := func(build func(d *Doc)) int {
		seen := map[string]struct{}{}
		for i := 0; i < 40; i++ {
			d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			build(d)
			b, err := EncodeStateAsUpdate(d, nil)
			if err != nil {
				t.Fatal(err)
			}
			seen[string(b)] = struct{}{}
		}
		return len(seen)
	}

	if got := encodings(func(d *Doc) {
		m := NewYMap(nil)
		m.Set("k0", 0)
		m.Set("k1", 1)
		m.Set("k2", 2)
		m.Set("k3", 3)
		d.GetArray("a").Insert(0, ArrayAny{m})
	}); got != 1 {
		t.Errorf("Y.Map prelim flush produced %d distinct byte streams across 40 identical builds, want 1", got)
	}

	if got := encodings(func(d *Doc) {
		el := NewYXmlElement("div")
		el.SetAttribute("a", "1")
		el.SetAttribute("b", "2")
		el.SetAttribute("c", "3")
		d.GetXMLFragment("x").Insert(0, ArrayAny{el})
	}); got != 1 {
		t.Errorf("Y.XmlElement prelim attrs produced %d distinct byte streams across 40 identical builds, want 1", got)
	}
}

// The reaper goroutine mutates the awareness maps on a timer. They used to be EXPORTED
// (States/Meta), so any consumer reading them directly raced it — and a Go map race is a fatal,
// unrecoverable process abort, not a catchable panic. Meta had no accessor at all, so direct
// field access was the ONLY way to inspect it. They are now unexported behind GetStates()/
// GetMeta(), which copy under the mutex. This test hammers both accessors across a reaper tick
// under -race; before the change, the equivalent using the exported fields reported DATA RACE.
func TestAwarenessAccessorsAreRaceFree(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a reaper tick")
	}
	d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	aw := NewAwareness(d)
	defer aw.Destroy()

	// Seed a stale remote client so the reaper actually mutates on its next tick.
	aw.mu.Lock()
	aw.states[99] = newObject()
	aw.meta[99] = MakeObject("clock", 0, "lastUpdated", getUnixTime()-int64(OutdatedTimeout/time.Millisecond)-1000)
	aw.mu.Unlock()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				for k := range aw.GetStates() {
					_ = k
				}
				for k := range aw.GetMeta() {
					_ = k
				}
			}
		}
	}()
	time.Sleep(OutdatedTimeout/10 + 500*time.Millisecond) // cover at least one reaper tick
	close(stop)
	wg.Wait()

	if _, stillThere := aw.GetStates()[99]; stillThere {
		t.Error("stale remote client was not reaped — the tick did not run, so this test proved nothing")
	}
}

// ContentType.Copy must yield a FRESH EMPTY type (yjs `new ContentType(this.type._copy())`), not a
// deep clone. Its only caller is RedoItem, and during a redo the nested type's children are
// re-created by their own redoItem calls — so deep-cloning both double-materialises them and
// resurrects tombstoned entries the redo should leave deleted.
//
// The capture window matters: with captureTimeout 0 each op is its own stack item, so Undo only
// reverts the last op and Copy is never reached. The ops must coalesce into ONE stack item so the
// undo removes the whole nested-type insertion and the redo goes through Content.Copy.
func TestRedoOfNestedTypeDoesNotResurrectTombstones(t *testing.T) {
	d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	root := d.GetMap("root")
	if root == nil {
		t.Fatal("GetMap did not yield *YMap")
	}
	um := newUndoManager(root, 500, nil, nil)
	defer um.Destroy()

	d.Transact(func(tr *Transaction) {
		root.Set("k", NewYMap(nil))
		live, ok := root.Get("k").(*YMap)
		if !ok {
			t.Fatal("nested value is not *YMap")
		}
		live.Set("a", 1)
		live.Set("b", 2)
		live.Delete("b") // tombstoned — must STAY deleted through undo+redo
	}, nil)

	um.Undo()
	um.Redo()

	m, ok := root.Get("k").(*YMap)
	if !ok {
		t.Fatal("nested map missing after redo")
	}
	keys := m.Keys()
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "a" {
		t.Errorf("keys after undo+redo = %v, want [a] (yjs); a deep Copy resurrects the tombstoned \"b\"", keys)
	}
}

// A transaction opened by an observer must QUEUE behind the outer cleanup drain, not nest inside
// it. doc.TransCleanup was read into a local slice and appended locally, never written back — and
// since the field is only ever assigned nil, every nested transaction looked like the first, so
// cleanupTransactions recursed. Beyond the event-count divergence asserted here, the inner
// transaction's `update` was broadcast to peers BEFORE the update of the transaction that caused it.
func TestNestedTransactionQueuesInsteadOfRecursing(t *testing.T) {
	d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := d.GetText("t")

	before, after, depth, maxDepth := 0, 0, 0, 0
	d.On("beforeAllTransactions", NewObserverHandler(func(v ...interface{}) {
		before++
		depth++
		if depth > maxDepth {
			maxDepth = depth
		}
	}))
	d.On("afterAllTransactions", NewObserverHandler(func(v ...interface{}) {
		after++
		depth--
	}))
	fired := false
	d.On("afterTransaction", NewObserverHandler(func(v ...interface{}) {
		if fired {
			return
		}
		fired = true
		d.Transact(func(tr *Transaction) { d.GetArray("a").Insert(0, ArrayAny{"nested"}) }, nil)
	}))

	txt.Insert(0, "x", Object{})

	if before != 1 || after != 1 || maxDepth != 1 {
		t.Errorf("beforeAll=%d afterAll=%d maxDepth=%d, want 1/1/1 (yjs) — the nested transaction recursed instead of queueing",
			before, after, maxDepth)
	}
	if !fired {
		t.Fatal("the nested-transaction observer never ran — test proved nothing")
	}
}

// --- Undo ordering (US1 / FR-001a). Regression guards for the ordering fix. ---

// The reference's undo restoration order originates in StructStore client FIRST-INSERTION order
// and is carried through the delete set to IterateDeletedStructs. Go lost it in two places — the
// store, and the delete set's iteration — and losing EITHER makes restoration order wrong.
//
// Beyond wrongness, losing it made results NONDETERMINISTIC: the same input gave different
// documents run to run, because Go map iteration is randomised. This asserts determinism directly,
// which is the property a canonical-but-different order would still satisfy and a randomised one
// cannot.
func TestUndoRestorationIsDeterministic(t *testing.T) {
	build := func() string {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		remote := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		rtxt := remote.GetText("t")
		um := newUndoManager([]abstractType{txt}, 100000, nil, nil)
		defer um.Destroy()

		// Content from two participants, then deletions from both, so restoration order is
		// observable — with a single client it is not.
		txt.Insert(0, "a", Object{})
		rtxt.Insert(0, "z", Object{})
		if u, e := EncodeStateAsUpdate(remote, nil); e == nil {
			_ = ApplyUpdate(doc, u, nil)
		}
		txt.Insert(0, "b", Object{})
		txt.Delete(0, 2)
		um.StopCapturing()
		txt.Insert(0, "c", Object{})
		um.Undo()

		s, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(s)
	}

	first := build()
	for i := 0; i < 40; i++ {
		if got := build(); got != first {
			t.Fatalf("undo produced a different document on run %d — restoration order depends on "+
				"Go map iteration somewhere, so the same input yields different output", i+2)
		}
	}
}

// StructStore must record clients in first-insertion order, which is where the reference's order
// originates (research R1).
func TestStructStoreTracksClientInsertionOrder(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(7))
	txt := doc.GetText("t")
	txt.Insert(0, "a", Object{})

	for _, cid := range []Number{3, 11, 5} {
		peer := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(cid))
		peer.GetText("t").Insert(0, "x", Object{})
		if u, e := EncodeStateAsUpdate(peer, nil); e == nil {
			_ = ApplyUpdate(doc, u, nil)
		}
	}

	order := doc.store.orderedClients()
	if len(order) != 4 {
		t.Fatalf("ClientOrder has %d clients, want 4: %v", len(order), order)
	}
	if order[0] != 7 {
		t.Errorf("first client = %d, want 7 (the local client wrote first)", order[0])
	}
	// Not sorted: 3, 11, 5 arrived in that order and must be reported that way.
	if order[1] == 3 && order[2] == 5 && order[3] == 11 {
		t.Error("ClientOrder returned clients in NUMERIC order — it must report ARRIVAL order, " +
			"which is what the reference's Map iteration gives")
	}
}

// A delete set must iterate clients in first-insertion order too. Ordering only the store is not
// enough: IterateDeletedStructs walks the delete set, and a randomised walk there loses the order
// again at the last hop (research R9).
func TestDeleteSetPreservesClientOrder(t *testing.T) {
	ds := newDeleteSet()
	for _, c := range []Number{9, 2, 40, 1} {
		addToDeleteSet(ds, c, 0, 1)
	}
	got := ds.orderedClients()
	want := []Number{9, 2, 40, 1}
	if len(got) != len(want) {
		t.Fatalf("ClientOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ClientOrder = %v, want insertion order %v", got, want)
		}
	}
}

// IterateDeletedStructs must VISIT clients in the delete set's insertion order, not Go map order.
//
// This is layer two of the ordering fix and needs its own test: an end-to-end determinism check
// does not reach it reliably, because whether map randomisation changes the outcome depends on how
// many clients the scenario happens to put in the delete set. Asserting the visit order directly
// makes the layer testable on its own (research R9).
func TestIterateDeletedStructsVisitsInInsertionOrder(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "a", Object{})

	// Three peers, each contributing content, merged in a known order.
	for _, cid := range []Number{40, 9, 22} {
		peer := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(cid))
		peer.GetText("t").Insert(0, "x", Object{})
		if u, e := EncodeStateAsUpdate(peer, nil); e == nil {
			_ = ApplyUpdate(doc, u, nil)
		}
	}

	// A delete set naming all four clients in a deliberately NON-numeric order.
	ds := newDeleteSet()
	for _, c := range []Number{40, 1, 22, 9} {
		addToDeleteSet(ds, c, 0, 1)
	}

	var visited []Number
	seen := map[Number]bool{}
	Transact(doc, func(trans *Transaction) {
		iterateDeletedStructs(trans, ds, func(st abstractStruct) {
			c := st.getID().Client
			if !seen[c] {
				seen[c] = true
				visited = append(visited, c)
			}
		})
	}, nil, true)

	want := []Number{40, 1, 22, 9}
	if len(visited) != len(want) {
		t.Fatalf("visited %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited %v, want the delete set's INSERTION order %v — a Go map walk here "+
				"loses the order at the last hop, even with the store ordered", visited, want)
		}
	}
}

// Merging must preserve first-touch order across the merged sets, as the reference's
// mergeDeleteSets does — it iterates each source's Map and `set`s into the target.
func TestMergeDeleteSetsPreservesFirstTouchOrder(t *testing.T) {
	a := newDeleteSet()
	addToDeleteSet(a, 50, 0, 1)
	addToDeleteSet(a, 4, 0, 1)
	b := newDeleteSet()
	addToDeleteSet(b, 4, 5, 1)
	addToDeleteSet(b, 33, 0, 1)

	got := mergeDeleteSets([]*deleteSet{a, b}).orderedClients()
	want := []Number{50, 4, 33}
	if len(got) != len(want) {
		t.Fatalf("merged ClientOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merged ClientOrder = %v, want first-touch order %v", got, want)
		}
	}
}

// A relative position anchored to a ROOT type that is not a Y.Map crashed the process.
//
// The root-type lookup used doc.GetMap(tname), which forces the YMap constructor; for a root
// Y.Text, Doc.Get returns an error ("already defined with a different constructor"), GetMap turns
// that into nil, and the next line dereferences it. The reference uses doc.get(tname) — the
// generic getter defaulting to AbstractType, which returns whatever is registered under the name.
//
// This is reachable from ordinary use: a position at the very end of a root Y.Text has no anchor
// item, so it encodes by NAME and takes exactly this branch.
func TestRelativePositionOnNonMapRootDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name string
		get  func(d *Doc) abstractType
	}{
		{"text", func(d *Doc) abstractType { return d.GetText("t") }},
		{"array", func(d *Doc) abstractType { return d.GetArray("a") }},
		{"xmlfragment", func(d *Doc) abstractType { return d.GetXMLFragment("x") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			typ := tc.get(doc)

			// assoc >= 0 at the end of an empty type: no anchor item, so the position is encoded
			// by type NAME and resolution takes the tname branch.
			rp := newRelativePositionFromTypeIndex(typ, 0, 0)
			if rp == nil {
				t.Fatal("could not create a relative position")
			}
			abs := CreateAbsolutePositionFromRelativePosition(rp, doc)
			if abs == nil {
				t.Fatal("position did not resolve; it anchors to a live root type")
			}
			if abs.Type != asSharedType(typ) {
				t.Errorf("resolved to a different type than the anchor — the root lookup must " +
					"return whatever type is registered, not force one kind")
			}
		})
	}
}

// --- XML tree walking (Constitution IX: stubs replaced with real implementations) ---

// NewYXmlTreeWalker returned nil and QuerySelector/QuerySelectorAll were empty bodies, so
// CreateTreeWalker and both selectors silently did nothing — exported API that looked functional
// and was not. Constitution IX forbids exactly that: a stub is worse than an absent method,
// because a caller gets silent nothing instead of a compile error.
//
// Expectations below are what the reference produces for the same document (verified against
// yjs@13.6.31 querySelector / querySelectorAll / YXmlTreeWalker).
func TestXmlTreeWalkerAndSelectors(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	if frag == nil {
		t.Fatal("GetXMLFragment did not yield *YXmlFragment")
	}

	//   <div><p/><span/></div><p/>
	div := NewYXmlElement("div")
	p1 := NewYXmlElement("p")
	span := NewYXmlElement("span")
	p2 := NewYXmlElement("p")
	frag.Insert(0, ArrayAny{div, p2})
	div.Insert(0, ArrayAny{p1, span})

	// Walker with no filter visits every element, depth-first.
	w := frag.CreateTreeWalker(nil)
	if w == nil {
		t.Fatal("CreateTreeWalker returned nil — the walker was a stub returning nil")
	}
	var names []string
	for n := w.Next(); n != nil; n = w.Next() {
		if el, ok := n.(*YXmlElement); ok {
			names = append(names, el.NodeName)
		}
	}
	want := []string{"div", "p", "span", "p"} // depth-first: div, its children, then the sibling
	if len(names) != len(want) {
		t.Fatalf("walker visited %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("walker visited %v, want depth-first order %v", names, want)
		}
	}

	// QuerySelector returns the FIRST match; QuerySelectorAll returns all of them.
	first := frag.QuerySelector("p")
	if first == nil {
		t.Fatal("QuerySelector('p') found nothing — it was an empty stub")
	}
	if el, ok := first.(*YXmlElement); !ok || el != p1 {
		t.Errorf("QuerySelector('p') returned the wrong node; it must return the first in " +
			"document order")
	}
	all := frag.QuerySelectorAll("p")
	if len(all) != 2 {
		t.Errorf("QuerySelectorAll('p') = %d nodes, want 2", len(all))
	}

	// Matching is case-insensitive, as the reference uppercases both sides.
	if frag.QuerySelector("DIV") == nil {
		t.Error("QuerySelector is case-sensitive; the reference uppercases query and nodeName")
	}
	if got := frag.QuerySelector("nosuch"); got != nil {
		t.Errorf("QuerySelector('nosuch') = %v, want nil", got)
	}
	if got := frag.QuerySelectorAll("nosuch"); len(got) != 0 {
		t.Errorf("QuerySelectorAll('nosuch') = %d nodes, want 0", len(got))
	}
}

// --- US7 residual defects, each reproduced before being fixed ---

// YXmlFragment.ToString silently DROPPED every child that is not an xmlType, where the reference
// coerces each child with String(). A Y.Map, Y.Text or scalar child vanished from the rendering
// entirely — silent data loss in a read path (FR-014).
func TestXmlToStringRendersEveryChildKind(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")

	frag.Insert(0, ArrayAny{NewYXmlElement("b")})
	frag.Insert(1, ArrayAny{"plain"})
	frag.Insert(2, ArrayAny{42})

	got := frag.ToString()
	for _, want := range []string{"<b></b>", "plain", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("ToString() = %q, missing %q — non-XML children must be coerced, not dropped", got, want)
		}
	}
}

// xmlAttrValueString rendered a binary attribute with Go's default slice formatting ("[1 2 3]")
// where the reference produces "1,2,3" (FR-014b).
func TestXmlBinaryAttributeRendering(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	el := NewYXmlElement("div")
	frag.Insert(0, ArrayAny{el})
	el.SetAttribute("data", []uint8{1, 2, 3})

	got := frag.ToString()
	if strings.Contains(got, "[1 2 3]") {
		t.Errorf("ToString() = %q — a binary attribute rendered with Go slice formatting; the "+
			"reference joins with commas", got)
	}
	if !strings.Contains(got, "1,2,3") {
		t.Errorf("ToString() = %q, want the attribute rendered as 1,2,3", got)
	}
}

// AddToScope silently DROPPED slice arguments: it skipped anything that was not a shared type, so
// a caller passing a slice got no scopes and no error. The constructor normalizes slices, so the
// shared primitive must too — the reference does it in the primitive (FR-014a).
func TestAddToScopeAcceptsSlices(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m1 := doc.GetMap("m1")
	m2 := doc.GetMap("m2")

	um := newUndoManager(m1, 500, nil, nil)
	defer um.Destroy()
	before := len(um.scopes)

	um.AddToScope([]abstractType{m2})
	if len(um.scopes) != before+1 {
		t.Errorf("AddToScope([]IAbstractType{...}) added %d scopes, want 1 — a slice argument was "+
			"silently dropped", len(um.scopes)-before)
	}

	m2.Set("k", "v")
	if !um.CanUndo() {
		t.Error("an edit through the slice-added scope was not tracked")
	}
}

// FR-015 (T063). A snapshot encoding carries NO version marker, so a cross-format decode cannot be
// detected — by this library or by the reference. This pins the ACTUAL behaviour so it cannot drift
// silently, and records that it MATCHES yjs rather than being a defect unique to this port.
//
// Verified against yjs@13.6.31: decodeSnapshotV2(v1Bytes) and decodeSnapshot(v2Bytes) both decode
// without throwing and both fail equalSnapshots against the original. Erroring here would reject
// input the reference accepts — a deviation, not a fix. The mitigation is the explicit
// DecodeSnapshotV1 / DecodeSnapshotV2 entry points plus the caller recording which it wrote.
func TestSnapshotCrossFormatDecodeMatchesReference(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "hello", Object{})
	txt.Delete(1, 1)
	snap := NewSnapshotByDoc(doc)

	v1, err := EncodeSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := EncodeSnapshotV2(snap)
	if err != nil {
		t.Fatal(err)
	}

	// The contract callers CAN rely on: each encoding round-trips exactly through its own decoder.
	back1, err := DecodeSnapshotV1(v1)
	if err != nil || !EqualSnapshots(back1, snap) {
		t.Errorf("V1 snapshot did not round-trip through DecodeSnapshotV1 (err=%v)", err)
	}
	back2, err := DecodeSnapshotV2(v2)
	if err != nil || !EqualSnapshots(back2, snap) {
		t.Errorf("V2 snapshot did not round-trip through DecodeSnapshotV2 (err=%v)", err)
	}

	// Cross-format is indistinguishable, exactly as in the reference. If a future change makes
	// this ERROR, that is a deliberate deviation and must be recorded as one — this test failing
	// is the prompt to record it, not a bug to silence.
	if cross, cerr := DecodeSnapshotV2(v1); cerr == nil && EqualSnapshots(cross, snap) {
		t.Error("V2 decoder reproduced a V1 snapshot exactly; the formats were assumed " +
			"indistinguishable, so this expectation needs revisiting")
	}
}

// T077. The tree walker was a stub whose constructor returned nil and whose Filter had the wrong
// signature, which made CreateTreeWalker / QuerySelector / QuerySelectorAll all silently do
// nothing. It now walks depth-first like the reference, so its traversal (down, right, up) and its
// nil-guards need tests — a walker that quietly returns nothing looks exactly like a document with
// no matches.
func TestXmlTreeWalkerTraversalAndSelectors(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("f")

	outer := NewYXmlElement("div")
	inner := NewYXmlElement("span")
	deep := NewYXmlElement("b")
	sibling := NewYXmlElement("span")
	frag.Insert(0, ArrayAny{outer, sibling})
	outer.Insert(0, ArrayAny{inner})
	inner.Insert(0, ArrayAny{deep})

	// QuerySelector must descend, not just scan the top level, and must be case-insensitive.
	if got := frag.QuerySelector("B"); got == nil {
		t.Error("QuerySelector did not find a depth-2 descendant; the walker is not descending")
	}
	if got := frag.QuerySelector("b"); got == nil {
		t.Error("QuerySelector is case-sensitive; the reference uppercases both sides")
	}
	if got := frag.QuerySelector("nosuchtag"); got != nil {
		t.Errorf("QuerySelector matched %v for a tag that does not exist", got)
	}

	// QuerySelectorAll must find BOTH spans — one nested, one a top-level sibling. That requires
	// the walker to come back UP out of the first subtree, the traversal step most easily broken.
	all := frag.QuerySelectorAll("span")
	if len(all) != 2 {
		t.Errorf("QuerySelectorAll(span) found %d, want 2 (one nested, one sibling) — "+
			"the walker is not returning up out of a subtree", len(all))
	}
	if got := frag.QuerySelectorAll("nosuchtag"); len(got) != 0 {
		t.Errorf("QuerySelectorAll matched %d nodes for a tag that does not exist", len(got))
	}

	// A nil filter means "match everything" (yjs's `f = () => true`), so the walker must visit
	// every element in the tree.
	w := frag.CreateTreeWalker(nil)
	if w == nil {
		t.Fatal("CreateTreeWalker(nil) returned nil")
	}
	count := 0
	for n := w.Next(); n != nil; n = w.Next() {
		count++
	}
	if count != 4 {
		t.Errorf("nil-filter walk visited %d nodes, want 4 (div, span, b, span)", count)
	}
}

// The walker's nil-guards: a nil root and a nil receiver must return nil rather than panic,
// because QuerySelector forwards whatever the constructor produced.
func TestXmlTreeWalkerNilGuards(t *testing.T) {
	if w := NewYXmlTreeWalker(nil, nil); w != nil {
		t.Error("NewYXmlTreeWalker accepted a nil root")
	}
	var nilWalker *YXmlTreeWalker
	if got := nilWalker.Next(); got != nil {
		t.Error("Next on a nil walker returned a node")
	}
}

// InsertAfter before integration must splice the FIELD. A reference that is not present must be
// reported and dropped rather than silently inserted at the front.
func TestXmlFragmentInsertAfterPrelim(t *testing.T) {
	frag := NewYXmlFragment()
	a := NewYXmlElement("a")
	b := NewYXmlElement("b")
	frag.Insert(0, ArrayAny{a})
	frag.InsertAfter(a, ArrayAny{b})
	if len(frag.prelimContent) != 2 || frag.prelimContent[0] != a || frag.prelimContent[1] != b {
		t.Fatalf("InsertAfter did not splice the prelim field: %v", frag.prelimContent)
	}
	// nil ref means "insert at the front" in the reference.
	c := NewYXmlElement("c")
	frag.InsertAfter(nil, ArrayAny{c})
	if frag.prelimContent[0] != c {
		t.Errorf("InsertAfter(nil, …) did not insert at the front: %v", frag.prelimContent)
	}
	// An unknown ref must be dropped, not inserted at index 0.
	before := len(frag.prelimContent)
	frag.InsertAfter(NewYXmlElement("ghost"), ArrayAny{NewYXmlElement("d")})
	if len(frag.prelimContent) != before {
		t.Errorf("InsertAfter with an unknown reference inserted anyway (%d -> %d)",
			before, len(frag.prelimContent))
	}
}
