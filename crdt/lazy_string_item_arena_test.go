package crdt

import (
	"reflect"
	"testing"
)

type noLazyStringContentArenaDecoder struct {
	updateDecoder
}

func (*noLazyStringContentArenaDecoder) disableLazyStringContentArena() {}

func (d *noLazyStringContentArenaDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func newNoLazyStringContentArenaDecoderV1(buf []byte) updateDecoder {
	return &noLazyStringContentArenaDecoder{updateDecoder: newUpdateDecoderV1(buf)}
}

func newNoLazyStringContentArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyStringContentArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

func TestLazyContentStringArenaPreservesUpdateUtilities(t *testing.T) {
	testLazyArenaPreservesUpdateUtilities(t, newNoLazyStringContentArenaDecoderV1, newNoLazyStringContentArenaDecoderV2)
}

func TestLazyContentStringArenaRetainsContentAcrossBlockReplacement(t *testing.T) {
	_, _, _, fullV2, _ := lazyIDArenaFixture(t)
	arena, err := decodeUpdateV2(fullV2)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	control, err := decodeUpdateWith(fullV2, newNoLazyStringContentArenaDecoderV2)
	if err != nil {
		t.Fatalf("control decode: %v", err)
	}
	if len(arena.structs) < 500 {
		t.Fatalf("fixture decoded only %d structs; want enough to replace several ContentString blocks", len(arena.structs))
	}
	if !reflect.DeepEqual(arena, control) {
		t.Fatal("decoded strings changed after later ContentString blocks replaced the arena slice")
	}
}

func TestLazyStructBlockArenaAllocatesFreshMaxSizedBlocks(t *testing.T) {
	var arena lazyStructBlockArena[contentString]
	var firstAtMax *contentString
	for i := range lazyStructBlockMax * 3 {
		content := arena.alloc()
		content.value = string(rune(i + 1))
		if len(arena.block) == lazyStructBlockMax && arena.used == 1 {
			firstAtMax = content
		}
		if firstAtMax != nil && arena.used == lazyStructBlockMax {
			break
		}
	}
	if firstAtMax == nil || len(arena.block) != lazyStructBlockMax || arena.used != lazyStructBlockMax {
		t.Fatalf("fixture did not fill a max-sized lazy struct block: len=%d used=%d", len(arena.block), arena.used)
	}
	wantFirst := *firstAtMax
	firstInReplacement := arena.alloc()
	firstInReplacement.value = "replacement"
	if firstInReplacement == firstAtMax {
		t.Fatal("lazy struct arena reused max-sized storage that was already published")
	}
	if *firstAtMax != wantFirst {
		t.Fatalf("replacing a max-sized lazy struct block mutated retained content: got %v want %v", *firstAtMax, wantFirst)
	}
}

func TestLazyContentStringArenaRemovesPerStringWrapperAllocations(t *testing.T) {
	_, _, _, fullV2, _ := lazyIDArenaFixture(t)
	arenaAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateV2(fullV2); err != nil {
			panic(err)
		}
	})
	controlAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newNoLazyStringContentArenaDecoderV2); err != nil {
			panic(err)
		}
	})
	if arenaAllocs*4 >= controlAllocs {
		t.Fatalf("ContentString blocks did not remove per-string allocations: arena %.0f control %.0f", arenaAllocs, controlAllocs)
	}
}
