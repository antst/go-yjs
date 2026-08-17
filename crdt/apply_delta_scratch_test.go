package crdt

import (
	"bytes"
	"testing"
)

func TestApplyDeltaObjectScratchIsAllocationOnly(t *testing.T) {
	t.Parallel()

	three := MakeObject("bold", true, "italic", true, "color", "red")
	two := MakeObject("bold", false, "italic", true)
	one := MakeObject("bold", true)
	attrs := []Object{three, two, one}

	build := func(useScratch bool) (*Doc, []byte, []byte) {
		doc := newDoc("apply-delta-scratch", false, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		Transact(doc, func(trans *Transaction) {
			pos := newItemTextListPosition(nil, text.start, 0, newObject())
			var scratch *insertTextObjectScratch
			if useScratch {
				scratch = &insertTextObjectScratch{}
			}
			for i := 0; i < 30; i++ {
				insertTextWithScratch(trans, text, pos, "x", attrs[i%len(attrs)], scratch)
			}
		}, nil, true)

		v1, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		return doc, v1, v2
	}

	plain, plainV1, plainV2 := build(false)
	optimized, optimizedV1, optimizedV2 := build(true)
	if !bytes.Equal(optimizedV1, plainV1) {
		t.Fatalf("scratch changed V1 encoding\nplain %x\n got  %x", plainV1, optimizedV1)
	}
	if !bytes.Equal(optimizedV2, plainV2) {
		t.Fatalf("scratch changed V2 encoding\nplain %x\n got  %x", plainV2, optimizedV2)
	}
	if optimized.GetText("t").ToString() != plain.GetText("t").ToString() {
		t.Fatal("scratch changed rendered text")
	}

	// The two-key input is promoted to a temporary large Object when the active `color`
	// attribute is negated. That promotion must affect only scratch storage, not the Object a
	// caller may reuse in later delta operations.
	if two.Len() != 2 || two.Has("color") {
		t.Fatalf("scratch mutation escaped into caller attributes: %+v", two)
	}
}

func TestFreshApplyDeltaReservesExactStructCapacity(t *testing.T) {
	t.Parallel()

	const ops = 20
	delta := make([]EventOperator, ops)
	for i := range delta {
		delta[i] = NewTextDeltaOp("chunk", MakeObject("bold", i%2 == 0))
	}
	if strings, origins := freshApplyDeltaTextStorageCounts(delta, true); strings != ops || origins != ops {
		t.Fatalf("fresh text storage counts = %d/%d, want %d/%d", strings, origins, ops, ops)
	}
	doc := newDoc("apply-delta-reserve", false, defaultGCFilter, nil, false, WithClientID(1))
	doc.GetText("t").ApplyDelta(delta, true)
	length, capacity := doc.store.clientLength(doc.ClientID), doc.store.clientCapacity(doc.ClientID)
	if length != ops*3 || capacity != ops*3 {
		t.Fatalf("client structs len/cap = %d/%d, want exact %d", length, capacity, ops*3)
	}

	withNull := []EventOperator{NewTextDeltaOp("x", MakeObject("clear", Null, "set", true))}
	if structs, formats := freshApplyDeltaCounts(withNull, true); structs != 3 || formats != 2 {
		t.Fatalf("fresh counts with null attribute = %d/%d, want 3/2", structs, formats)
	}
	if structs, formats := freshApplyDeltaCounts([]EventOperator{NewTextDeltaOp("\n", Object{})}, false); structs != 0 || formats != 0 {
		t.Fatalf("fresh counts for sanitized-away trailing newline = %d/%d, want 0/0", structs, formats)
	}
	if structs, formats := freshApplyDeltaCounts([]EventOperator{NewRetainDeltaOp(1, Object{})}, true); structs != 0 || formats != 0 {
		t.Fatalf("mixed/non-insert delta reserved %d/%d", structs, formats)
	}
	if len(doc.formatItemBlock) != ops*2 || doc.formatItemBlockUsed != ops*2 {
		t.Fatalf("format-item reservation len/used = %d/%d, want %d/%d",
			len(doc.formatItemBlock), doc.formatItemBlockUsed, ops*2, ops*2)
	}
	if len(doc.stringItemBlock) != ops || doc.stringItemBlockUsed != ops {
		t.Fatalf("string-item reservation len/used = %d/%d, want %d/%d",
			len(doc.stringItemBlock), doc.stringItemBlockUsed, ops, ops)
	}
	if len(doc.itemOriginBlock) != ops || doc.itemOriginBlockUsed != ops {
		t.Fatalf("origin reservation len/used = %d/%d, want %d/%d",
			len(doc.itemOriginBlock), doc.itemOriginBlockUsed, ops, ops)
	}
}

func TestFreshApplyDeltaCountsStringInValueArm(t *testing.T) {
	t.Parallel()

	delta := make([]EventOperator, localFormatItemArenaThreshold+1)
	for i := range delta {
		delta[i] = NewValueDeltaOp("wide", MakeObject("bold", true))
	}
	strings, origins := freshApplyDeltaTextStorageCounts(delta, true)
	if strings != 0 || origins != 0 {
		t.Fatalf("value-arm string reserved exact storage: %d/%d", strings, origins)
	}
	doc := newDoc("apply-delta-value-string-reserve", false, defaultGCFilter, nil, false, WithClientID(1))
	doc.GetText("t").ApplyDelta(delta, true)
	if len(doc.stringItemBlock) > 32 {
		t.Fatalf("value-arm string retained a %d-slot block, want bounded arena", len(doc.stringItemBlock))
	}
}

func TestFreshApplyDeltaDoesNotReserveMergeableStringRun(t *testing.T) {
	t.Parallel()

	const ops = 20
	delta := make([]EventOperator, ops)
	for i := range delta {
		delta[i] = NewTextDeltaOp("chunk", Object{})
	}
	strings, origins := freshApplyDeltaTextStorageCounts(delta, true)
	if strings != 0 || origins != 0 {
		t.Fatalf("mergeable string run reserved exact storage: %d/%d", strings, origins)
	}
	doc := newDoc("apply-delta-mergeable-strings", false, defaultGCFilter, nil, false, WithClientID(1))
	doc.GetText("t").ApplyDelta(delta, true)
	if len(doc.stringItemBlock) > 32 {
		t.Fatalf("mergeable string run retained a %d-slot block, want bounded arena", len(doc.stringItemBlock))
	}
	structs := doc.store.structsForClient(doc.ClientID)
	if len(structs) != 1 {
		t.Fatalf("mergeable string run left %d structs, want one", len(structs))
	}
}

func TestFreshFormattedDeltaKeepsReservedStringsPhysicallySeparated(t *testing.T) {
	t.Parallel()

	const ops = 20
	delta := make([]EventOperator, ops)
	for i := range delta {
		delta[i] = NewTextDeltaOp("chunk", MakeObject("bold", true))
	}
	for _, gc := range []bool{false, true} {
		doc := newDoc("apply-delta-separated-strings", gc, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		text.ApplyDelta(delta, true)
		stringItems := 0
		for item := text.start; item != nil; item = item.right {
			if _, ok := item.content.(*contentString); ok {
				stringItems++
			}
		}
		if stringItems != ops {
			t.Fatalf("gc=%v left %d physical string Items, want %d reserved slots in use", gc, stringItems, ops)
		}
	}
}

func TestStringUTF16LengthExceedsOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"a", false},
		{"é", false},
		{string([]byte{0xff}), false}, // one invalid byte decodes as one U+FFFD
		{"ab", true},
		{"éa", true},
		{"😀", true}, // one supplementary rune occupies two UTF-16 units
		{string([]byte{0xff, 0xff}), true},
	}
	for _, tc := range cases {
		if got := stringUTF16LengthExceedsOne(tc.value); got != tc.want {
			t.Errorf("stringUTF16LengthExceedsOne(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
