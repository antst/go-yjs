package crdt

import (
	"reflect"
	"testing"
)

type noLazyAnyContentArenaDecoder struct {
	updateDecoder
}

func (*noLazyAnyContentArenaDecoder) disableLazyAnyContentArena() {}

func (d *noLazyAnyContentArenaDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func newNoLazyAnyContentArenaDecoderV1(buf []byte) updateDecoder {
	return &noLazyAnyContentArenaDecoder{updateDecoder: newUpdateDecoderV1(buf)}
}

func newNoLazyAnyContentArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyAnyContentArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

func TestLazyContentAnyArenaPreservesMapUpdateUtilities(t *testing.T) {
	testLazyMapArenaPreservesUpdateUtilities(t, newNoLazyAnyContentArenaDecoderV1, newNoLazyAnyContentArenaDecoderV2)
}

func TestLazyContentAnyArenaRetainsContentAcrossBlockReplacement(t *testing.T) {
	_, fullV2 := lazyItemArenaMapFixture(t)
	arena, err := decodeUpdateV2(fullV2)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	control, err := decodeUpdateWith(fullV2, newNoLazyAnyContentArenaDecoderV2)
	if err != nil {
		t.Fatalf("control decode: %v", err)
	}
	if len(arena.structs) < 500 {
		t.Fatalf("fixture decoded only %d structs; want enough to replace several ContentAny blocks", len(arena.structs))
	}
	if !reflect.DeepEqual(arena, control) {
		t.Fatal("decoded values changed after later ContentAny blocks replaced the arena slice")
	}
}

func TestLazyContentAnyArenaRemovesPerContentWrapperAllocations(t *testing.T) {
	_, fullV2 := lazyItemArenaMapFixture(t)
	arenaAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateV2(fullV2); err != nil {
			panic(err)
		}
	})
	controlAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newNoLazyAnyContentArenaDecoderV2); err != nil {
			panic(err)
		}
	})
	if arenaAllocs*100 >= controlAllocs*85 {
		t.Fatalf("ContentAny blocks did not remove per-wrapper allocations: arena %.0f control %.0f", arenaAllocs, controlAllocs)
	}
}
