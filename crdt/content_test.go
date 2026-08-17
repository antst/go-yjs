package crdt

import (
	"bytes"
	"testing"
)

// ---------------------------------------------------------------- from content_any_allowlist_regression_test.go
// content_any_allowlist_regression_test.go covers the Copilot review finding on
// PR #2: the ContentAny primitive-type allowlist in typeListInsertGenerics
// (list inserts) and typeMapSet (map sets) excluded integer widths that
// WriteAny encodes via its ContentAny (writeAny) path. Specifically int32 and
// the unsigned ints uint8/uint16/uint32 were missing, so inserting/setting a
// value of one of those Go types errored "unexpected content type" even though
// WriteAny encodes it byte-identically to lib0 (as a JS number).
//
// The two allowlists now mirror WriteAny's ContentAny path exactly:
//
//	WriteAny case            -> routes to       -> allowed here
//	int8/int16/int32         writeAnyNumber        yes
//	Number (int)             writeAnyNumber        yes
//	uint8/uint16/uint32      writeAnyNumber        yes
//	int64                    tag 122 (bigint)      yes
//	float32/float64          writeAnyNumber        yes
//	bool                     tag 120/121           yes
//	string                   tag 119               yes
//	Object / ArrayAny        tag 118 / 117         yes
//	null / undefined         tag 126 / 127         yes (NullType/UndefinedType)
//	[]uint8                  ContentBinary         no (own content case)
//	*Doc                     ContentDoc            no (own content case)
//	IAbstractType            ContentType           no (own content case)
//
// WriteAny has NO uint64 case (it falls through to default → undefined/127), so
// uint64 is deliberately NOT in the allowlist.
//
// Each subtest asserts (a) NO "unexpected content type" error on the
// list-insert and map-set paths, and (b) the value round-trips through
// encode→apply→read with its numeric value preserved. These integers encode as
// JS numbers (tag 125 for |n| ≤ bits31), so the decoder hands them back as
// Number (the established int→number read-back semantics) — the assertion
// compares numeric VALUE, normalizing the read-back Go type.

// numericValueOf normalizes a decoded any-value to float64 so a round-tripped
// integer can be compared by value regardless of which numeric Go type the
// decoder reconstructs (Number/int for small ints, float64 for non-integral or
// large values).
func numericValueOf(t *testing.T, v interface{}) float64 {
	t.Helper()
	switch n := v.(type) {
	case Number: // == int
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	default:
		t.Fatalf("read-back value %#v has non-numeric type %T", v, v)
		return 0
	}
}

// allowlistIntegerCases is every integer value whose Go type was newly added to
// the ContentAny allowlist by this fix. Each must be accepted by both the
// list-insert and the map-set paths.
func allowlistIntegerCases() []struct {
	name string
	val  interface{}
	want float64
} {
	return []struct {
		name string
		val  interface{}
		want float64
	}{
		{"int32", int32(123456), 123456},
		{"uint8", uint8(200), 200},
		{"uint16", uint16(40000), 40000},
		{"uint32", uint32(3000000000), 3000000000},
	}
}

// TestContentAnyAllowlist_ListInsert_NoError asserts the list-insert primitive
// path (typeListInsertGenerics) accepts each newly-allowed integer type without
// the "unexpected content type in insert operation" error. It calls the
// function directly inside a transaction so the returned error is observed (the
// public YArray.Insert swallows it inside Transact).
func TestContentAnyAllowlist_ListInsert_NoError(t *testing.T) {
	for _, tc := range allowlistIntegerCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("g", true, defaultGCFilter, nil, false)
			arr := doc.GetArray("a")
			if arr == nil {
				t.Fatal("setup: GetArray returned nil")
			}
			var insertErr error
			Transact(doc, func(trans *Transaction) {
				insertErr = typeListInsertGenerics(trans, arr, 0, ArrayAny{tc.val})
			}, nil, true)
			if insertErr != nil {
				t.Fatalf("typeListInsertGenerics(%T) errored: %v", tc.val, insertErr)
			}
		})
	}
}

// TestContentAnyAllowlist_MapSet_NoError asserts the map-set primitive path
// (typeMapSet) accepts each newly-allowed integer type without the "unexpected
// content type" error. It calls the function directly to observe the returned
// error (the public YMap.Set swallows it inside Transact).
func TestContentAnyAllowlist_MapSet_NoError(t *testing.T) {
	for _, tc := range allowlistIntegerCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("g", true, defaultGCFilter, nil, false)
			m := doc.GetMap("m")
			if m == nil {
				t.Fatal("setup: GetMap returned nil")
			}
			var setErr error
			Transact(doc, func(trans *Transaction) {
				setErr = typeMapSet(trans, m, tc.name, tc.val)
			}, nil, true)
			if setErr != nil {
				t.Fatalf("typeMapSet(%T) errored: %v", tc.val, setErr)
			}
		})
	}
}

// TestContentAnyAllowlist_RoundTrip drives each newly-allowed integer type
// through the full public path — YArray.Insert / YMap.Set → encode → apply into
// a fresh doc → read — for both the V1 and V2 update codecs, and asserts the
// numeric value is preserved on read-back.
func TestContentAnyAllowlist_RoundTrip(t *testing.T) {
	codecs := []struct {
		name   string
		encode func(*Doc) []uint8
		apply  func(*Doc, []uint8)
	}{
		{
			name:   "v1",
			encode: func(d *Doc) []uint8 { return mustBytes(EncodeStateAsUpdate(d, nil)) },
			apply:  func(d *Doc, u []uint8) { _ = ApplyUpdate(d, u, "remote") },
		},
		{
			name:   "v2",
			encode: func(d *Doc) []uint8 { return mustBytes(EncodeStateAsUpdateV2(d, nil)) },
			apply:  func(d *Doc, u []uint8) { _ = ApplyUpdateV2(d, u, "remote") },
		},
	}

	for _, codec := range codecs {
		codec := codec
		t.Run(codec.name, func(t *testing.T) {
			for _, tc := range allowlistIntegerCases() {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					src := newDoc("g", true, defaultGCFilter, nil, false)
					src.GetArray("a").Insert(0, ArrayAny{tc.val})
					src.GetMap("m").Set(tc.name, tc.val)

					update := codec.encode(src)
					if len(update) == 0 {
						t.Fatalf("encode produced empty update for %T", tc.val)
					}

					dst := newDoc("g", true, defaultGCFilter, nil, false)
					codec.apply(dst, update)

					gotArr := dst.GetArray("a").Get(0)
					if got := numericValueOf(t, gotArr); got != tc.want {
						t.Errorf("array round-trip %T: value not preserved: want %v got %v (read type %T)", tc.val, tc.want, got, gotArr)
					}

					gotMap := typeMapGet(dst.GetMap("m"), tc.name)
					if got := numericValueOf(t, gotMap); got != tc.want {
						t.Errorf("map round-trip %T: value not preserved: want %v got %v (read type %T)", tc.val, tc.want, got, gotMap)
					}
				})
			}
		})
	}
}

// ---------------------------------------------------------------- from content_any_arena_test.go
func TestSingleAnyItemArenaEncodesLikeBulkInsert(t *testing.T) {
	t.Parallel()

	values := ArrayAny{
		0, "one", true, Null, 4.5, false, "seven", 8,
		9, "ten", true, 12, "thirteen", false, 15, "sixteen",
	}

	piecewise := newDoc("single-any-piecewise", false, defaultGCFilter, nil, false, WithClientID(1))
	pa := piecewise.GetArray("a")
	Transact(piecewise, func(*Transaction) {
		for _, value := range values {
			pa.Insert(pa.GetLength(), ArrayAny{value})
		}
	}, nil, true)

	bulk := newDoc("single-any-piecewise", false, defaultGCFilter, nil, false, WithClientID(1))
	ba := bulk.GetArray("a")
	ba.Insert(0, values)

	items := 0
	for item := pa.start; item != nil; item = item.right {
		items++
	}
	if items != 1 {
		t.Fatalf("piecewise primitive run left %d items, want 1", items)
	}
	if got := pa.ToArray(); len(got) != len(values) {
		t.Fatalf("piecewise array length = %d, want %d", len(got), len(values))
	}

	piecewiseV1, err := EncodeStateAsUpdate(piecewise, nil)
	if err != nil {
		t.Fatal(err)
	}
	bulkV1, err := EncodeStateAsUpdate(bulk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(piecewiseV1, bulkV1) {
		t.Fatalf("single-value arena changed V1 encoding\npiecewise %x\n bulk     %x", piecewiseV1, bulkV1)
	}

	piecewiseV2, err := EncodeStateAsUpdateV2(piecewise, nil)
	if err != nil {
		t.Fatal(err)
	}
	bulkV2, err := EncodeStateAsUpdateV2(bulk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(piecewiseV2, bulkV2) {
		t.Fatalf("single-value arena changed V2 encoding\npiecewise %x\n bulk     %x", piecewiseV2, bulkV2)
	}
}

// ---------------------------------------------------------------- from content_copy_identity_test.go
// What Copy and Splice must preserve, and what they are free to change.
//
// yjs shares the array on copy() and allocates fresh containers on splice() and
// mergeWith() (slice and concat both return new arrays). Two separate properties
// hide inside that: element reference identity, which callers can observe, and
// container identity, which in JavaScript is only the mechanism that keeps one
// content from writing through another's array.
//
// This port cannot copy the mechanism and stay fast — cloning both halves on
// every split is what made front-deletion quadratic (394 ms at 16,000 elements,
// 1.5 GB copied; see perf_bench_delete_test.go). It copies the GUARANTEE instead:
// every array a contentAny can reach through another contentAny is
// capacity-bounded, so append — which unlike concat writes in place whenever
// spare capacity exists — is forced to reallocate. The tests below pin that
// guarantee directly rather than pinning container identity as a proxy for it.
func TestContentAnyCopyPreservesElementIdentity(t *testing.T) {
	t.Parallel()

	nested := MakeObject("n", 1)
	content := newContentAny(ArrayAny{nested, "middle", nested})
	copyContent := content.copyContent().(*contentAny)
	copyContent.arr[1] = "shared-container"
	if content.arr[1] != "shared-container" {
		t.Fatal("contentAny.Copy did not preserve the source array reference")
	}

	copyNested, ok := copyContent.arr[0].(Object)
	if !ok || !copyNested.sameRef(nested) {
		t.Fatal("contentAny.Copy deep-copied a nested value")
	}
}

func TestContentJSONCopyPreservesElementIdentity(t *testing.T) {
	t.Parallel()

	nested := MakeObject("n", 1)
	content := newContentJSON(ArrayAny{nested, "middle", nested})
	copyContent := content.copyContent().(*contentJSON)
	copyContent.arr[1] = "shared-container"
	if content.arr[1] != "shared-container" {
		t.Fatal("contentJSON.Copy did not preserve the source array reference")
	}

	copyNested, ok := copyContent.arr[0].(Object)
	if !ok || !copyNested.sameRef(nested) {
		t.Fatal("contentJSON.Copy deep-copied a nested value")
	}
}

// Splice keeps element references and produces halves that cover the right
// values. Deep-copying here would break redo, which relies on a redone item
// seeing the same nested Object the original held.
func TestContentAnySpliceKeepsElementIdentity(t *testing.T) {
	t.Parallel()

	nested := MakeObject("n", 1)
	content := newContentAny(ArrayAny{"a", nested, "c", "d"})
	right := content.spliceContent(2).(*contentAny)

	if len(content.arr) != 2 || len(right.arr) != 2 {
		t.Fatalf("split at 2 of a 4-element run gave %d/%d", len(content.arr), len(right.arr))
	}
	if content.arr[0] != "a" || right.arr[0] != "c" || right.arr[1] != "d" {
		t.Fatalf("split moved the wrong values: left=%v right=%v", content.arr, right.arr)
	}
	leftNested, ok := content.arr[1].(Object)
	if !ok {
		t.Fatalf("left half element 1 is %T, want Object", content.arr[1])
	}
	leftNested.Set("shared", true)
	if got := nested.GetOr("shared"); got != true {
		t.Fatal("contentAny.Splice deep-copied a nested value")
	}
}

// THE GUARANTEE, half against half. Growing one half of a split must never be
// visible through the other. This is what container identity bought in
// JavaScript, and it is the property that makes sharing one backing array safe.
//
// The fixture is deliberately len == cap. That is the only shape that reaches
// the reslicing path AND leaves the hazard live: with spare capacity the
// retention rule copies both halves out and the test proves nothing, and with a
// tight left half there is nowhere for an append to land. Dropping the capacity
// bound in spliceArrayAny turns the append below into a write over the right
// half's first element.
func TestContentAnySplitHalvesCannotBeWrittenThroughEachOther(t *testing.T) {
	t.Parallel()

	backing := make(ArrayAny, 8)
	for i := range backing {
		backing[i] = i
	}
	content := newContentAny(backing)
	right := content.spliceContent(4).(*contentAny)

	if cap(content.arr) != 4 || cap(right.arr) != 4 {
		t.Fatalf("fixture did not reach the reslicing path: left cap %d, right cap %d, want 4 and 4",
			cap(content.arr), cap(right.arr))
	}

	content.mergeContentWith(newContentAny(ArrayAny{"left-grew"}))
	if right.arr[0] != 4 {
		t.Fatalf("appending to the left half overwrote the right half's first element: %v", right.arr[0])
	}

	right.mergeContentWith(newContentAny(ArrayAny{"right-grew"}))
	if got := content.arr[len(content.arr)-1]; got != "left-grew" {
		t.Fatalf("appending to the right half overwrote the left half's tail: %v", got)
	}
}

// THE GUARANTEE, copy against copy. Two copies of one content share its array,
// so without the capacity bound in Copy they both append into the same free slot
// and the second silently rewrites the first's last element.
//
// This one needs spare capacity to be reachable, which is exactly what
// ReadContentAny (built by repeated append) and the tail fast path in
// typeListInsertGenericsAfter produce. yjs is immune by construction: concat
// always allocates, so no yjs content can grow into another's array. append does
// not, which is why the bound has to be added by hand here.
func TestContentAnyCopiesCannotBeWrittenThroughEachOther(t *testing.T) {
	t.Parallel()

	backing := make(ArrayAny, 0, 16)
	for i := 0; i < 8; i++ {
		backing = append(backing, i)
	}
	content := newContentAny(backing)
	if cap(content.arr) <= len(content.arr) {
		t.Fatalf("fixture has no spare capacity (len %d cap %d); the hazard is unreachable",
			len(content.arr), cap(content.arr))
	}

	first := content.copyContent().(*contentAny)
	second := content.copyContent().(*contentAny)
	first.mergeContentWith(newContentAny(ArrayAny{"first-grew"}))
	second.mergeContentWith(newContentAny(ArrayAny{"second-grew"}))

	if got := first.arr[len(first.arr)-1]; got != "first-grew" {
		t.Fatalf("the second copy's append overwrote the first copy's tail: %v", got)
	}
	if got := content.arr[len(content.arr)-1]; got != 7 {
		t.Fatalf("a copy's append was visible in the source: %v", got)
	}
}

// The split must stay allocation-free for the balanced case, which is the case
// every mid-run item split takes. Cloning a half is the fallback that bounds
// retention, not the normal path — if this starts allocating, the quadratic is
// back and only the largest delete benchmarks would notice.
func TestContentAnySpliceDoesNotCopyABalancedSplit(t *testing.T) {
	// No t.Parallel: AllocsPerRun panics when called from a parallel test.
	arr := make(ArrayAny, 4096)
	for i := range arr {
		arr[i] = i
	}

	allocs := testing.AllocsPerRun(100, func() {
		c := &contentAny{arr: arr}
		_ = c.spliceContent(len(arr) / 2)
	})
	// One allocation is the returned *contentAny itself; a copied half is 32 KB more.
	if allocs > 1 {
		t.Fatalf("a balanced split allocated %.0f times; it must reslice, not clone", allocs)
	}
}

// The retention bound: a half that pins far more than it holds is copied out, so
// a long run whose pieces are mostly deleted cannot keep the whole array alive.
func TestContentAnySpliceCopiesOutASmallHalf(t *testing.T) {
	t.Parallel()

	arr := make(ArrayAny, 4096)
	for i := range arr {
		arr[i] = i
	}
	content := &contentAny{arr: arr}
	right := content.spliceContent(8).(*contentAny)

	// Capacity does not answer this: a three-index reslice reports the half's own
	// length as its capacity while still pinning the whole array. Element address
	// identity is what distinguishes a copied-out half from a resliced one.
	if &content.arr[0] == &arr[0] {
		t.Fatal("the 8-element left half still points into the 4096-element array; it must be copied out")
	}
	if &right.arr[0] != &arr[8] {
		t.Fatal("the 4088-element right half was copied though it holds most of what it pins")
	}
}

// The two thresholds are not interchangeable and the left one is the tighter of
// the two on purpose: a bounded left half can never reuse the capacity it pins,
// so pinning is pure waste for it, while the tail will spend that capacity on its
// next append. This case sits between the two — 30% of what it pins — so it is
// copied out under the left's rule and would be retained under the tail's.
// Without it, loosening the left threshold to match the tail's silently doubles
// retention behind every left half and no test notices.
func TestContentAnySpliceUsesTheTighterThresholdForTheLeftHalf(t *testing.T) {
	t.Parallel()

	arr := make(ArrayAny, 4096)
	for i := range arr {
		arr[i] = i
	}
	content := &contentAny{arr: arr}
	_ = content.spliceContent(1200)

	if &content.arr[0] == &arr[0] {
		t.Fatal("a left half holding 30% of what it pins was retained; the left threshold " +
			"must stay tighter than the tail's, because a bounded half cannot reuse that capacity")
	}
}

// THE TAIL MUST KEEP ITS SPARE CAPACITY. This is a performance invariant with a
// correctness-shaped failure mode, so it is pinned rather than commented.
//
// The right half of a split is the tail item's content, and the tail is what the
// append fast path in TypeListInsertGenericsAfter grows in place. Capacity-bound
// it and every push following a delete reallocates the whole run. That is not
// hypothetical: an earlier version of spliceArrayAny bounded both halves, and a
// memory profile of a push-interleaved-with-delete workload put 99.46% of all
// allocated bytes in TypeListInsertGenericsAfter — the quadratic had moved from
// the delete path to the push path rather than gone away. Restoring the tail's
// capacity took that workload from 567 ms and 2.1 GB to 5.0 ms and 2.7 MB at
// 16,000 elements.
//
// Bounding the tail is safe to omit because the tail can only grow into the
// region past the original length, and no other live view reaches there: left
// halves are bounded in spliceArrayAny and copies are bounded in Copy.
func TestContentAnySpliceLeavesTheTailAppendable(t *testing.T) {
	t.Parallel()

	arr := make(ArrayAny, 0, 4096)
	for i := 0; i < 2048; i++ {
		arr = append(arr, i)
	}
	content := newContentAny(arr)
	right := content.spliceContent(1).(*contentAny)

	if cap(right.arr) != 4095 {
		t.Fatalf("the tail half has cap %d, want 4095: bounding it forces every push "+
			"after a delete to copy the whole run", cap(right.arr))
	}
	if cap(content.arr) > 2 {
		t.Fatalf("the one-element left half has cap %d; it must be bounded and copied out",
			cap(content.arr))
	}

	before := &right.arr[0]
	right.mergeContentWith(newContentAny(ArrayAny{"grew"}))
	if &right.arr[0] != before {
		t.Fatal("appending to the tail reallocated; its spare capacity was not preserved")
	}
}

func TestEmbedAndFormatCopyShareNestedValues(t *testing.T) {
	t.Parallel()

	nested := MakeObject("n", 1)
	embed := newContentEmbed(nested).copyContent().(*contentEmbed)
	embedObject := embed.embed.(Object)
	if !embedObject.sameRef(nested) {
		t.Fatal("contentEmbed.Copy deep-copied its payload")
	}

	format := newContentFormat("meta", nested).copyContent().(*contentFormat)
	formatObject := format.value.(Object)
	if !formatObject.sameRef(nested) {
		t.Fatal("contentFormat.Copy deep-copied its value")
	}
}

// ---------------------------------------------------------------- from content_doc_subdocs_test.go
// Full-review finding: ContentDoc.Copy must rebuild a FRESH doc from guid+opts (yjs
// createDocFromOpts), not deep-copy the live *Doc. shouldLoad = opts.shouldLoad ||
// opts.autoLoad || false (opts carry no shouldLoad) = autoLoad. Teeth: pre-fix the
// autoLoad=false copy kept ShouldLoad=true (and on an integrated subdoc, hung/crashed).
func TestContentDocCopyResetsShouldLoadToAutoLoad(t *testing.T) {
	// autoLoad=false: a local subdoc defaults ShouldLoad=true; the copy must be false.
	d := newDoc("sub", true, defaultGCFilter, nil, false)
	if !d.ShouldLoad {
		t.Fatal("precondition: a local doc should default ShouldLoad=true")
	}
	cp := newContentDoc(d).copyContent().(*contentDoc)
	if cp.doc.ShouldLoad {
		t.Errorf("Copy of autoLoad=false subdoc: ShouldLoad=true, want false (yjs createDocFromOpts)")
	}
	if cp.doc.Guid != "sub" {
		t.Errorf("Copy lost guid: %q", cp.doc.Guid)
	}

	// autoLoad=true: the copy must be load-pending (ShouldLoad=true).
	da := newDoc("sub2", true, defaultGCFilter, nil, true)
	cpa := newContentDoc(da).copyContent().(*contentDoc)
	if !cpa.doc.ShouldLoad {
		t.Errorf("Copy of autoLoad=true subdoc: ShouldLoad=false, want true")
	}
}

// Full-pass finding: the 'subdocs' event must NOT fire on a transaction that touched
// no subdocs (yjs gates the block on non-empty added/removed/loaded). A plain Y.Text
// edit fired a spurious empty event before the gate; a real subdoc op still fires one.
func TestSubdocsEventGatedOnNonEmpty(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	var events int
	doc.On("subdocs", NewObserverHandler(func(...interface{}) { events++ }))

	txt.Insert(0, "hello", Object{}) // no subdocs involved
	if events != 0 {
		t.Errorf("plain text edit fired %d spurious 'subdocs' events, want 0", events)
	}

	doc.GetMap("m").Set("s", newDoc("sub", false, defaultGCFilter, nil, false))
	if events != 1 {
		t.Errorf("subdoc insert fired %d 'subdocs' events, want 1", events)
	}
}

// Subdoc-lifecycle audit finding: an integrated subdoc must inherit the parent's
// ClientID (yjs cleanupTransactions: subdoc.clientID = doc.clientID), so subdoc edits
// are authored under the parent's id (wire parity with Yjs). Pre-fix the subdoc kept
// its own random newDoc clientID.
func TestSubdocInheritsParentClientID(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(4242))
	m := doc.GetMap("m")
	sub := newDoc("sub", false, defaultGCFilter, nil, false) // its own random ClientID
	m.Set("s", sub)

	var got *Doc
	for k := range doc.subDocs {
		got = k.(*Doc)
	}
	if got == nil {
		t.Fatal("subdoc not integrated")
	}
	if got.ClientID != doc.ClientID {
		t.Errorf("integrated subdoc ClientID = %d, want parent's %d", got.ClientID, doc.ClientID)
	}
}

// Regression for the final-pass finding: Doc.Destroy ranged the live SubDocs map
// while subDoc.Destroy() re-added a reconstructed replacement into it, so Go's
// undefined add-during-range re-visited/re-destroyed replacements non-deterministically
// (N subdocs -> up to >N 'removed' events). Snapshotting the subdocs first makes it
// deterministic: destroying a parent with N subdocs reports exactly N removed.
func TestDocDestroyWithManySubdocsDeterministic(t *testing.T) {
	const n = 40
	for trial := 0; trial < 5; trial++ {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		m := doc.GetMap("m")
		for i := 0; i < n; i++ {
			m.Set("s"+itoa(i), newDoc("sub"+itoa(i), false, defaultGCFilter, nil, false))
		}
		if len(doc.subDocs) != n {
			t.Fatalf("trial %d: setup subdocs=%d, want %d", trial, len(doc.subDocs), n)
		}
		removed := 0
		doc.On("subdocs", NewObserverHandler(func(v ...interface{}) {
			if ev, ok := v[0].(Object); ok {
				if r, ok := ev.GetOr("removed").(Set); ok {
					removed += len(r)
				}
			}
		}))
		doc.Destroy()
		if removed != n {
			t.Errorf("trial %d: destroy reported %d removed, want exactly %d (non-deterministic re-visit?)", trial, removed, n)
		}
	}
}

// Regression for the full-review CRASH finding: redo of a subdoc insertion calls
// RedoItem -> ContentDoc.Copy on an INTEGRATED subdoc (a parent<->child cycle). The
// old copystructure.Copy hung on the cycle / returned nil and the redo panicked
// (nil deref at content_doc.go). With Copy rebuilding a fresh doc, redo must succeed
// and re-add the subdoc. This is the integrated case the ShouldLoad unit test missed.
func TestRedoOfSubdocInsertionDoesNotCrash(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	origins := NewSet()
	origins.Add(nil)
	um := newUndoManager(arr, 500, func(*itemStruct) bool { return true }, origins)

	arr.Push(ArrayAny{newDoc("subguid", false, defaultGCFilter, nil, false)})
	if arr.GetLength() != 1 || len(doc.subDocs) != 1 {
		t.Fatalf("setup: arr len=%d subdocs=%d, want 1/1", arr.GetLength(), len(doc.subDocs))
	}

	um.Undo()
	if arr.GetLength() != 0 {
		t.Fatalf("after undo: arr len=%d, want 0", arr.GetLength())
	}

	if r := um.Redo(); r == nil { // must not panic/hang; must redo
		t.Fatal("Redo returned nil — subdoc insertion not redone")
	}
	if arr.GetLength() != 1 {
		t.Errorf("after redo: arr len=%d, want 1 (subdoc re-added)", arr.GetLength())
	}
	if sub, ok := arr.Get(0).(*Doc); !ok || sub.Guid != "subguid" {
		t.Errorf("redone item is not the subdoc: %#v", arr.Get(0))
	}
}

// US3 / FR-008..FR-011 (work item 1.1). Nested subdocuments must be fed into the
// existing subdoc infrastructure: ContentDoc.Integrate registers the subdoc (and
// loads it when ShouldLoad), ContentDoc.Delete removes/destroys it (or withdraws a
// same-transaction add), and ContentDoc.GC stays a no-op. Verified against
// yjs@13.6.31 src/structs/ContentDoc.js (integrate/delete/gc + createDocFromOpts).

func subdocsEventCapture(parent *Doc, target *Doc) (added, loaded, removed *bool) {
	a, l, r := false, false, false
	parent.On("subdocs", NewObserverHandler(func(v ...interface{}) {
		ev := v[0].(Object)
		if s, ok := ev.GetOr("added").(Set); ok && s.Has(target) {
			a = true
		}
		if s, ok := ev.GetOr("loaded").(Set); ok && s.Has(target) {
			l = true
		}
		if s, ok := ev.GetOr("removed").(Set); ok && s.Has(target) {
			r = true
		}
	}))
	return &a, &l, &r
}

func TestSubdocIntegrateAddsAndLoads(t *testing.T) {
	parent := newDoc("parent", false, nil, nil, false, WithClientID(1))
	arr := parent.GetArray("a")
	sub := newDoc("sub", true, defaultGCFilter, nil, false) // ShouldLoad defaults true (yjs Doc ctor)
	added, loaded, _ := subdocsEventCapture(parent, sub)

	arr.Insert(0, ArrayAny{sub})

	if !parent.GetSubdocs().Has(sub) {
		t.Error("subdoc not registered in parent.GetSubdocs() after integrate (ContentDoc.Integrate missing)")
	}
	if !*added {
		t.Error("subdoc not in the 'added' set of the subdocs event")
	}
	if !*loaded {
		t.Error("subdoc with ShouldLoad not in the 'loaded' set")
	}
}

func TestSubdocDeleteRemovesAndDestroys(t *testing.T) {
	parent := newDoc("parent", false, nil, nil, false, WithClientID(1))
	arr := parent.GetArray("a")
	sub := newDoc("sub", true, defaultGCFilter, nil, false)
	arr.Insert(0, ArrayAny{sub})
	if !parent.GetSubdocs().Has(sub) {
		t.Fatal("setup: subdoc not added")
	}

	destroyed := false
	sub.On("destroy", NewObserverHandler(func(v ...interface{}) { destroyed = true }))
	_, _, removed := subdocsEventCapture(parent, sub)

	arr.Delete(0, 1)

	if parent.GetSubdocs().Has(sub) {
		t.Error("subdoc still in parent.GetSubdocs() after delete")
	}
	if !*removed {
		t.Error("subdoc not in the 'removed' set of the subdocs event")
	}
	if !destroyed {
		t.Error("subdoc not destroyed on removal")
	}
}

func TestSubdocSameTxnAddDeleteWithdrawn(t *testing.T) {
	parent := newDoc("parent", false, nil, nil, false, WithClientID(1))
	arr := parent.GetArray("a")
	sub := newDoc("sub", true, defaultGCFilter, nil, false)
	added, _, removed := subdocsEventCapture(parent, sub)

	parent.Transact(func(trans *Transaction) {
		arr.Insert(0, ArrayAny{sub})
		arr.Delete(0, 1)
	}, nil)

	if parent.GetSubdocs().Has(sub) {
		t.Error("withdrawn subdoc should not be registered")
	}
	if *added {
		t.Error("same-txn add+delete should NOT report the subdoc as added (withdrawn)")
	}
	if *removed {
		t.Error("same-txn add+delete should NOT report the subdoc as removed (withdrawn)")
	}
}

// FR-009 decode path: ReadContentDoc must set a decoded subdoc's ShouldLoad to
// autoLoad (yjs createDocFromOpts: shouldLoad || autoLoad), unlike a locally-created
// doc which defaults ShouldLoad=true.
func TestSubdocDecodeShouldLoadFollowsAutoLoad(t *testing.T) {
	for _, autoLoad := range []bool{true, false} {
		parent := newDoc("parent", false, nil, nil, false, WithClientID(1))
		sub := newDoc("sub", true, defaultGCFilter, nil, autoLoad)
		parent.GetArray("a").Insert(0, ArrayAny{sub})

		update, err := EncodeStateAsUpdate(parent, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		fresh := newDoc("parent", false, nil, nil, false, WithClientID(2))
		_ = ApplyUpdate(fresh, update, nil)

		var decoded *Doc
		for sd := range fresh.GetSubdocs() {
			decoded = sd.(*Doc)
		}
		if decoded == nil {
			t.Fatalf("autoLoad=%v: no decoded subdoc registered", autoLoad)
		}
		if decoded.ShouldLoad != autoLoad {
			t.Errorf("autoLoad=%v: decoded subdoc ShouldLoad=%v, want %v", autoLoad, decoded.ShouldLoad, autoLoad)
		}
	}
}

// FR-011: ContentDoc.GC is a no-op and must not touch the subdocument's content.
func TestContentDocGCIsNoOp(t *testing.T) {
	sub := newDoc("sub", true, defaultGCFilter, nil, false)
	sub.GetText("t").Insert(0, "hello", Object{})
	c := newContentDoc(sub)

	c.gcContent(nil) // must be a no-op (no panic, no mutation)

	got := c.contentValues()
	if len(got) != 1 || got[0].(*Doc) != sub {
		t.Errorf("ContentDoc.GC altered content: %#v", got)
	}
	if s := sub.GetText("t").ToString(); s != "hello" {
		t.Errorf("subdoc content changed by GC: %q", s)
	}
}

// ---------------------------------------------------------------- from content_embed_arena_test.go
func TestContentEmbedItemArenaKeepsPointersStableAndTailBounded(t *testing.T) {
	doc := newDoc("embed-item-arena", false, defaultGCFilter, nil, false, WithClientID(1))
	const count = 200
	storages := make([]*itemWithContentEmbed, count)
	for i := range storages {
		storage := doc.allocateEmbedItemStorage()
		storage.item.id.Clock = i
		storage.content.embed = i
		storage.item.content = &storage.content
		storages[i] = storage
	}

	for i, storage := range storages {
		if storage.item.id.Clock != i || storage.content.embed != i ||
			storage.item.content != &storage.content {
			t.Fatalf("embed storage %d moved or was overwritten", i)
		}
	}
	if len(doc.embedItemBlock) > 16 || len(doc.embedItemBlock)-doc.embedItemBlockUsed >= 16 {
		t.Fatalf("embed arena tail = %d/%d, want bounded tail",
			len(doc.embedItemBlock), doc.embedItemBlockUsed)
	}
}

// ---------------------------------------------------------------- from content_format_arena_test.go
func TestContentFormatItemArenaKeepsPointersStableAndTailsBounded(t *testing.T) {
	t.Parallel()

	doc := newDoc("format-item-arena", false, defaultGCFilter, nil, false, WithClientID(1))
	const count = 200
	storages := make([]*itemWithContentFormat, count)
	origins := make([]*ID, count)
	for i := range storages {
		storage := doc.allocateFormatItemStorage()
		storage.item.id.Clock = i
		storage.content.key = mapKey(i)
		storage.item.content = &storage.content
		storages[i] = storage

		origin := doc.allocateItemOriginStorage()
		*origin = GenID(7, i)
		origins[i] = origin
	}

	for i, storage := range storages {
		if storage.item.id.Clock != i || storage.content.key != mapKey(i) ||
			storage.item.content != &storage.content {
			t.Fatalf("format storage %d moved or was overwritten", i)
		}
		if *origins[i] != GenID(7, i) {
			t.Fatalf("origin storage %d moved or was overwritten: %+v", i, *origins[i])
		}
	}
	if len(doc.formatItemBlock) != 8 || len(doc.formatItemBlock)-doc.formatItemBlockUsed >= 8 {
		t.Fatalf("format-item arena tail = %d/%d, want bounded tail", len(doc.formatItemBlock), doc.formatItemBlockUsed)
	}
	if len(doc.itemOriginBlock) != 32 || len(doc.itemOriginBlock)-doc.itemOriginBlockUsed >= 32 {
		t.Fatalf("item-origin arena tail = %d/%d, want bounded tail", len(doc.itemOriginBlock), doc.itemOriginBlockUsed)
	}
}

func TestLocalFormatItemArenaGrowsWithoutSpeculativeTail(t *testing.T) {
	t.Parallel()

	doc := newDoc("format-item-growth", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	trans := newTransaction(doc, nil, true, true)
	first := newLocalFormatItem(trans, nil, nil, text, "a", true)
	if first == nil || first.content == nil || len(doc.formatItemBlock) != 1 || doc.formatItemBlockUsed != 1 {
		t.Fatalf("first format item block = %d/%d, want 1/1", len(doc.formatItemBlock), doc.formatItemBlockUsed)
	}
	second := newLocalFormatItem(trans, nil, nil, text, "b", true)
	if second == nil || second.content == nil || len(doc.formatItemBlock) != 2 || doc.formatItemBlockUsed != 1 {
		t.Fatalf("second format item block = %d/%d, want 2/1", len(doc.formatItemBlock), doc.formatItemBlockUsed)
	}

	// Origins likewise start at one slot rather than retaining a 32-ID block
	// for the first item that cannot point directly at its left neighbour's ID.
	first.length = 2
	third := newLocalFormatItem(trans, first, nil, text, "c", true)
	if third.origin == nil || len(doc.itemOriginBlock) != 1 || doc.itemOriginBlockUsed != 1 {
		t.Fatalf("first item-origin block = %d/%d, want 1/1", len(doc.itemOriginBlock), doc.itemOriginBlockUsed)
	}
}

func TestReservedFormatItemStorageKeepsEarlierBlocksStable(t *testing.T) {
	t.Parallel()

	doc := newDoc("format-item-reserve", false, defaultGCFilter, nil, false, WithClientID(1))
	first := doc.allocateFormatItemStorage()
	first.content = contentFormat{key: "first", value: true}
	first.item.content = &first.content

	const reserved = 40
	doc.reserveFormatItemStorage(reserved)
	for i := 0; i < reserved; i++ {
		storage := doc.allocateFormatItemStorage()
		storage.content = contentFormat{key: mapKey(i), value: i}
		storage.item.content = &storage.content
	}

	if first.content.key != "first" || first.item.content != &first.content {
		t.Fatal("reserving a format-item block moved or overwrote an earlier item")
	}
	if len(doc.formatItemBlock) != reserved || doc.formatItemBlockUsed != reserved {
		t.Fatalf("reserved format-item block len/used = %d/%d, want %d/%d",
			len(doc.formatItemBlock), doc.formatItemBlockUsed, reserved, reserved)
	}
}

// ---------------------------------------------------------------- from content_format_v2_test.go
// TestV2ContentFormatKeyColumnSymmetry locks in the fix for the
// ReadContentFormat key-column desync under the V2 codec.
//
// ContentFormat.Write emits its key via UpdateEncoder.WriteKey. Under V2,
// WriteKey advances the keyClock column (and, on a cache miss, also appends to
// the string column). If ReadContentFormat read the key back via ReadString
// (string column only), the keyClock column entry would be left unconsumed and
// every subsequent ReadKey on the same decoder would read a stale keyClock,
// silently mis-pairing keys. The fix reads the key via ReadKey so the keyClock
// column stays aligned.
//
// This test writes a ContentFormat key followed by a second key (as a later
// struct's parentSub would be encoded), then reads them back in order: with the
// old ReadString path the second ReadKey desyncs; with ReadKey both round-trip.
func TestV2ContentFormatKeyColumnSymmetry(t *testing.T) {
	enc := newDefaultUpdateEncoderV2()

	// 1) A ContentFormat: writes "bold" via WriteKey (keyClock col + string col)
	//    plus a JSON value into the rest buffer.
	cf := newContentFormat("bold", true)
	if err := cf.writeContent(enc, 0); err != nil {
		t.Fatalf("ContentFormat.Write: %v", err)
	}
	// 2) A following struct's key, also written via WriteKey.
	if err := enc.writeKey("name"); err != nil {
		t.Fatalf("WriteKey: %v", err)
	}

	buf := enc.toBytes()
	dec := newUpdateDecoderV2(buf)

	content, err := readContentFormat(dec)
	if err != nil {
		t.Fatalf("ReadContentFormat: %v", err)
	}
	gotFmt, ok := content.(*contentFormat)
	if !ok {
		t.Fatalf("expected *ContentFormat, got %T", content)
	}
	if gotFmt.key != "bold" {
		t.Fatalf("ContentFormat key: want %q got %q", "bold", gotFmt.key)
	}

	// The real invariant: ReadContentFormat must go through ReadKey, which both
	// consumes the keyClock column entry AND registers the key in the decoder's
	// key cache (v2.keys). A ReadString-based implementation would do neither, so
	// the cache would still be empty here and the keyClock column would be left
	// with an unconsumed entry. Asserting the cache grew pins ReadKey usage —
	// matching Yjs's readContentFormat (decoder.readKey()).
	if len(dec.keys) != 1 || dec.keys[0] != "bold" {
		t.Fatalf("ReadContentFormat must register the key via ReadKey "+
			"(keyClock column + key cache); got cache %v", dec.keys)
	}

	// And the following key still decodes correctly, with the cache now holding
	// both keys in keyClock order.
	gotKey, err := dec.readKey()
	if err != nil {
		t.Fatalf("following ReadKey after ContentFormat: %v", err)
	}
	if gotKey != "name" {
		t.Fatalf("following key: want %q got %q", "name", gotKey)
	}
	if len(dec.keys) != 2 || dec.keys[1] != "name" {
		t.Fatalf("key cache after both reads: want [bold name], got %v", dec.keys)
	}
}

// TestV2FormattedTextThenMapConverges is an end-to-end guard for the same desync:
// a document that mixes a formatted Y.Text (ContentFormat keys) and a Y.Map
// (parentSub keys read via ReadKey) must survive a V2 encode -> apply round trip.
func TestV2FormattedTextThenMapConverges(t *testing.T) {
	src := newDoc("guid", true, defaultGCFilter, nil, false)
	src.Transact(func(trans *Transaction) {
		txt := src.GetText("rich")
		txt.Insert(0, "Hello World", Object{})
		txt.Format(0, 5, MakeObject("bold", true))
		txt.Format(6, 5, MakeObject("italic", true, "color", "#ff0000"))

		m := src.GetMap("meta")
		m.Set("author", "alice")
		m.Set("title", "doc")
	}, nil)

	update := mustBytes(EncodeStateAsUpdateV2(src, nil))

	dst := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(dst, update, nil)

	if got := dst.GetText("rich").ToString(); got != "Hello World" {
		t.Errorf("text content: want %q got %q", "Hello World", got)
	}
	meta := dst.GetMap("meta")
	if got := meta.Get("author"); got != "alice" {
		t.Errorf("map author: want %q got %v", "alice", got)
	}
	if got := meta.Get("title"); got != "doc" {
		t.Errorf("map title: want %q got %v", "doc", got)
	}
}

// sanity: ensure the V2 encoder column layout used above is self-consistent for a
// bare key pair (guards against accidental column-prefix drift in ToUint8Array).
func TestV2BareKeyPairRoundTrip(t *testing.T) {
	enc := newDefaultUpdateEncoderV2()
	_ = enc.writeKey("k1")
	_ = enc.writeKey("k2")
	dec := newUpdateDecoderV2(enc.toBytes())

	k1, err := dec.readKey()
	if err != nil || k1 != "k1" {
		t.Fatalf("k1: got %q err %v", k1, err)
	}
	k2, err := dec.readKey()
	if err != nil || k2 != "k2" {
		t.Fatalf("k2: got %q err %v", k2, err)
	}
}

// ---------------------------------------------------------------- from content_type_arena_test.go
func TestTypeItemArenaKeepsPublishedPointersStable(t *testing.T) {
	doc := newDoc("type-item-arena", false, defaultGCFilter, nil, false, WithClientID(1))
	items := make([]*itemWithContentType, 200)
	for i := range items {
		items[i] = doc.allocateTypeItemStorage()
		items[i].item.id.Clock = i
		items[i].content.value = NewYArray()
	}
	for i, storage := range items {
		if storage.item.id.Clock != i || storage.content.value == nil {
			t.Fatalf("storage %d moved or was overwritten", i)
		}
	}
	if len(doc.typeItemBlock) > 32 || len(doc.typeItemBlock)-doc.typeItemBlockUsed > 31 {
		t.Fatalf("final block len/used = %d/%d, want bounded tail",
			len(doc.typeItemBlock), doc.typeItemBlockUsed)
	}
}

// ---------------------------------------------------------------- from content_type_cascade_test.go
// US2 / FR-005..FR-007 (work item 1.2). Deleting a nested collaborative type MUST
// cascade: ContentType.Delete tombstones every child (linked-list + map), and in a
// gc=true doc ContentType.GC replaces those children with GC structs. Verified
// against yjs@13.6.31 src/structs/ContentType.js (delete + gc).
//
// Regression: before the fix ContentType.Delete/GC were no-ops, so deleting a
// nested YMap/YArray/YText left its children live in the struct store (divergent
// state, leaked content, wrong re-encode).

// findStructByID returns the struct in the store that covers id.Clock, or nil.
func findStructByID(store *structStore, id ID) abstractStruct {
	value, _ := findStruct(store, id)
	return value
}

func buildArrayWithNestedMap(t *testing.T, gc bool) (*Doc, *YArray, *YMap) {
	t.Helper()
	var doc *Doc
	if gc {
		doc = newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(1))
	} else {
		doc = newDoc("guid", false, nil, nil, false, WithClientID(1))
	}
	arr := doc.GetArray("a")
	nested := NewYMap(nil)
	arr.Insert(0, ArrayAny{nested})
	nested.Set("k1", "v1")
	nested.Set("k2", "v2")
	nested.Set("k3", "v3")
	return doc, arr, nested
}

func childItemsOf(m *YMap) []*itemStruct {
	items := make([]*itemStruct, 0, len(m.getMap()))
	for _, it := range m.getMap() {
		items = append(items, it)
	}
	return items
}

func TestContentTypeDeleteCascadeTombstonesChildren(t *testing.T) {
	_, arr, nested := buildArrayWithNestedMap(t, false)

	children := childItemsOf(nested)
	if len(children) != 3 {
		t.Fatalf("setup: expected 3 child items, got %d", len(children))
	}

	arr.Delete(0, 1) // delete the nested map

	for _, it := range children {
		if !it.isDeleted() {
			t.Errorf("child %v not tombstoned after deleting parent — ContentType.Delete cascade missing", *it.getID())
		}
	}
	if got := arr.GetLength(); got != 0 {
		t.Errorf("array length after delete = %d, want 0", got)
	}
}

func TestContentTypeGCCascadeReplacesChildren(t *testing.T) {
	doc, arr, nested := buildArrayWithNestedMap(t, true)

	childIDs := make([]ID, 0, 3)
	for _, it := range childItemsOf(nested) {
		childIDs = append(childIDs, *it.getID())
	}

	arr.Delete(0, 1) // gc=true: delete triggers tryGcDeleteSet → ContentType.GC cascade

	for _, id := range childIDs {
		s := findStructByID(doc.store, id)
		if s == nil {
			t.Errorf("child %v vanished from store entirely", id)
			continue
		}
		if _, isGC := s.(*gcStruct); !isGC {
			t.Errorf("child %v = %T after gc, want *GC — ContentType.GC cascade missing", id, s)
		}
	}

	// Re-encode and apply to a fresh gc=true doc: must round-trip to an empty array.
	update, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate: %v", err)
	}
	fresh := newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdate(fresh, update, nil)
	if got := fresh.GetArray("a").GetLength(); got != 0 {
		t.Errorf("round-tripped array length = %d, want 0", got)
	}
}

// The nested-array tests below exercise the `_start` linked-list branches of
// ContentType.Delete/GC (a YMap's children live in `_map`; a YArray's live in
// `_start`), covering both loops.

func buildArrayWithNestedArray(t *testing.T, gc bool) (*Doc, *YArray, *YArray) {
	t.Helper()
	var doc *Doc
	if gc {
		doc = newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(1))
	} else {
		doc = newDoc("guid", false, nil, nil, false, WithClientID(1))
	}
	outer := doc.GetArray("a")
	nested := NewYArray()
	outer.Insert(0, ArrayAny{nested})
	nested.Insert(0, ArrayAny{"x"})
	nested.Insert(1, ArrayAny{"y"})
	nested.Insert(2, ArrayAny{"z"})
	return doc, outer, nested
}

func startItemsOf(typ abstractType) []*itemStruct {
	var items []*itemStruct
	for it := typ.startItem(); it != nil; it = it.right {
		items = append(items, it)
	}
	return items
}

func TestContentTypeDeleteCascadeNestedArrayStart(t *testing.T) {
	_, outer, nested := buildArrayWithNestedArray(t, false)
	children := startItemsOf(nested)
	if len(children) == 0 {
		t.Fatal("setup: nested array has no _start items")
	}
	outer.Delete(0, 1)
	for _, it := range children {
		if !it.isDeleted() {
			t.Errorf("nested _start child %v not tombstoned — ContentType.Delete _start cascade missing", *it.getID())
		}
	}
}

func TestContentTypeGCCascadeNestedArrayStart(t *testing.T) {
	doc, outer, nested := buildArrayWithNestedArray(t, true)
	var ids []ID
	for _, it := range startItemsOf(nested) {
		ids = append(ids, *it.getID())
	}
	outer.Delete(0, 1)
	for _, id := range ids {
		s := findStructByID(doc.store, id)
		if s == nil {
			t.Errorf("nested _start child %v vanished from store", id)
			continue
		}
		if _, isGC := s.(*gcStruct); !isGC {
			t.Errorf("nested _start child %v = %T after gc, want *GC", id, s)
		}
	}
}

func TestContentTypeDeleteStartMergeBranch(t *testing.T) {
	_, outer, nested := buildArrayWithNestedArray(t, false)
	nested.Delete(0, 1) // pre-delete first element → tombstoned, below beforeState
	outer.Delete(0, 1)  // exercises the _start merge branch; must not panic
	if got := outer.GetLength(); got != 0 {
		t.Errorf("outer length after delete = %d, want 0", got)
	}
}

// TestContentTypeDeleteMergeBranch exercises the `else if clock < beforeState`
// branch of ContentType.Delete: a child deleted in a prior transaction is already
// tombstoned and below the current beforeState, so deleting the parent routes it to
// trans.MergeStructs (not a re-delete). The absent-client case (no beforeState entry
// → 0) is the Go map zero-value default, verified in T019 against yjs `(… || 0)`.
func TestContentTypeDeleteMergeBranch(t *testing.T) {
	_, arr, nested := buildArrayWithNestedMap(t, false)

	// pre-delete one child in its own transaction so it is already tombstoned and
	// below the parent-delete transaction's beforeState.
	nested.Delete("k1")
	var preDeleted *itemStruct
	for key, it := range nested.getMap() {
		if key == "k1" {
			preDeleted = it
		}
	}
	if preDeleted == nil || !preDeleted.isDeleted() {
		t.Fatalf("setup: k1 not pre-deleted")
	}

	arr.Delete(0, 1) // must not panic; merge-branch child stays deleted

	if !preDeleted.isDeleted() {
		t.Errorf("pre-deleted child unexpectedly resurrected")
	}
	if got := arr.GetLength(); got != 0 {
		t.Errorf("array length after delete = %d, want 0", got)
	}
}
