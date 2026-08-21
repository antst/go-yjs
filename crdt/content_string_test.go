package crdt

import (
	"strings"
	"testing"
	"unsafe"
)

// ---------------------------------------------------------------- from content_string_adjacency_test.go
// mergeSplitRight has to answer "are these two fragments contiguous in one
// allocation?", and the obvious way to ask — add the left length to the left
// data pointer and compare — is not available. When the answer is NO, which is
// the case the check exists to reject, that addition lands past the end of the
// left allocation. Go does not define such a pointer, and under -race the
// checkptr instrumentation kills the process outright with "pointer arithmetic
// result points to invalid allocation". The arithmetic has to happen in uintptr
// space, where no pointer is ever materialised.
//
// This test builds the rejection case deliberately. A batch of equally sized
// heap strings lands consecutively in one size-class span, so for two fragments
// taken from opposite ends of the batch, leftData+len(left) points into a
// DIFFERENT live heap object — exactly the condition checkptr fatals on, and
// the condition that made TestUndoDiff abort the whole race run rather than
// fail a single case.
//
// The teeth only bite under -race, since that is where checkptr runs. That is
// worth stating plainly: this test passing under a plain `go test` proves
// nothing, and the gate has to run the full package under -race for it to mean
// anything.
func TestMergeSplitRightRejectsNonAdjacentFragments(t *testing.T) {
	const batch, width = 64, 16

	// Fresh equally sized allocations, kept alive for the whole test so nothing
	// is recycled underneath the comparison.
	fragments := make([]*contentString, batch)
	for i := range fragments {
		b := make([]byte, width)
		for j := range b {
			b[j] = byte('a' + i%26)
		}
		s := string(b)
		fragments[i] = &contentString{value: s}
	}

	// Opposite ends of the batch: far enough apart that they cannot be the same
	// contiguous run, while the memory just past the left fragment still belongs
	// to a live object from this batch.
	left, right := fragments[0], fragments[batch-1]
	leftBefore, rightBefore := left.value, right.value

	if left.mergeSplitRight(right, width, width) {
		t.Fatal("two independently allocated fragments were treated as one contiguous " +
			"buffer; the merged string would span an allocation the runtime is not " +
			"keeping alive through this pointer")
	}
	if left.value != leftBefore || right.value != rightBefore {
		t.Fatalf("a rejected merge mutated its operands: left %q->%q right %q->%q",
			leftBefore, left.value, rightBefore, right.value)
	}

	// The rejection must not come from a failed ASCII-width proof, or the test
	// would pass for the wrong reason and keep passing if the
	// adjacency comparison were deleted entirely.
	if !left.hasASCIIWidth(width) || !right.hasASCIIWidth(width) {
		t.Fatal("fixture lost its ASCII width proof, so the false return proves nothing " +
			"about the adjacency comparison")
	}

	runtimeKeepAliveFragments(fragments)
}

// The genuinely contiguous case must still merge, otherwise a fix for the
// arithmetic that simply always returned false would pass the test above.
func TestMergeSplitRightStillJoinsContiguousFragments(t *testing.T) {
	const total = 32
	b := make([]byte, total)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	whole := string(b)

	left := &contentString{value: whole[:total/2]}
	right := &contentString{value: whole[total/2:]}

	if !left.mergeSplitRight(right, total/2, total/2) {
		t.Fatal("two halves of one immutable string were not recognised as contiguous, " +
			"so the zero-copy split rejoin is no longer firing")
	}
	if left.value != whole {
		t.Fatalf("rejoined string = %q, want %q", left.value, whole)
	}
	if !left.hasASCIIWidth(total) {
		t.Fatal("rejoined fragment lost the ASCII width proof, so the next merge in a " +
			"chain would fall back to copying")
	}
}

//go:noinline
func runtimeKeepAliveFragments(f []*contentString) {}

// ---------------------------------------------------------------- from content_string_arena_test.go
func TestContentStringItemArenaKeepsPointersStableAndTailBounded(t *testing.T) {
	doc := perfDoc()
	const count = 200
	storages := make([]*itemWithContentString, count)
	for i := range storages {
		storage := doc.allocateStringItemStorage()
		storage.content.value = string(rune('a' + i%26))
		storage.item.content = &storage.content
		storages[i] = storage
	}
	for i, storage := range storages {
		want := string(rune('a' + i%26))
		if storage.content.value != want || storage.item.content != &storage.content {
			t.Fatalf("storage %d moved or changed: content=%q item=%p contentPtr=%p", i, storage.content.value, storage.item.content, &storage.content)
		}
	}
	if len(doc.stringItemBlock) > 32 || len(doc.stringItemBlock)-doc.stringItemBlockUsed >= 32 {
		t.Fatalf("unbounded arena tail: len=%d used=%d", len(doc.stringItemBlock), doc.stringItemBlockUsed)
	}
}

func TestTextDeleteCoLocatesSplitStringsWithItems(t *testing.T) {
	doc := newDoc("split-string-arena", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "abcdef", Object{})
	text.Delete(2, 1)

	if got := text.ToString(); got != "abdef" {
		t.Fatalf("text after split delete = %q, want %q", got, "abdef")
	}
	if len(doc.stringItemBlock) != 2 || doc.stringItemBlockUsed != 2 {
		t.Fatalf("split arena block = %d/%d, want 2/2", len(doc.stringItemBlock), doc.stringItemBlockUsed)
	}

	want := []struct {
		str     string
		deleted bool
	}{
		{str: "ab"},
		{str: "c", deleted: true},
		{str: "def"},
	}
	count := 0
	for item, i := text.start, 0; item != nil; item, i = item.right, i+1 {
		if i >= len(want) {
			t.Fatalf("unexpected extra item %d", i)
		}
		content, ok := item.content.(*contentString)
		if !ok {
			t.Fatalf("item %d content = %T, want *ContentString", i, item.content)
		}
		if content.value != want[i].str || item.isDeleted() != want[i].deleted {
			t.Fatalf("item %d = (%T, %q, deleted=%v), want ContentString %q deleted=%v",
				i, item.content, content.value, item.isDeleted(), want[i].str, want[i].deleted)
		}
		if i > 0 {
			storage := &doc.stringItemBlock[i-1]
			if item != &storage.item || content != &storage.content {
				t.Fatalf("split item %d was not co-located with its content", i)
			}
			wantOrigin := GenID(1, i)
			if item.origin == nil || *item.origin != wantOrigin {
				t.Fatalf("split item %d origin = %v, want %v", i, item.origin, wantOrigin)
			}
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("item count = %d, want %d", count, len(want))
	}
}

func TestSplitStringMergeReusesOnlyProvenBacking(t *testing.T) {
	makeSplit := func() (*contentString, *itemStruct, *itemStruct, *byte) {
		doc := newDoc("split-string-merge", false, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		text.Insert(0, "abcdef", Object{})
		left := text.start
		originalData := unsafe.StringData(left.content.(*contentString).value)
		right := splitItem(newTransaction(doc, nil, true, true), left, 3)
		if right.info&itemInfoStringSplitBacking == 0 {
			t.Fatal("ASCII split did not record shared-backing provenance")
		}
		return left.content.(*contentString), left, right, originalData
	}

	leftContent, left, right, originalData := makeSplit()
	if !left.mergeStructWith(right) || leftContent.value != "abcdef" {
		t.Fatalf("validated split merge = %q, want abcdef", leftContent.value)
	}
	if unsafe.StringData(leftContent.value) != originalData {
		t.Fatal("validated split merge copied instead of reusing the original backing string")
	}

	leftContent, left, right, originalData = makeSplit()
	right.content.(*contentString).value = strings.Clone("def")
	if !left.mergeStructWith(right) || leftContent.value != "abcdef" {
		t.Fatalf("reassigned split merge = %q, want abcdef", leftContent.value)
	}
	if unsafe.StringData(leftContent.value) == originalData {
		t.Fatal("reassigned split bypassed the backing-adjacency guard")
	}
}

func TestSplitStringMergeReusesNonASCIIBackingAndIndex(t *testing.T) {
	doc := newDoc("split-unicode-string-merge", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	whole := strings.Repeat("界", 320)
	text.Insert(0, whole, Object{})
	left := text.start
	leftContent := left.content.(*contentString)
	originalData := unsafe.StringData(leftContent.value)

	right := splitItem(newTransaction(doc, nil, true, true), left, 160)
	rightContent := right.content.(*contentString)
	index := leftContent.utf16Index
	if right.info&itemInfoStringSplitBacking == 0 {
		t.Fatal("non-ASCII zero-copy split did not record shared-backing provenance")
	}
	if index == nil || rightContent.utf16Index != index {
		t.Fatalf("split index = left %p / right %p, want one shared index", index, rightContent.utf16Index)
	}

	if !left.mergeStructWith(right) || leftContent.value != whole {
		t.Fatalf("non-ASCII split merge length/value = %d/%q, want %d/original",
			left.length, leftContent.value, 320)
	}
	if unsafe.StringData(leftContent.value) != originalData {
		t.Fatal("non-ASCII split merge copied instead of reusing the original backing string")
	}
	if leftContent.utf16Index != index || leftContent.contentLength() != 320 {
		t.Fatalf("merged index/length = %p/%d, want %p/320", leftContent.utf16Index, leftContent.contentLength(), index)
	}
}

func TestSplitStringMergeRetainsIndexAtNonZeroSourceOrigin(t *testing.T) {
	doc := newDoc("split-offset-unicode-string-merge", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	prefix := strings.Repeat("界", 160) + "😀"
	middle := strings.Repeat("語", 160) + "𐐷"
	tail := strings.Repeat("文", 160)
	whole := prefix + middle + tail
	text.Insert(0, whole, Object{})

	prefixLength := stringLength(prefix)
	middleLength := stringLength(middle)
	tailLength := stringLength(tail)
	prefixItem := text.start
	middleItem := splitItem(newTransaction(doc, nil, true, true), prefixItem, prefixLength)
	tailItem := splitItem(newTransaction(doc, nil, true, true), middleItem, middleLength)
	middleContent := middleItem.content.(*contentString)
	tailContent := tailItem.content.(*contentString)
	index := middleContent.utf16Index
	if index == nil || tailContent.utf16Index != index {
		t.Fatalf("non-zero-origin fragments did not share the source index: middle=%p tail=%p",
			index, tailContent.utf16Index)
	}

	if !middleItem.mergeStructWith(tailItem) {
		t.Fatal("non-zero-origin fragments refused a provenance-backed merge")
	}
	mergedLength := middleLength + tailLength
	if middleContent.value != middle+tail || middleContent.utf16Index != index {
		t.Fatalf("merged non-zero-origin fragment value/index = %q/%p, want suffix/%p",
			middleContent.value, middleContent.utf16Index, index)
	}
	if got := middleContent.contentLength(); got != mergedLength {
		t.Fatalf("merged non-zero-origin length = %d, want %d", got, mergedLength)
	}

	// Samples are absolute to whole, while every requested offset below is
	// local to the remerged suffix. Check every UTF-16 position, including the
	// supplementary rune, against the scanner to prove that lookup rebases.
	for offset := Number(0); offset <= mergedLength+1; offset++ {
		wantLeft, wantRight := splitStringUTF16(middleContent.value, offset)
		candidate := *middleContent
		var gotRight contentString
		candidate.spliceWithLengthInto(offset, mergedLength, &gotRight)
		if candidate.value != wantLeft || gotRight.value != wantRight {
			t.Fatalf("non-zero-origin post-merge split at %d = (%q, %q), want (%q, %q)",
				offset, candidate.value, gotRight.value, wantLeft, wantRight)
		}
	}
}

func TestSplitStringMergeDoesNotMarkSurrogateBisectAsSharedBacking(t *testing.T) {
	doc := newDoc("split-surrogate-string-merge", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "😀", Object{})
	left := text.start
	right := splitItem(newTransaction(doc, nil, true, true), left, 1)

	if right.info&itemInfoStringSplitBacking != 0 {
		t.Fatal("surrogate-bisect replacement strings retained shared-backing provenance")
	}
	if left.content.(*contentString).value != "�" || right.content.(*contentString).value != "�" {
		t.Fatalf("surrogate-bisect split = %q/%q, want replacement halves",
			left.content.(*contentString).value, right.content.(*contentString).value)
	}
	if !left.mergeStructWith(right) || left.content.(*contentString).value != "��" {
		t.Fatal("surrogate-bisect fragments did not merge through the copying fallback")
	}
}

// ---------------------------------------------------------------- from content_string_bulk_merge_test.go
func TestBatchedStringCleanupCoalescesUTF16AndRepairsMarkers(t *testing.T) {
	doc := newDoc("batched-string-coalesce", false, nil, nil, false, WithClientID(7))
	text := doc.GetText("t")
	parts := make([]string, 96)
	for i := range parts {
		switch i % 3 {
		case 0:
			parts[i] = "a"
		case 1:
			parts[i] = "😀"
		default:
			parts[i] = "é"
		}
	}

	var marker *arraySearchMarker
	Transact(doc, func(*Transaction) {
		for _, part := range parts {
			text.Insert(text.Length(), part, Object{})
		}
		second := text.start.right
		if second == nil {
			t.Fatal("batched inserts merged before cleanup; test does not reach the bulk path")
		}
		marker = markPosition(text.getSearchMarker(), second, text.start.length)
	}, nil, true)

	want := strings.Join(parts, "")
	if got := text.ToString(); got != want {
		t.Fatalf("ToString() = %q, want %q", got, want)
	}
	if text.Length() != stringLength(want) {
		t.Fatalf("Length() = %d, want UTF-16 length %d", text.Length(), stringLength(want))
	}
	if text.start == nil || text.start.right != nil {
		t.Fatal("batched string run did not collapse to one item")
	}
	if marker == nil || marker.p != text.start || marker.index != 0 {
		t.Fatalf("absorbed-item marker = %#v, want surviving start item at index 0", marker)
	}

	update, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("encode batched document: %v", err)
	}
	replica := newDoc("batched-string-coalesce", false, nil, nil, false, WithClientID(8))
	_ = ApplyUpdate(replica, update, nil)
	if got := replica.GetText("t").ToString(); got != want {
		t.Fatalf("round-trip ToString() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------- from content_string_splice_perf_test.go
func TestSplitStringUTF16(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		offset Number
		left   string
		right  string
	}{
		{name: "start", text: "abcdef", offset: 0, left: "", right: "abcdef"},
		{name: "ascii", text: "abcdef", offset: 3, left: "abc", right: "def"},
		{name: "end", text: "abcdef", offset: 6, left: "abcdef", right: ""},
		{name: "past end", text: "abcdef", offset: 7, left: "abcdef", right: ""},
		{name: "bmp", text: "a世界b", offset: 2, left: "a世", right: "界b"},
		{name: "before supplementary", text: "a😀b", offset: 1, left: "a", right: "😀b"},
		{name: "inside supplementary", text: "a😀b", offset: 2, left: "a�", right: "�b"},
		{name: "after supplementary", text: "a😀b", offset: 3, left: "a😀", right: "b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left, right := splitStringUTF16(tc.text, tc.offset)
			if left != tc.left || right != tc.right {
				t.Fatalf("splitStringUTF16(%q, %d) = (%q, %q), want (%q, %q)", tc.text, tc.offset, left, right, tc.left, tc.right)
			}
			if got := stringHeader(tc.text, tc.offset); got != tc.left {
				t.Fatalf("stringHeader(%q, %d) = %q, want %q", tc.text, tc.offset, got, tc.left)
			}
			if got := stringTail(tc.text, tc.offset); got != tc.right {
				t.Fatalf("stringTail(%q, %d) = %q, want %q", tc.text, tc.offset, got, tc.right)
			}

			content := newContentString(tc.text)
			rightContent := content.spliceContent(tc.offset).(*contentString)
			if content.value != tc.left || rightContent.value != tc.right {
				t.Fatalf("ContentString.Splice(%d) = (%q, %q), want (%q, %q)", tc.offset, content.value, rightContent.value, tc.left, tc.right)
			}
			if content.contentLength() != stringLength(tc.left) || rightContent.contentLength() != stringLength(tc.right) {
				t.Fatalf("ContentString.Splice(%d) left/right lengths are inconsistent", tc.offset)
			}
		})
	}
}

func TestContentStringLengthTracksInternalValueReassignment(t *testing.T) {
	t.Parallel()

	c := newContentString("ascii")
	if got := c.contentLength(); got != 5 {
		t.Fatalf("initial length = %d, want 5", got)
	}

	// Replacing the package-private value before integration must immediately
	// affect the computed UTF-16 length, including for a surrogate pair.
	c.value = "a😀b"
	if got := c.contentLength(); got != 4 {
		t.Fatalf("length after direct Str mutation = %d, want 4", got)
	}

	right := c.spliceContent(2).(*contentString)
	if c.value != "a�" || c.contentLength() != 2 {
		t.Fatalf("left after split = %q/%d, want %q/2", c.value, c.contentLength(), "a�")
	}
	if right.value != "�b" || right.contentLength() != 2 {
		t.Fatalf("right after split = %q/%d, want %q/2", right.value, right.contentLength(), "�b")
	}
}

func TestContentStringLengthAuthorizesASCIIFastPath(t *testing.T) {
	t.Parallel()

	c := newContentString("abcd")
	if got := c.contentLength(); got != 4 || !c.hasASCIIWidth(got) {
		t.Fatalf("ASCII validation = length %d / width proof %v, want 4 / true", got, c.hasASCIIWidth(got))
	}

	// Same UTF-8 byte length as "abcd", but only two UTF-16 units. Recompute the
	// authoritative length after the package-private value is reassigned.
	c.value = "😀"
	right := c.spliceWithLength(1, c.contentLength()).(*contentString)
	if c.value != "�" || right.value != "�" {
		t.Fatalf("reassigned non-ASCII split = (%q, %q), want replacement halves", c.value, right.value)
	}
}

func TestContentStringUTF16IndexThresholdAndSplitPreservation(t *testing.T) {
	t.Parallel()

	short := newContentString(strings.Repeat("界", contentStringUTF16IndexThreshold-1))
	shortLength := short.contentLength()
	short.spliceWithLength(shortLength/2, shortLength)
	if short.utf16Index != nil {
		t.Fatal("short non-ASCII string retained a sampled index")
	}

	const runes = contentStringUTF16IndexThreshold * 3
	content := newContentString(strings.Repeat("界", runes))
	length := content.contentLength()
	splitAt := contentStringUTF16IndexThreshold + 32
	right := content.spliceWithLength(splitAt, length).(*contentString)
	index := content.utf16Index
	if index == nil || right.utf16Index != index {
		t.Fatalf("long split did not share its sampled index: left=%p right=%p", index, right.utf16Index)
	}
	if got, want := content.value, strings.Repeat("界", splitAt); got != want {
		t.Fatalf("indexed left = %q, want %q", got, want)
	}
	if got, want := right.value, strings.Repeat("界", runes-splitAt); got != want {
		t.Fatalf("indexed right = %q, want %q", got, want)
	}
	if got := right.contentLength(); got != length-splitAt {
		t.Fatalf("indexed right length = %d, want %d", got, length-splitAt)
	}

	rightTail := right.spliceWithLength(right.contentLength()-32, right.contentLength()).(*contentString)
	if right.utf16Index != index {
		t.Fatal("indexed fragment lost the shared index on its next split")
	}
	if rightTail.utf16Index != nil {
		t.Fatal("short tail fragment retained an index below the threshold")
	}
}

func TestContentStringUTF16IndexKeepsShortContentLayoutCompact(t *testing.T) {
	t.Parallel()

	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit layout assertion")
	}
	if got := unsafe.Sizeof(contentString{}); got != 24 {
		t.Fatalf("contentString size = %d, want 24; short strings must not carry index storage", got)
	}
	if got := unsafe.Sizeof(contentStringUTF16Sample{}); got != 8 {
		t.Fatalf("UTF-16 sample size = %d, want 8", got)
	}
}

func TestContentStringUTF16IndexHandlesSurrogateBoundaries(t *testing.T) {
	t.Parallel()

	prefix := strings.Repeat("界", 160)
	suffix := strings.Repeat("語", 160)
	content := newContentString(prefix + "😀" + suffix)
	length := content.contentLength()
	right := content.spliceWithLength(160, length).(*contentString)
	index := content.utf16Index
	if content.value != prefix || right.value != "😀"+suffix {
		t.Fatalf("boundary split = (%q, %q), want (%q, %q)", content.value, right.value, prefix, "😀"+suffix)
	}
	if index == nil || right.utf16Index != index {
		t.Fatal("surrogate-boundary split did not preserve the shared index")
	}

	insideRight := right.spliceWithLength(1, right.contentLength()).(*contentString)
	if right.value != "�" || insideRight.value != "�"+suffix {
		t.Fatalf("indexed surrogate-bisect split = (%q, %q), want (%q, %q)", right.value, insideRight.value, "�", "�"+suffix)
	}
	if right.utf16Index != nil || insideRight.utf16Index != nil {
		t.Fatal("surrogate-bisect replacement retained an index into the old backing string")
	}
}

func TestContentStringReassignmentInvalidatesUTF16Index(t *testing.T) {
	t.Parallel()

	content := newContentString(strings.Repeat("界", 320))
	length := content.contentLength()
	content.spliceWithLengthInto(160, length, &contentString{})
	oldIndex := content.utf16Index
	if oldIndex == nil {
		t.Fatal("long non-ASCII split did not build an index")
	}

	replacement := strings.Repeat("語", 320)
	content.value = replacement
	var right contentString
	content.spliceWithLengthInto(160, 320, &right)
	if content.value != strings.Repeat("語", 160) || right.value != strings.Repeat("語", 160) {
		t.Fatalf("reassigned indexed split = (%q, %q), want equal 160-rune halves", content.value, right.value)
	}
	if content.utf16Index == nil || content.utf16Index == oldIndex || right.utf16Index != content.utf16Index {
		t.Fatalf("reassigned string did not replace and share its stale index: old=%p left=%p right=%p",
			oldIndex, content.utf16Index, right.utf16Index)
	}
}

func TestContentStringUTF16IndexMatchesScannerAtEveryOffset(t *testing.T) {
	t.Parallel()

	source := strings.Repeat("a界😀é𐐷", 80)
	base := newContentString(source)
	length := base.contentLength()
	base.utf16Index = buildContentStringUTF16Index(base.value, length)
	if base.utf16Index == nil {
		t.Fatal("astral-plane fixture did not build a sampled index")
	}

	assertEveryOffset := func(t *testing.T, fragment contentString, fragmentLength Number) {
		t.Helper()
		for offset := Number(0); offset <= fragmentLength+1; offset++ {
			wantLeft, wantRight := splitStringUTF16(fragment.value, offset)
			candidate := fragment
			var gotRight contentString
			candidate.spliceWithLengthInto(offset, fragmentLength, &gotRight)
			if candidate.value != wantLeft || gotRight.value != wantRight {
				t.Fatalf("offset %d split = (%q, %q), want scanner (%q, %q)",
					offset, candidate.value, gotRight.value, wantLeft, wantRight)
			}
		}
	}

	assertEveryOffset(t, *base, length)

	// Repeat from a non-zero byte and UTF-16 origin. This is the case that
	// proves fragments can share absolute samples without storing local bases.
	left := *base
	right := left.spliceWithLength(161, length).(*contentString)
	if right.utf16Index != base.utf16Index {
		t.Fatal("non-zero-origin fragment did not retain the shared index")
	}
	assertEveryOffset(t, *right, right.contentLength())
}

func TestContentStringUTF16IndexSurvivesItemSplit(t *testing.T) {
	t.Parallel()

	doc := newDoc("indexed-item-split", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, strings.Repeat("界", 320), Object{})
	leftItem := text.start
	trans := newTransaction(doc, nil, true, true)
	rightItem := splitItem(trans, leftItem, 160)
	left := leftItem.content.(*contentString)
	right := rightItem.content.(*contentString)
	if left.value != strings.Repeat("界", 160) || right.value != strings.Repeat("界", 160) {
		t.Fatalf("item split values have lengths %d/%d bytes, want equal 160-rune halves",
			len(left.value), len(right.value))
	}
	if left.utf16Index == nil || right.utf16Index != left.utf16Index {
		t.Fatalf("item split did not share sampled index: left=%p right=%p", left.utf16Index, right.utf16Index)
	}
}

func TestContentStringUTF16IndexInvalidatesOnMergeAndRebuilds(t *testing.T) {
	t.Parallel()

	left := newContentString(strings.Repeat("界", 160))
	right := newContentString(strings.Repeat("語", 160))
	leftLength, rightLength := left.contentLength(), right.contentLength()
	left.utf16Index = buildContentStringUTF16Index(left.value, leftLength)
	right.utf16Index = buildContentStringUTF16Index(right.value, rightLength)
	if left.utf16Index == nil || right.utf16Index == nil {
		t.Fatal("merge fixture did not build both indexes")
	}

	if !left.mergeContentWith(right) {
		t.Fatal("contentString merge was refused")
	}
	if left.utf16Index != nil {
		t.Fatal("merged content retained offsets anchored to a pre-merge source")
	}

	var splitRight contentString
	left.spliceWithLengthInto(160, leftLength+rightLength, &splitRight)
	if left.value != strings.Repeat("界", 160) || splitRight.value != strings.Repeat("語", 160) {
		t.Fatalf("post-merge indexed split = (%q, %q), want original halves", left.value, splitRight.value)
	}
	if left.utf16Index == nil || splitRight.utf16Index != left.utf16Index {
		t.Fatal("first post-merge split did not build and share a fresh index")
	}
}

func TestCharCodeAtUTF16(t *testing.T) {
	text := "a😀b"
	want := []uint16{'a', 0xD83D, 0xDE00, 'b'}
	for i, code := range want {
		got, err := charCodeAt(text, i)
		if err != nil || got != code {
			t.Fatalf("CharCodeAt(%q, %d) = (%#x, %v), want (%#x, nil)", text, i, got, err, code)
		}
	}
	for _, index := range []Number{-1, 4} {
		if _, err := charCodeAt(text, index); err == nil {
			t.Fatalf("CharCodeAt(%q, %d) unexpectedly succeeded", text, index)
		}
	}
}

func TestUTF16HelpersRejectNegativeOrOutOfRangeOffsets(t *testing.T) {
	t.Run("negative split", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("splitStringUTF16 unexpectedly accepted a negative offset")
			}
		}()
		splitStringUTF16("abc", -1)
	})
}
