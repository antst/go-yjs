package crdt

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- from search_marker_merge_regression_test.go
func assertSearchMarkersExact(t *testing.T, parent abstractType) {
	t.Helper()
	for _, marker := range *parent.getSearchMarker() {
		index := 0
		found := false
		for item := parent.startItem(); item != nil; item = item.right {
			if item == marker.p {
				found = true
				break
			}
			if !item.isDeleted() && item.countable() {
				index += item.length
			}
		}
		if !found {
			t.Fatalf("marker points outside the live item chain: %+v", marker)
		}
		if marker.index != index {
			t.Fatalf("marker index = %d, actual item position = %d", marker.index, index)
		}
	}
}

func TestSearchMarkersSurviveDeferredItemMerges(t *testing.T) {
	doc := newDoc("marker-merge", false, defaultGCFilter, nil, false, WithClientID(23))
	array := doc.GetArray("a")

	// Public batching deliberately leaves adjacent items unmerged until cleanup.
	// Persist markers in the middle of that run, then verify Item.MergeWith moves
	// them onto the surviving item with the exact adjusted index.
	doc.Transact(func(*Transaction) {
		for i := 0; i < 128; i++ {
			array.Insert(array.GetLength(), ArrayAny{i})
			if i > 2 {
				findMarker(array, i/2)
			}
		}
	}, nil)
	if len(array.searchMarker) == 0 {
		t.Fatal("test setup did not create a search marker")
	}
	assertSearchMarkersExact(t, array)

	want := make(ArrayAny, 128)
	for i := range want {
		want[i] = i
	}
	state := uint32(1)
	next := func(n int) int {
		state = state*1664525 + 1013904223
		return int(state % uint32(n))
	}
	for i := 0; i < 500; i++ {
		if i%3 == 0 && len(want) > 0 {
			index := next(len(want))
			array.Delete(index, 1)
			want = append(want[:index], want[index+1:]...)
		} else {
			index := next(len(want) + 1)
			value := 1000 + i
			array.Insert(index, ArrayAny{value})
			want = append(want, nil)
			copy(want[index+1:], want[index:len(want)-1])
			want[index] = value
		}
		assertSearchMarkersExact(t, array)
	}

	got := array.ToArray()
	if len(got) != len(want) {
		t.Fatalf("array length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("array[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------- from search_marker_scale_correctness_test.go
// Correctness of the large-sequence writer position accelerators.
//
// WHY THIS EXISTS, AND WHY THE REST OF THE SUITE CANNOT REPLACE IT.
//
// The writer block index activates only at 16k linked Items. Every ordinary differential cell,
// batched gate, read-cache stress, and concurrent matrix builds documents of tens to low thousands
// of Items. They therefore stay on the unchanged marker path no matter how many seeds run; seed
// volume does not control document size, so it cannot exercise this code.
//
// WHY A WRONG INDEX IS A CORRECTNESS BUG, NOT A PERFORMANCE ONE. The AVL nodes and bit6/bit7 Item
// flags are pure local state and never reach the wire: Item.Write rebuilds its info byte from
// content/origin metadata. But an indexed result decides WHERE the next insert or delete lands. A
// drifted visible sum or stale block-first pointer applies a valid operation at the wrong offset,
// leaving a structurally valid but incorrect document.
//
// WHAT IS ASSERTED HERE. An independent model is maintained alongside the document through a long
// random insert/delete sequence crossing the threshold, and full content—not merely length—is
// compared. The tree's AVL balance, physical/visible sums, anchor ownership, membership bits, and
// Doc side-table reachability are checked separately. Finally the document must survive its own
// encoding, because a wrong position can render plausibly in memory yet rebuild differently from
// bytes. The historical TestScaledMarker names remain stable because the scale-tier commands invoke
// them directly; their large-sequence responsibility now includes the block index that supersedes
// markers on the internal mutation path.

func markerScaleTarget() int {
	// The default crosses 16k (C: 80 -> 160). MARKER_SCALE_N=64000 reaches the 320 regime and
	// 256000 the 640 regime; both are far slower and belong to the scale tier rather than to PR time.
	if v := os.Getenv("MARKER_SCALE_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 20_000
}

// markerLCG is the same generator shape the performance work uses, kept local so this test's
// sequence cannot drift when a benchmark fixture is retuned.
func markerLCG(seed uint32) func(n int) int {
	state := seed
	return func(n int) int {
		state = state*1664525 + 1013904223
		if n <= 0 {
			return 0
		}
		return int(state % uint32(n))
	}
}

// TestScaledMarkerTextMatchesModel drives a YText past the scaling threshold with random inserts
// and deletes, comparing against a byte-slice model of the same sequence.
//
// The text is deliberately left UNFORMATTED. ContentFormat.integrate disables the marker cache
// entirely by setting the slice to nil, so a formatted text would abandon the very path under test
// — the scaled cache only ever applies to large unformatted sequences, which is precisely the case
// the change targets.
func TestScaledMarkerTextMatchesModel(t *testing.T) {
	target := markerScaleTarget()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	rng := markerLCG(0x5eed)

	model := make([]byte, 0, target)
	letters := "abcdefghijklmnopqrstuvwxyz"

	// Phase 1: grow past the threshold with random-position inserts.
	for len(model) < target {
		idx := rng(len(model) + 1)
		ch := letters[rng(len(letters))]
		txt.Insert(idx, string(ch), Object{})
		model = append(model, 0)
		copy(model[idx+1:], model[idx:])
		model[idx] = ch
	}

	if got := txt.ToString(); got != string(model) {
		t.Fatalf("after growth: content diverged from model (len want %d got %d)\n%s",
			len(model), len(got), firstDiff(string(model), got))
	}

	_, indexAfterGrowth := ownedListPositionIndex(txt)
	if indexAfterGrowth == nil || indexAfterGrowth.items < buildListPositionIndexItems {
		t.Fatalf("position index was not active at length=%d items=%d; this test is vacuous",
			txt.Length(), listItemCount(txt))
	}
	blocksAfterGrowth := len(indexAfterGrowth.anchors)
	validateListPositionTree(t, indexAfterGrowth, txt)
	validateDocPositionIndexEntries(t, doc)

	// Phase 2: mixed inserts and deletes, held above the threshold so the block tree remains active
	// while every split, integration, deletion, and cleanup merge updates it.
	ops := target / 2
	for i := 0; i < ops; i++ {
		if rng(3) == 0 && len(model) > 16_500 {
			idx := rng(len(model))
			n := 1 + rng(4)
			if idx+n > len(model) {
				n = len(model) - idx
			}
			txt.Delete(idx, n)
			model = append(model[:idx], model[idx+n:]...)
		} else {
			idx := rng(len(model) + 1)
			ch := letters[rng(len(letters))]
			txt.Insert(idx, string(ch), Object{})
			model = append(model, 0)
			copy(model[idx+1:], model[idx:])
			model[idx] = ch
		}
	}

	got := txt.ToString()
	if got != string(model) {
		t.Fatalf("after mixed ops: content diverged from model (len want %d got %d)\n%s",
			len(model), len(got), firstDiff(string(model), got))
	}
	_, textIndex := ownedListPositionIndex(txt)
	if textIndex == nil {
		t.Fatal("position index fell back during mixed text operations")
	}
	validateListPositionTree(t, textIndex, txt)
	validateDocPositionIndexEntries(t, doc)

	// The document must rebuild identically from its own bytes. A drifted index can yield a
	// structure that renders correctly in memory but encodes an item order that decodes differently.
	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	if rt := fresh.GetText("t").ToString(); rt != got {
		t.Fatalf("round-trip diverged (len want %d got %d)\n%s", len(got), len(rt), firstDiff(got, rt))
	}

	t.Logf("POSITION_INDEX_TEXT len=%d items=%d blocks=%d(after growth) %d(final)",
		txt.Length(), listItemCount(txt), blocksAfterGrowth, len(textIndex.anchors))
}

// TestScaledMarkerArrayMatchesModel repeats the check on YArray. It is a separate surface, not a
// duplicate: YArray carries ContentAny rather than ContentString, so it never touches the
// string-split backing or the append buffer, and its items merge under different conditions. The
// marker bookkeeping is shared, which is exactly why both consumers of it need checking.
func TestScaledMarkerArrayMatchesModel(t *testing.T) {
	target := markerScaleTarget()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(0xA55E)

	model := make([]int, 0, target)
	for len(model) < target {
		idx := rng(len(model) + 1)
		v := rng(1 << 20)
		arr.Insert(idx, ArrayAny{v})
		model = append(model, 0)
		copy(model[idx+1:], model[idx:])
		model[idx] = v
	}

	_, indexAfterGrowth := ownedListPositionIndex(arr)
	if indexAfterGrowth == nil || indexAfterGrowth.items < buildListPositionIndexItems {
		t.Fatalf("array position index was not active at length=%d items=%d",
			arr.GetLength(), listItemCount(arr))
	}
	blocksAfterGrowth := len(indexAfterGrowth.anchors)
	validateListPositionTree(t, indexAfterGrowth, arr)
	validateDocPositionIndexEntries(t, doc)

	ops := target / 2
	for i := 0; i < ops; i++ {
		if rng(3) == 0 && len(model) > 16_500 {
			idx := rng(len(model))
			arr.Delete(idx, 1)
			model = append(model[:idx], model[idx+1:]...)
		} else {
			idx := rng(len(model) + 1)
			v := rng(1 << 20)
			arr.Insert(idx, ArrayAny{v})
			model = append(model, 0)
			copy(model[idx+1:], model[idx:])
			model[idx] = v
		}
	}

	if arr.GetLength() != len(model) {
		t.Fatalf("array length = %d, model %d", arr.GetLength(), len(model))
	}
	// Compare every element, not a checksum: a checksum over a permutation of the same values would
	// pass while the order — the only thing a marker can corrupt — is wrong.
	got := arr.ToArray()
	for i := range model {
		gv, ok := toModelInt(got[i])
		if !ok || gv != model[i] {
			t.Fatalf("array element %d = %v, want %d (length %d, markers %d)",
				i, got[i], model[i], len(model), len(*arr.getSearchMarker()))
		}
	}
	_, arrayIndex := ownedListPositionIndex(arr)
	if arrayIndex == nil {
		t.Fatal("position index fell back during mixed array operations")
	}
	validateListPositionTree(t, arrayIndex, arr)
	validateDocPositionIndexEntries(t, doc)

	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	rtArr := fresh.GetArray("a")
	if rtArr.GetLength() != len(model) {
		t.Fatalf("round-trip array length = %d, model %d", rtArr.GetLength(), len(model))
	}
	rt := rtArr.ToArray()
	for i := range model {
		gv, ok := toModelInt(rt[i])
		if !ok || gv != model[i] {
			t.Fatalf("round-trip element %d = %v, want %d", i, rt[i], model[i])
		}
	}

	t.Logf("POSITION_INDEX_ARRAY len=%d items=%d blocks=%d(after growth) %d(final)",
		arr.GetLength(), listItemCount(arr), blocksAfterGrowth, len(arrayIndex.anchors))
}

// TestScaledMarkerShrinkThenReuse covers the branch the schedule needs but the schedule test does
// not reach: a type that temporarily held many split Items, kept its enlarged marker slice, and
// then merged those Items. findMarker deliberately keeps judging cache density by the ACTUAL
// marker count in that state rather than by the smaller item-count-derived limit, and that
// asymmetry is only correct if the markers it retains still describe live positions.
func TestScaledMarkerShrinkThenReuse(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	rng := markerLCG(0xC0FFEE)

	model := make([]byte, 20_000)
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i := range model {
		model[i] = letters[rng(len(letters))]
	}
	txt.Insert(0, string(model), Object{})

	// Split one ContentString into 20k linked Items inside a transaction, populate the scaled
	// marker cache while those nodes exist, then let cleanup merge the pieces back into one. This
	// reaches the retained-cache state directly in the unit the schedule actually uses: Items.
	Transact(doc, func(trans *Transaction) {
		for clock := Number(1); clock < len(model); clock++ {
			if getItemCleanStart(trans, GenID(doc.ClientID, clock)) == nil {
				t.Fatalf("failed to split at clock %d", clock)
			}
		}
		if items := assertLinkedItemCount(t, txt); items != len(model) {
			t.Fatalf("split fixture has %d Items, want %d", items, len(model))
		}
		for index := Number(125); index < len(model); index += 125 {
			findMarker(txt, index)
		}
	}, nil, true)
	grown := len(*txt.getSearchMarker())
	if grown <= maxSearchMarker {
		t.Fatalf("cache never grew past %d; the shrink branch cannot be reached", maxSearchMarker)
	}
	if items := assertLinkedItemCount(t, txt); items != 1 {
		t.Fatalf("cleanup left %d Items, want one merged ContentString", items)
	}
	limit := searchMarkerLimit(listItemCount(txt))
	if limit != maxSearchMarker {
		t.Fatalf("item-count-derived limit after merge = %d, want %d", limit, maxSearchMarker)
	}
	retained := len(*txt.getSearchMarker())
	if retained <= limit {
		t.Fatalf("slice holds %d markers against an item-count-derived limit of %d; the retained-count "+
			"branch is not reached and this test does not cover what it claims", retained, limit)
	}

	// Keep operating below the 16k-Item threshold: every lookup starts with a marker slice far
	// larger than the item-count-derived limit, which is the retained-count configuration.
	for i := 0; i < 4_000; i++ {
		if rng(3) == 0 && len(model) > 0 {
			idx := rng(len(model))
			txt.Delete(idx, 1)
			model = append(model[:idx], model[idx+1:]...)
		} else {
			idx := rng(len(model) + 1)
			ch := letters[rng(len(letters))]
			txt.Insert(idx, string(ch), Object{})
			model = append(model, 0)
			copy(model[idx+1:], model[idx:])
			model[idx] = ch
		}
	}

	got := txt.ToString()
	if got != string(model) {
		t.Fatalf("after shrunk-state ops: content diverged (len want %d got %d)\n%s",
			len(model), len(got), firstDiff(got, string(model)))
	}

	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	if rt := fresh.GetText("t").ToString(); rt != got {
		t.Fatalf("round-trip after shrink diverged\n%s", firstDiff(got, rt))
	}

	// The branch must still be active at the end, not merely at the start. Without this the band
	// above could be retuned or the pruning behaviour changed, and the test would keep passing while
	// covering the ordinary path.
	endLimit := searchMarkerLimit(listItemCount(txt))
	endRetained := len(*txt.getSearchMarker())
	if endRetained <= endLimit {
		t.Fatalf("at the end the slice holds %d markers against limit %d; the loop left the "+
			"retained-count state and covered the ordinary path instead", endRetained, endLimit)
	}

	t.Logf("MARKER_SCALE_SHRINK grown=%d retained=%d/%d(limit) finalLen=%d items=%d",
		grown, endRetained, endLimit, txt.Length(), listItemCount(txt))
}

func toModelInt(v interface{}) (int, bool) {
	// Number is an alias for int, so it must not appear as a separate case here.
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// firstDiff reports where two large strings first differ, with a window of context. A bare
// "content diverged" on a 20k string is unactionable, and printing both in full is unreadable.
func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			lo := i - 20
			if lo < 0 {
				lo = 0
			}
			hi := i + 20
			if hi > n {
				hi = n
			}
			var b strings.Builder
			b.WriteString(" first difference at index ")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("\n want ...")
			b.WriteString(want[lo:hi])
			b.WriteString("...\n got  ...")
			b.WriteString(got[lo:hi])
			b.WriteString("...")
			return b.String()
		}
	}
	return " common prefix identical; lengths differ"
}

// ---------------------------------------------------------------- from search_marker_scale_test.go
func modeledMarkerCumulativeExponent(smallN, largeN Number, smallC, largeC int) float64 {
	// One lookup pays for the marker slice plus the average distance from the
	// nearest marker. Integrating that per-operation cost adds one to its exponent.
	smallCost := float64(smallC) + float64(smallN)/float64(smallC)
	largeCost := float64(largeC) + float64(largeN)/float64(largeC)
	return 1 + math.Log(largeCost/smallCost)/math.Log(float64(largeN)/float64(smallN))
}

func TestSearchMarkerLimitScalesAsSquareRoot(t *testing.T) {
	cases := []struct {
		length Number
		want   int
	}{
		{0, 80},
		{10_000, 80}, // the cross-implementation non-batched baseline
		{15_999, 80},
		{16_000, 160},
		{63_999, 160},
		{64_000, 320},
		{255_999, 320},
		{256_000, 640},
		{1_024_000, 1_280},
	}
	for _, tc := range cases {
		if got := searchMarkerLimit(tc.length); got != tc.want {
			t.Errorf("searchMarkerLimit(%d) = %d, want %d", tc.length, got, tc.want)
		}
	}

	const smallN, largeN = Number(16_000), Number(128_000)
	got := modeledMarkerCumulativeExponent(
		smallN, largeN, searchMarkerLimit(smallN), searchMarkerLimit(largeN),
	)
	if got >= 1.6 {
		t.Fatalf("modeled cumulative exponent = %.3f, want < 1.6", got)
	}

	// Pin the guard's teeth: restoring the reference's fixed 80-marker cap must
	// cross the bound that this test claims to defend.
	fixed := modeledMarkerCumulativeExponent(smallN, largeN, maxSearchMarker, maxSearchMarker)
	if fixed < 1.6 {
		t.Fatalf("fixed-cap control exponent = %.3f; scaling guard would not catch the regression", fixed)
	}
}

// Keep the historical name because scale-tier invocations select it explicitly. Internal random
// mutations now graduate from the marker cache to the block position index at the same threshold.
func TestLargeRandomInsertExpandsSearchMarkerCache(t *testing.T) {
	doc := newDoc("marker-scale", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	state := uint32(42)
	for i := 0; i < 20_000; i++ {
		state = state*1664525 + 1013904223
		index := Number(state % uint32(text.Length()+1))
		text.Insert(index, "x", Object{})
	}

	_, index := ownedListPositionIndex(text)
	if index == nil {
		t.Fatal("large random-insert workload did not activate the block position index")
	}
	if index.items != listItemCount(text) {
		t.Fatalf("position index has %d Items, linked list has %d", index.items, listItemCount(text))
	}
	if markers := *text.getSearchMarker(); len(markers) != 0 {
		t.Fatalf("internal mutation path retained %d mutable markers after block-index activation", len(markers))
	}
	if text.Length() != 20_000 {
		t.Fatalf("text length = %d, want 20000", text.Length())
	}
}
