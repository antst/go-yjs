package crdt

import (
	"reflect"
	"strconv"
	"testing"
)

type noLazyItemArenaDecoder struct {
	updateDecoder
}

func (*noLazyItemArenaDecoder) disableLazyItemArena() {}

func (d *noLazyItemArenaDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func newNoLazyItemArenaDecoderV1(buf []byte) updateDecoder {
	return &noLazyItemArenaDecoder{updateDecoder: newUpdateDecoderV1(buf)}
}

func newNoLazyItemArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyItemArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

func lazyItemArenaMapFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	doc := newDoc("map", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	for i := 0; i < 768; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	v1, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v1, v2
}

func TestLazyItemArenaPreservesMapUpdateUtilities(t *testing.T) {
	testLazyMapArenaPreservesUpdateUtilities(t, newNoLazyItemArenaDecoderV1, newNoLazyItemArenaDecoderV2)
}

func testLazyMapArenaPreservesUpdateUtilities(t *testing.T, newControlV1, newControlV2 func([]byte) updateDecoder) {
	t.Helper()
	fullV1, fullV2 := lazyItemArenaMapFixture(t)
	zeroState := []byte{0}

	arena, arenaErr := encodeStateVectorFromUpdateV2(fullV2)
	control, controlErr := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newControlV2)
	requireSameBytes(t, "state-vector-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = ConvertUpdateFormatV2ToV1(fullV2)
	control, controlErr = convertUpdateFormatWith(fullV2, newControlV2, newEncoderV1)
	requireSameBytes(t, "convert-v2-v1", arena, control, arenaErr, controlErr)

	arena, arenaErr = ConvertUpdateFormatV1ToV2(fullV1)
	control, controlErr = convertUpdateFormatWith(fullV1, newControlV1, newEncoderV2)
	requireSameBytes(t, "convert-v1-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = DiffUpdateV2(fullV2, zeroState)
	control, controlErr = diffUpdateWith(fullV2, zeroState, newControlV2, newEncoderV2)
	requireSameBytes(t, "diff-v2", arena, control, arenaErr, controlErr)
}

func TestLazyItemArenaRetainsItemsAcrossBlockReplacement(t *testing.T) {
	_, fullV2 := lazyItemArenaMapFixture(t)
	arena, err := decodeUpdateV2(fullV2)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	control, err := decodeUpdateWith(fullV2, newNoLazyItemArenaDecoderV2)
	if err != nil {
		t.Fatalf("control decode: %v", err)
	}
	if len(arena.structs) < 500 {
		t.Fatalf("fixture decoded only %d structs; want enough to replace several Item blocks", len(arena.structs))
	}
	if !reflect.DeepEqual(arena, control) {
		t.Fatal("decoded Items changed after later lazy Item blocks replaced the arena slice")
	}
}

func TestLazyItemArenaReducesAllocations(t *testing.T) {
	_, fullV2 := lazyItemArenaMapFixture(t)
	arenaAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateV2(fullV2); err != nil {
			panic(err)
		}
	})
	controlAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newNoLazyItemArenaDecoderV2); err != nil {
			panic(err)
		}
	})
	if arenaAllocs*100 >= controlAllocs*85 {
		t.Fatalf("lazy Item blocks did not remove per-Item allocations: arena %.0f control %.0f", arenaAllocs, controlAllocs)
	}
}
