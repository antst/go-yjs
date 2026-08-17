package crdt

import (
	"reflect"
	"testing"
	"unsafe"
)

type noLazyFormatContentArenaDecoder struct {
	updateDecoder
}

type countingFormatKeyDecoder struct {
	updateDecoder
	keyReads int
}

func (*noLazyFormatContentArenaDecoder) disableLazyFormatContentArena() {}

func (d *noLazyFormatContentArenaDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func (d *countingFormatKeyDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func (d *countingFormatKeyDecoder) readKey() (string, error) {
	d.keyReads++
	return d.updateDecoder.readKey()
}

func newNoLazyFormatContentArenaDecoderV1(buf []byte) updateDecoder {
	return &noLazyFormatContentArenaDecoder{updateDecoder: newUpdateDecoderV1(buf)}
}

func newNoLazyFormatContentArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyFormatContentArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

const (
	lazyFormatFixtureParts = 3
	lazyFormatFixtureOps   = 256
)

func lazyFormatArenaFixture(t *testing.T) ([][]byte, [][]byte, []byte, []byte, *Snapshot) {
	t.Helper()
	partsV1 := make([][]byte, lazyFormatFixtureParts)
	partsV2 := make([][]byte, lazyFormatFixtureParts)
	combined := newDoc("format-combined", false, defaultGCFilter, nil, false, WithClientID(999))
	for part := range partsV1 {
		doc := newDoc("format-part", false, defaultGCFilter, nil, false, WithClientID(Number(part+1)))
		delta := make([]EventOperator, lazyFormatFixtureOps)
		for i := range delta {
			attrs := newObject()
			key := "bold"
			if i&1 != 0 {
				key = "italic"
			}
			attrs.Set(key, true)
			delta[i] = NewTextDeltaOp(string(rune('a'+part)), attrs)
		}
		doc.GetText("t").ApplyDelta(delta, true)

		var err error
		partsV1[part], err = EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatalf("encode V1 format part %d: %v", part, err)
		}
		partsV2[part], err = EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("encode V2 format part %d: %v", part, err)
		}
		_ = ApplyUpdateV2(combined, partsV2[part], nil)
	}

	fullV1, err := EncodeStateAsUpdate(combined, nil)
	if err != nil {
		t.Fatalf("encode combined V1: %v", err)
	}
	fullV2, err := EncodeStateAsUpdateV2(combined, nil)
	if err != nil {
		t.Fatalf("encode combined V2: %v", err)
	}
	return partsV1, partsV2, fullV1, fullV2, NewSnapshotByDoc(combined)
}

func lazyFormatContentCounts(t *testing.T, update []byte) (structs, strings, formats int) {
	t.Helper()
	decoded, err := decodeUpdateWith(update, newNoLazyFormatContentArenaDecoderV2)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range decoded.structs {
		item, ok := raw.(*itemStruct)
		if !ok {
			continue
		}
		switch item.content.(type) {
		case *contentString:
			strings++
		case *contentFormat:
			formats++
		}
	}
	return len(decoded.structs), strings, formats
}

func TestLazyContentFormatArenaPreservesUpdateUtilities(t *testing.T) {
	testLazyArenaPreservesUpdateUtilitiesWithFixture(
		t, newNoLazyFormatContentArenaDecoderV1, newNoLazyFormatContentArenaDecoderV2, lazyFormatArenaFixture,
	)
}

func TestLazyContentFormatArenaReadsKeysFromKeyColumn(t *testing.T) {
	_, _, _, fullV2, _ := lazyFormatArenaFixture(t)
	_, _, formatItems := lazyFormatContentCounts(t, fullV2)
	var decoder *countingFormatKeyDecoder
	_, err := decodeUpdateWith(fullV2, func(buf []byte) updateDecoder {
		decoder = &countingFormatKeyDecoder{updateDecoder: newUpdateDecoderV2(buf)}
		return decoder
	})
	if err != nil {
		t.Fatalf("decode format update: %v", err)
	}
	if decoder.keyReads != formatItems {
		t.Fatalf("ContentFormat decoder used ReadKey %d times for %d format items", decoder.keyReads, formatItems)
	}
}

func TestLazyContentFormatArenaRetainsContentAcrossBlockReplacement(t *testing.T) {
	_, _, _, fullV2, _ := lazyFormatArenaFixture(t)
	arena, err := decodeUpdateV2(fullV2)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	control, err := decodeUpdateWith(fullV2, newNoLazyFormatContentArenaDecoderV2)
	if err != nil {
		t.Fatalf("control decode: %v", err)
	}
	_, strings, formats := lazyFormatContentCounts(t, fullV2)
	wantStrings := lazyFormatFixtureParts * lazyFormatFixtureOps
	wantFormats := wantStrings * 2
	if strings != wantStrings || formats != wantFormats {
		t.Fatalf("fixture shape changed: ContentString=%d want %d, ContentFormat=%d want %d", strings, wantStrings, formats, wantFormats)
	}
	if !reflect.DeepEqual(arena, control) {
		t.Fatal("decoded formats changed after later ContentFormat blocks replaced the arena slice")
	}
}

func TestLazyContentFormatArenaRemovesPerFormatAllocations(t *testing.T) {
	_, _, _, fullV2, _ := lazyFormatArenaFixture(t)
	_, _, formatItems := lazyFormatContentCounts(t, fullV2)
	wantFormats := lazyFormatFixtureParts * lazyFormatFixtureOps * 2
	if formatItems != wantFormats {
		t.Fatalf("allocation fixture has %d ContentFormat items, want exactly %d", formatItems, wantFormats)
	}
	arenaAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateV2(fullV2); err != nil {
			panic(err)
		}
	})
	controlAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newNoLazyFormatContentArenaDecoderV2); err != nil {
			panic(err)
		}
	})
	if saved := controlAllocs - arenaAllocs; saved*100 < float64(formatItems)*95 {
		t.Fatalf("ContentFormat blocks did not remove one allocation per format item: items %d arena %.0f control %.0f saved %.0f", formatItems, arenaAllocs, controlAllocs, saved)
	}
}

func TestLazyContentFormatArenaDoesNotTaxUnformattedBlocks(t *testing.T) {
	_, _, _, textV2, _ := lazyIDArenaFixture(t)
	_, mapV2 := lazyItemArenaMapFixture(t)
	for name, update := range map[string][]byte{"unformatted-text": textV2, "map": mapV2} {
		t.Run(name, func(t *testing.T) {
			structs, _, formats := lazyFormatContentCounts(t, update)
			if structs < 500 || formats != 0 {
				t.Fatalf("baseline fixture does not exercise a substantial format-free block: structs=%d formats=%d", structs, formats)
			}
			arenaAllocs := testing.AllocsPerRun(5, func() {
				if _, err := encodeStateVectorFromUpdateV2(update); err != nil {
					panic(err)
				}
			})
			controlAllocs := testing.AllocsPerRun(5, func() {
				if _, err := encodeStateVectorFromUpdateWith(update, newEncoderV2, newNoLazyFormatContentArenaDecoderV2); err != nil {
					panic(err)
				}
			})
			if arenaAllocs > controlAllocs {
				t.Fatalf("format arena taxed a format-free block: arena %.0f control %.0f", arenaAllocs, controlAllocs)
			}
		})
	}
}

func TestLazyContentFormatBlockStaysWithinMallocClass(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit malloc-class assertion")
	}
	const (
		pointerfulMallocHeader = uintptr(8)
		mallocClassLimit       = uintptr(4096)
	)
	formatSize := unsafe.Sizeof(contentFormat{})
	if formatSize != 32 {
		t.Fatalf("ContentFormat size = %d, want 32; recalibrate the %d-entry format block against Go's pointerful malloc classes", formatSize, lazyFormatContentBlockMax)
	}
	blockBytes := uintptr(lazyFormatContentBlockMax)*formatSize + pointerfulMallocHeader
	if blockBytes > mallocClassLimit {
		t.Fatalf("%d-entry ContentFormat block needs %d bytes including the scan header; exceeds the %d-byte malloc class", lazyFormatContentBlockMax, blockBytes, mallocClassLimit)
	}
	nextBlockBytes := uintptr(lazyFormatContentBlockMax+1)*formatSize + pointerfulMallocHeader
	if nextBlockBytes <= mallocClassLimit {
		t.Fatalf("%d-entry ContentFormat cap is not maximal: %d entries still fit in the %d-byte malloc class", lazyFormatContentBlockMax, lazyFormatContentBlockMax+1, mallocClassLimit)
	}
}
