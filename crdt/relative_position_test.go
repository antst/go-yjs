package crdt

import (
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------- from relative_position_bench_test.go
func benchDenseRelativeText(n int) *YText {
	text := perfDoc().GetText("t")
	rng := perfRand()
	for inserted := 0; inserted < n; inserted++ {
		text.Insert(rng.intn(text.Length()+1), "x", Object{})
	}
	return text
}

func BenchmarkRelativePositionCreateDense100k(b *testing.B) {
	text := benchDenseRelativeText(perfHundredK)
	rng := perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		benchSinkRelPos = newRelativePositionFromTypeIndex(text, rng.intn(text.Length()+1), 0)
	}
}

func BenchmarkRelativePositionResolveDense100k(b *testing.B) {
	text := benchDenseRelativeText(perfHundredK)
	rng := perfRand()
	positions := make([]*RelativePosition, 1024)
	for index := range positions {
		positions[index] = newRelativePositionFromTypeIndex(text, rng.intn(text.Length()+1), 0)
	}
	doc := text.getDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		benchSinkAbsPos = CreateAbsolutePositionFromRelativePosition(positions[iteration%len(positions)], doc)
	}
}

func BenchmarkRelativePositionCreateSingleRun100k(b *testing.B) {
	text := perfDoc().GetText("t")
	text.Insert(0, perfString(perfHundredK), Object{})
	rng := perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		benchSinkRelPos = newRelativePositionFromTypeIndex(text, rng.intn(text.Length()+1), 0)
	}
}

func BenchmarkRelativePositionResolveSingleRun100k(b *testing.B) {
	text := perfDoc().GetText("t")
	text.Insert(0, perfString(perfHundredK), Object{})
	position := newRelativePositionFromTypeIndex(text, perfHundredK/2, 0)
	doc := text.getDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		benchSinkAbsPos = CreateAbsolutePositionFromRelativePosition(position, doc)
	}
}

// ---------------------------------------------------------------- from relative_position_index_test.go
func linearRelativePositionFromTypeIndex(tp abstractType, index, assoc Number) *RelativePosition {
	item := tp.startItem()
	if assoc < 0 {
		if index == 0 {
			return newRelativePosition(tp, nil, assoc)
		}
		index--
	}
	for item != nil {
		if !item.isDeleted() && item.countable() {
			if item.length > index {
				id := GenID(item.id.Client, item.id.Clock+index)
				return newRelativePosition(tp, &id, assoc)
			}
			index -= item.length
		}
		if item.right == nil && assoc < 0 {
			return newRelativePosition(tp, item.lastID(), assoc)
		}
		item = item.right
	}
	return newRelativePosition(tp, nil, assoc)
}

func linearAbsolutePositionFromRelativeItem(rpos *RelativePosition, doc *Doc) *AbsolutePosition {
	if rpos.Item == nil {
		return CreateAbsolutePositionFromRelativePosition(rpos, doc)
	}
	if getState(doc.store, rpos.Item.Client) <= rpos.Item.Clock {
		return nil
	}
	right, diff := followRedone(doc.store, *rpos.Item)
	if right == nil {
		return nil
	}
	parent := right.parent.(abstractType)
	index := Number(0)
	if parent.getItem() == nil || !parent.getItem().isDeleted() {
		if !right.isDeleted() && right.countable() {
			if rpos.Assoc >= 0 {
				index = diff
			} else {
				index = diff + 1
			}
		}
		for item := right.left; item != nil; item = item.left {
			if !item.isDeleted() && item.countable() {
				index += item.length
			}
		}
	}
	return newAbsolutePosition(parent, index, rpos.Assoc)
}

func installTestListPositionIndex(t *testing.T, parent abstractType) *listPositionIndex {
	t.Helper()
	destroyListPositionIndex(parent)
	state := abstractTypeState(parent)
	if state == nil || state.doc == nil {
		t.Fatal("fixture has no attached abstract type state")
	}
	index := buildListPositionIndex(parent)
	if state.doc.positionIndexes == nil {
		state.doc.positionIndexes = make(map[*abstractTypeBase]*listPositionIndex)
	}
	state.doc.positionIndexes[state] = index
	validateListPositionTree(t, index, parent)
	return index
}

func TestRelativePositionIndexMatchesLinkedWalkAtEveryBoundary(t *testing.T) {
	doc := newDoc("relative-position-index", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, perfString(300), Object{})
	rng := markerLCG(0x51de)
	retained := make([]*RelativePosition, 0, 40)
	for inserted := 0; inserted < 240; inserted++ {
		at := rng(text.Length() + 1)
		text.Insert(at, "x", Object{})
		if inserted%6 == 0 {
			retained = append(retained, linearRelativePositionFromTypeIndex(text, at, 0))
		}
	}
	deletedAnchor := linearRelativePositionFromTypeIndex(text, 10, 0)
	text.Delete(10, 1)
	retained = append(retained, deletedAnchor)
	for deleted := 0; deleted < 60; deleted++ {
		if text.Length() > 2 {
			text.Delete(rng(text.Length()-1), 1)
		}
	}
	bold := newObject()
	bold.Set("bold", true)
	text.Format(20, 80, bold)
	clearBold := newObject()
	clearBold.Set("bold", Null)
	text.Format(50, 30, clearBold)

	index := installTestListPositionIndex(t, text)
	visible := Number(0)
	physical := Number(0)
	for item := text.startItem(); item != nil; item = item.right {
		got, ok := indexedVisibleStart(text, item)
		if !ok || got != visible {
			t.Fatalf("physical item %d indexed start=(%d,%v), want %d", physical, got, ok, visible)
		}
		visible += itemVisibleLength(item)
		physical++
	}
	if visible != text.Length() || physical != index.items {
		t.Fatalf("fixture totals visible/items=%d/%d, want %d/%d", visible, physical, text.Length(), index.items)
	}
	deletedItem := getStruct(doc.store, *deletedAnchor.Item)
	if deletedItem == nil || !deletedItem.isDeleted() {
		t.Fatal("retained-position fixture did not retain an anchor inside a deleted Item")
	}

	for _, assoc := range []Number{-1, 0, 1} {
		for target := Number(0); target <= text.Length(); target++ {
			indexedTarget := target
			if assoc < 0 && indexedTarget > 0 {
				indexedTarget--
			}
			if _, _, ok := indexedReadPosition(text, indexedTarget); !ok && target > 0 {
				t.Fatalf("indexed position unavailable at target=%d assoc=%d", target, assoc)
			}
			want := linearRelativePositionFromTypeIndex(text, target, assoc)
			got := newRelativePositionFromTypeIndex(text, target, assoc)
			if !CompareRelativePositions(got, want) {
				t.Fatalf("relative target=%d assoc=%d got=%+v want=%+v", target, assoc, got, want)
			}
			gotAbs := CreateAbsolutePositionFromRelativePosition(got, doc)
			wantAbs := linearAbsolutePositionFromRelativeItem(want, doc)
			if gotAbs == nil || wantAbs == nil || gotAbs.Type != wantAbs.Type || gotAbs.Index != wantAbs.Index || gotAbs.Assoc != wantAbs.Assoc {
				t.Fatalf("absolute target=%d assoc=%d got=%+v want=%+v", target, assoc, gotAbs, wantAbs)
			}
		}
	}
	for position, retainedPosition := range retained {
		got := CreateAbsolutePositionFromRelativePosition(retainedPosition, doc)
		want := linearAbsolutePositionFromRelativeItem(retainedPosition, doc)
		if got == nil || want == nil || got.Type != want.Type || got.Index != want.Index || got.Assoc != want.Assoc {
			t.Fatalf("retained position %d got=%+v want=%+v", position, got, want)
		}
	}
	formatChecked := false
	for item := text.startItem(); item != nil; item = item.right {
		if _, ok := item.content.(*contentFormat); !ok || item.isDeleted() {
			continue
		}
		for _, assoc := range []Number{-1, 0, 1} {
			id := item.id
			position := &RelativePosition{Item: &id, Assoc: assoc}
			got := CreateAbsolutePositionFromRelativePosition(position, doc)
			want := linearAbsolutePositionFromRelativeItem(position, doc)
			if got == nil || want == nil || got.Type != want.Type || got.Index != want.Index || got.Assoc != want.Assoc {
				t.Fatalf("format anchor assoc=%d got=%+v want=%+v", assoc, got, want)
			}
		}
		formatChecked = true
		break
	}
	if !formatChecked {
		t.Fatal("fixture contains no live uncountable format anchor")
	}
}

func TestRelativePositionUsesActiveListIndexAtScale(t *testing.T) {
	doc := newDoc("relative-position-index-scale", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	rng := markerLCG(0x1de5)
	for inserted := 0; inserted < buildListPositionIndexItems+500; inserted++ {
		text.Insert(rng(text.Length()+1), "x", Object{})
	}
	_, index := ownedListPositionIndex(text)
	if index == nil {
		t.Fatal("dense fixture did not activate the writer position index")
	}
	validateListPositionTree(t, index, text)
	for sample := 0; sample < 256; sample++ {
		target := rng(text.Length() + 1)
		want := linearRelativePositionFromTypeIndex(text, target, 0)
		got := newRelativePositionFromTypeIndex(text, target, 0)
		if !CompareRelativePositions(got, want) {
			t.Fatalf("sample %d target=%d got=%+v want=%+v", sample, target, got, want)
		}
		gotAbs := CreateAbsolutePositionFromRelativePosition(got, doc)
		wantAbs := linearAbsolutePositionFromRelativeItem(want, doc)
		if gotAbs == nil || wantAbs == nil || gotAbs.Index != wantAbs.Index || gotAbs.Type != wantAbs.Type {
			t.Fatalf("sample %d absolute got=%+v want=%+v", sample, gotAbs, wantAbs)
		}
	}
}

func TestRelativePositionConcurrentReadsNeverActivateIndex(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "inactive"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			doc := newDoc("relative-position-index-concurrent-"+name, false, defaultGCFilter, nil, false, WithClientID(1))
			text := doc.GetText("t")
			rng := markerLCG(0xc011)
			for inserted := 0; inserted < buildListPositionIndexItems+500; inserted++ {
				text.Insert(rng(text.Length()+1), "x", Object{})
			}
			_, built := ownedListPositionIndex(text)
			if built == nil {
				t.Fatal("dense fixture did not activate the writer position index")
			}
			if !active {
				destroyListPositionIndex(text)
				built = nil
			}

			var wait sync.WaitGroup
			errors := make(chan error, 8)
			for worker := 0; worker < 8; worker++ {
				wait.Add(1)
				go func(worker int) {
					defer wait.Done()
					for step := 0; step < 64; step++ {
						target := (worker*977 + step*131) % (text.Length() + 1)
						position := newRelativePositionFromTypeIndex(text, target, 0)
						absolute := CreateAbsolutePositionFromRelativePosition(position, doc)
						if absolute == nil || absolute.Type != text || absolute.Index != target {
							errors <- fmt.Errorf("worker=%d step=%d target=%d absolute=%+v", worker, step, target, absolute)
							return
						}
					}
				}(worker)
			}
			wait.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
			_, after := ownedListPositionIndex(text)
			if after != built {
				t.Fatalf("relative-position reads changed index pointer from %p to %p", built, after)
			}
		})
	}
}

// ---------------------------------------------------------------- from relative_position_malformed_test.go
// A relative position arrives from a peer, so every byte of it is untrusted, and
// the only two acceptable outcomes are a valid position or an error.
//
// It used to have a third. Every read in readRelativePosition discarded its
// error, and the assoc read finished with a bare `v.(Number)`. ReadVarInt returns
// an any holding a Number on success and a uint64 partial on failure, so the
// failure path asserted uint64 to int and took the process down. That turns any
// peer that can send bytes into one that can stop the process, and it needs no
// malformed intent — a truncated frame is enough.
//
// Exhaustive rather than sampled because the input is short and the failures were
// dense: 16,000 of the 65,792 one- and two-byte inputs panicked, the shortest
// being 03 80. A sampling fuzzer would have found it, but it also would have
// found it a year from now; enumerating the whole space costs milliseconds.
func TestDecodeRelativePositionNeverPanicsOnShortInput(t *testing.T) {
	panics := 0
	var firstPanic string
	decoded, refused := 0, 0

	try := func(raw []byte) {
		defer func() {
			if r := recover(); r != nil {
				panics++
				if firstPanic == "" {
					firstPanic = fmt.Sprintf("%x -> %v", raw, r)
				}
			}
		}()
		rp, err := DecodeRelativePosition(raw)
		switch {
		case err != nil:
			refused++
			if rp != nil {
				t.Errorf("input %x returned both an error and a position; a rejected frame must "+
					"not yield something a caller can resolve", raw)
			}
		default:
			decoded++
		}
	}

	for a := 0; a < 256; a++ {
		try([]byte{byte(a)})
		for b := 0; b < 256; b++ {
			try([]byte{byte(a), byte(b)})
		}
	}

	if panics > 0 {
		t.Fatalf("%d of %d short inputs panicked (first %s); a decoder fed by remote peers must "+
			"return an error, never crash", panics, 256+256*256, firstPanic)
	}
	// Guard against the opposite failure: a decoder that rejects everything would
	// pass the assertion above while making the type useless.
	if decoded == 0 {
		t.Fatal("no short input decoded successfully, so this test would pass against a decoder " +
			"that rejects all input")
	}
	t.Logf("RELPOS short-input sweep: decoded=%d refused=%d panics=0", decoded, refused)
}

// The round trip must still work, so the hardening above cannot have been bought
// by rejecting valid frames.
func TestDecodeRelativePositionRoundTripsAllThreeTags(t *testing.T) {
	itemID := GenID(7, 9)
	typeID := GenID(3, 4)
	for name, rp := range map[string]*RelativePosition{
		"item":  {Item: &itemID, Assoc: 0},
		"tname": {Tname: "XmlText", Assoc: -1},
		"type":  {Type: &typeID, Assoc: 1},
	} {
		got, err := DecodeRelativePosition(EncodeRelativePosition(rp))
		if err != nil {
			t.Fatalf("%s: valid position was refused: %v", name, err)
		}
		if got.Assoc != rp.Assoc || got.Tname != rp.Tname {
			t.Fatalf("%s: round trip changed the position: got assoc=%d tname=%q", name, got.Assoc, got.Tname)
		}
	}
}

// An unknown tag must be refused rather than silently producing an empty position,
// which resolves as "start of an unnamed root" and puts a remote cursor somewhere
// real instead of reporting that the frame made no sense.
func TestDecodeRelativePositionRejectsUnknownTag(t *testing.T) {
	for _, tag := range []byte{3, 4, 9, 0x7f} {
		if _, err := DecodeRelativePosition([]byte{tag, 0}); err == nil {
			t.Errorf("tag %d was accepted; only 0, 1 and 2 are written by the reference", tag)
		}
	}
}

// The JSON projection must round-trip through its own two halves. It did not:
// RelativePositionToJSON wrote *ID and CreateRelativePositionFromJSON asserted
// ID, so feeding one directly into the other panicked. Nothing caught it because
// nothing had ever connected them.
func TestRelativePositionJSONRoundTrips(t *testing.T) {
	itemID, typeID := GenID(11, 22), GenID(33, 44)
	for name, rp := range map[string]*RelativePosition{
		"item":  {Item: &itemID, Assoc: 1},
		"type":  {Type: &typeID, Assoc: -1},
		"tname": {Tname: "root", Assoc: 0},
	} {
		back, err := CreateRelativePositionFromJSON(RelativePositionToJSON(rp))
		if err != nil {
			t.Fatalf("%s: our own projection was rejected by our own reader: %v", name, err)
		}
		if !CompareRelativePositions(rp, back) {
			t.Fatalf("%s: json round trip changed the position: %#v -> %#v", name, rp, back)
		}
	}
}

// A malformed projection must be refused rather than crash: the Object can come
// from a caller or from real JSON, neither of which this package controls.
func TestRelativePositionJSONRejectsWrongTypes(t *testing.T) {
	for name, build := range map[string]func() Object{
		"type not an id":  func() Object { o := newObject(); o.Set("type", "nope"); return o },
		"item not an id":  func() Object { o := newObject(); o.Set("item", 42); return o },
		"tname not text":  func() Object { o := newObject(); o.Set("tname", 7); return o },
		"assoc not a num": func() Object { o := newObject(); o.Set("assoc", "left"); return o },
	} {
		if _, err := CreateRelativePositionFromJSON(build()); err == nil {
			t.Errorf("%s: malformed projection was accepted", name)
		}
	}
}

// A root type must produce Type == nil, matching yjs createRelativePosition:
//
//	if (type._item === null) { tname = findRootTypeKey(type) } else { typeid = createID(...) }
//
// It used to set Type to the address of a zero ID for roots, which is a divergence
// from the reference: compareRelativePositions compares type, so a zero ID made a
// root position unequal to any correctly-built one.
//
// Note what this deliberately does NOT assert. yjs sets tname alongside item for a
// root type, while the wire form carries only the item, so an original position and
// its decoding differ in tname there. That asymmetry is reference behaviour, not a
// defect, and a test that "fixed" it would be diverging from yjs rather than
// converging on it.
func TestRootRelativePositionHasNilType(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "hello", Object{})

	for _, idx := range []Number{0, 3, 5} {
		rp := newRelativePositionFromTypeIndex(txt, idx, 0)
		if rp.Type != nil {
			t.Fatalf("index %d: root position carries Type=%v; yjs leaves typeid null for a root "+
				"type and compareRelativePositions compares it", idx, rp.Type)
		}
		if rp.Tname != "t" {
			t.Fatalf("index %d: root position lost its type name: %q", idx, rp.Tname)
		}
		back, err := DecodeRelativePosition(EncodeRelativePosition(rp))
		if err != nil {
			t.Fatalf("index %d: decode: %v", idx, err)
		}
		abs := CreateAbsolutePositionFromRelativePosition(back, doc)
		if abs == nil || abs.Index != idx {
			t.Fatalf("index %d: round-tripped position resolved to %v", idx, abs)
		}
	}
}

// ---------------------------------------------------------------- from relative_position_test.go
func TestDecodeRelativePositionTags(t *testing.T) {
	itemID := GenID(1, 2)
	rp0 := &RelativePosition{Item: &itemID, Assoc: 0}
	dec0, err := DecodeRelativePosition(EncodeRelativePosition(rp0))
	if err != nil {
		t.Fatalf("decode relative position: %v", err)
	}
	if dec0.Item == nil || dec0.Item.Client != 1 || dec0.Item.Clock != 2 {
		t.Fatalf("case 0: got item %#v", dec0.Item)
	}

	rp1 := &RelativePosition{Tname: "XmlText", Assoc: -1}
	dec1, err := DecodeRelativePosition(EncodeRelativePosition(rp1))
	if err != nil {
		t.Fatalf("decode relative position: %v", err)
	}
	if dec1.Tname != "XmlText" || dec1.Assoc != -1 {
		t.Fatalf("case 1: got tname=%q assoc=%v", dec1.Tname, dec1.Assoc)
	}

	typeID := GenID(3, 4)
	rp2 := &RelativePosition{Type: &typeID, Assoc: 1}
	dec2, err := DecodeRelativePosition(EncodeRelativePosition(rp2))
	if err != nil {
		t.Fatalf("decode relative position: %v", err)
	}
	if dec2.Type == nil || dec2.Type.Client != 3 || dec2.Type.Clock != 4 {
		t.Fatalf("case 2: got type %#v", dec2.Type)
	}
}
