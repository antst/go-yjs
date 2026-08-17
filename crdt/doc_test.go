package crdt

import (
	"fmt"
	"testing"
)

// ---------------------------------------------------------------- from doc_get_constructor_test.go
func countedTypeConstructor[T abstractType](calls *int, construct func() T) TypeConstructor {
	return func() SharedType {
		(*calls)++
		return asSharedType(construct())
	}
}

func TestDocGetConstructsRequestedTypeOnce(t *testing.T) {
	doc := newDoc("constructors", false, defaultGCFilter, nil, false, WithClientID(1))
	textCalls := 0
	textConstructor := countedTypeConstructor(&textCalls, func() *YText { return NewYText("") })
	first, err := doc.Get("text", textConstructor)
	if err != nil || textCalls != 1 {
		t.Fatalf("first Get: calls=%d err=%v", textCalls, err)
	}
	second, err := doc.Get("text", textConstructor)
	if err != nil || textCalls != 2 || second != first {
		t.Fatalf("repeat Get: calls=%d same=%v err=%v", textCalls, second == first, err)
	}

	// A generic root is created while decoding an update whose concrete type has not been requested
	// yet. Adoption must reuse the one requested instance rather than constructing another probe.
	if _, err := doc.Get("pending", newAbstractType); err != nil {
		t.Fatal(err)
	}
	arrayCalls := 0
	arrayConstructor := countedTypeConstructor(&arrayCalls, NewYArray)
	if got, err := doc.Get("pending", arrayConstructor); err != nil {
		t.Fatal(err)
	} else if _, ok := got.(*YArray); !ok || arrayCalls != 1 {
		t.Fatalf("generic adoption returned %T after %d constructor calls", got, arrayCalls)
	}

	wrongCalls := 0
	wrongConstructor := countedTypeConstructor(&wrongCalls, NewYArray)
	if _, err := doc.Get("text", wrongConstructor); err == nil || wrongCalls != 1 {
		t.Fatalf("mismatched Get: calls=%d err=%v", wrongCalls, err)
	}
}

// ---------------------------------------------------------------- from doc_get_no_alloc_test.go
// Resolving a type that already exists must not allocate.
//
// Doc.Get takes a constructor and calls it unconditionally, including on the
// path where the type is already registered and the constructed value serves
// only to compare types before being dropped. That is the hot path, not a rare
// one: the eager struct decoder resolves every named parent through it, so
// applying an update to a map-heavy document constructed and discarded one
// AbstractType, with its map, per root-parented struct. On a 4,000-key document
// those throwaway values were 29.48% of every object allocated during a connect,
// which was the largest single allocation site in the apply path — ahead of
// ReadString. Removing them took that connect from 27,891 to 19,893 allocations
// and 2.36 ms to 1.88 ms.
//
// The failure mode if this regresses is silence: the result stays correct and
// only the allocation count moves, which no correctness test looks at.
func TestResolvingAnExistingTypeDoesNotAllocate(t *testing.T) {
	// No t.Parallel: AllocsPerRun panics when called from a parallel test.
	doc := NewDoc("get-alloc", WithGC(false), WithClientID(1))
	doc.GetMap("m")
	doc.GetArray("a")
	doc.GetText("t")
	doc.GetXmlFragment("x")

	for _, tc := range []struct {
		name string
		get  func()
	}{
		{"GetMap", func() { doc.GetMap("m") }},
		{"GetArray", func() { doc.GetArray("a") }},
		{"GetText", func() { doc.GetText("t") }},
		{"GetXmlFragment", func() { doc.GetXmlFragment("x") }},
		{"getGeneric", func() { doc.getGeneric("m") }},
	} {
		if allocs := testing.AllocsPerRun(200, tc.get); allocs != 0 {
			t.Errorf("%s on an existing type allocated %.0f times; it must return the "+
				"registered type without constructing one to throw away", tc.name, allocs)
		}
	}
}

// The fast path must not swallow the generic-to-concrete migration. A type first
// created generically — which is exactly what the struct decoder does for a
// named parent it sees before the local code asks for it — has to be upgraded in
// place when a concrete getter later asks for it, keeping its contents.
func TestFastPathStillMigratesAGenericType(t *testing.T) {
	t.Parallel()

	doc := NewDoc("migrate", WithGC(false), WithClientID(1))
	generic := doc.getGeneric("m")
	if _, isGeneric := generic.(*abstractTypeBase); !isGeneric {
		t.Fatalf("getGeneric returned %T, want the generic placeholder", generic)
	}

	upgraded := doc.GetMap("m")
	ymap := upgraded
	ymap.Set("k", 1)
	if got := ymap.Get("k"); got != 1 {
		t.Fatalf("migrated map lost its contents: Get(k) = %v", got)
	}
	if again := doc.GetMap("m"); again != upgraded {
		t.Fatal("a second GetMap returned a different instance than the migrated one")
	}
}

// A conflicting concrete request must still fail rather than be short-circuited
// into returning the wrong type.
func TestFastPathStillRejectsAConflictingConstructor(t *testing.T) {
	t.Parallel()

	doc := NewDoc("conflict", WithGC(false), WithClientID(1))
	doc.GetText("shared")
	if got := doc.GetMap("shared"); got != nil {
		t.Fatalf("GetMap on a name already held by a Y.Text returned %T, want nil", got)
	}
}

// The decoder path, which is where nearly all of the waste was.
//
// The two tests above pin Doc's own getters, but the eager struct decoder
// reaches types through getGeneric rather than through them, and reverting THAT
// call site alone restores the whole regression while leaving those tests green.
// This is the guard for that, and it is a budget rather than an exact count
// because the total moves whenever anything else on the apply path changes.
//
// The separation it relies on is not marginal. Applying a map document costs a
// fixed 2.00 allocations per key more when the decoder constructs a throwaway
// AbstractType for each named parent, measured identically at every size:
//
//	keys    with the fix    reverted
//	200         4.37          6.36
//	500         4.65          6.65
//	1,000       4.84          6.83
//
// The budget sits between the two columns with roughly 14% of headroom on each
// side. If it starts failing because unrelated work added allocations to the
// apply path, raise it deliberately and record the new measurement — do not
// raise it far enough to stop separating these two columns, because at that
// point it stops testing anything.
func TestApplyingAMapDocumentStaysWithinItsAllocationBudget(t *testing.T) {
	// No t.Parallel: AllocsPerRun panics when called from a parallel test.
	const keys = 1000
	const budgetPerKey = 5.5

	src := NewDoc("budget", WithGC(false), WithClientID(1))
	m := src.GetMap("m")
	for j := 0; j < keys; j++ {
		m.Set(fmt.Sprintf("k%d", j), j)
	}
	update, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatal(err)
	}

	allocs := testing.AllocsPerRun(20, func() {
		d := NewDoc("budget", WithGC(false), WithClientID(9))
		if err := ApplyUpdate(d, update, nil); err != nil {
			t.Fatal(err)
		}
	})
	if perKey := allocs / keys; perKey > budgetPerKey {
		t.Errorf("applying a %d-key document cost %.2f allocations per key, budget %.2f. "+
			"A jump of about 2.00 per key means the decoder is constructing a throwaway "+
			"type for every named parent again; see Doc.getGeneric", keys, perKey, budgetPerKey)
	}
}

// ---------------------------------------------------------------- from doc_subdocs_destroy_review_test.go
// doc_subdocs_destroy_review_test.go reproduces FINDING C (completion) from the
// full code-review of the Go Yjs v2 codec (PR antst/y-crdt#2): newDoc never
// initialized Doc.SubDocs (a Set / map), so Doc.Destroy's subdoc-reconstruct
// path panics with "assignment to entry in nil map".
//
// Destroy, for a non-deleted integrated subdocument, reconstructs the inner Doc
// and runs Transact(parentDoc, ...) which schedules the reconstructed doc into
// trans.SubdocsAdded. cleanupTransactions (transaction.go) then does
//
//	for subdoc := range trans.SubdocsAdded { doc.SubDocs.Add(subdoc) }
//
// against the PARENT doc's SubDocs. With newDoc leaving SubDocs as the nil zero
// Set, that Add panics. (The path is currently reachable only by hand-wiring
// Doc.Item, since ContentDoc.Integrate is a TODO stub — but the panic is real
// the moment subdocs get wired, and the sibling .(bool) hardening in 8e6e93a
// already made the rest of this method panic-safe.)
//
// FIX: newDoc initializes SubDocs = NewSet(), so the Add is safe. A nil Set and
// an empty Set behave identically for every SubDocs READ (range in GetSubdocs /
// GetSubdocGuids / Destroy, and Delete) — only Add differs (nil panics) — so the
// initialization changes nothing except removing the panic.
//
// This test FAILS (panics) on the unpatched tree and PASSES after the fix.
func TestHandWiredSubdocDestroyDoesNotPanic(t *testing.T) {
	// Parent document. Its SubDocs is the map that Destroy's reconstruct path
	// writes to via cleanupTransactions.
	parent := newDoc("parent", true, defaultGCFilter, nil, false)

	// Integrate a shared type into the parent through the public API, so the
	// type's GetDoc() returns `parent`. Destroy reads
	// item.Parent.(IAbstractType).GetDoc() to pick the transaction document.
	parentType := parent.GetMap("container")
	if parentType == nil {
		t.Fatalf("setup: could not get a parent container type")
	}

	// The inner (sub) document, wired as a ContentDoc whose Item lives under the
	// parent type. It is NOT deleted, so Destroy takes the reconstruct branch.
	sub := newDoc("sub", true, defaultGCFilter, nil, false)
	content := newContentDoc(sub)
	item := newItem(
		GenID(parent.ClientID, 0),
		nil, nil, nil, nil,
		parentType, "container",
		content,
	)
	if item == nil {
		t.Fatalf("setup: NewItem returned nil")
	}
	// item.Deleted() must be false here (bit3 unset) so Destroy reconstructs and
	// schedules the new doc into the parent's SubDocs.
	if item.isDeleted() {
		t.Fatalf("setup: hand-wired item must not be deleted")
	}
	sub.item = item

	// On the unpatched tree this panics inside cleanupTransactions at
	// `parent.SubDocs.Add(reconstructed)` (nil map). With SubDocs initialized in
	// newDoc it completes cleanly.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FINDING C: hand-wired subdoc Destroy panicked: %v "+
				"(newDoc must initialize Doc.SubDocs so SubDocs.Add is safe)", r)
		}
	}()

	sub.Destroy()

	// The reconstruct path actually ran the Add: the parent now tracks exactly one
	// subdoc (the reconstructed inner Doc). This proves the test exercises the
	// nil-map write site rather than passing vacuously.
	if got := len(parent.subDocs); got != 1 {
		t.Fatalf("FINDING C: expected parent to track exactly 1 reconstructed subdoc "+
			"after Destroy, got %d", got)
	}
}

// TestNewDocInitializesSubDocs is the direct unit-level guard: a freshly
// constructed Doc must have a non-nil SubDocs so an Add never panics, while a
// READ (range/len) over it behaves exactly as it did when SubDocs was nil
// (empty / zero iterations).
func TestNewDocInitializesSubDocs(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)

	if doc.subDocs == nil {
		t.Fatalf("FINDING C: newDoc left Doc.SubDocs nil; Add would panic")
	}
	if len(doc.subDocs) != 0 {
		t.Fatalf("a fresh Doc should have zero subdocs, got %d", len(doc.subDocs))
	}
	// Reads behave identically to the nil case.
	if got := doc.GetSubdocs(); len(got) != 0 {
		t.Fatalf("GetSubdocs on a fresh Doc should be empty, got %d", len(got))
	}
	if got := doc.GetSubdocGuids(); len(got) != 0 {
		t.Fatalf("GetSubdocGuids on a fresh Doc should be empty, got %d", len(got))
	}
	// And Add no longer panics.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("SubDocs.Add panicked on a fresh Doc: %v", r)
			}
		}()
		other := newDoc("sub", true, defaultGCFilter, nil, false)
		doc.subDocs.Add(other)
	}()
	if len(doc.subDocs) != 1 {
		t.Fatalf("after Add, SubDocs should hold 1 entry, got %d", len(doc.subDocs))
	}
}
