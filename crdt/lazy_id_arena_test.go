package crdt

import (
	"bytes"
	"reflect"
	"strconv"
	"testing"
	"unsafe"
)

// noLazyIDArenaDecoder deliberately hides enableLazyIDArena from the wrapped
// concrete decoder. It is the allocation-identical control implementation used
// to prove that lazy ID blocks cannot change any update utility's result.
type noLazyIDArenaDecoder struct {
	updateDecoder
}

func newNoLazyIDArenaDecoderV1(buf []byte) updateDecoder {
	return &noLazyIDArenaDecoder{updateDecoder: newUpdateDecoderV1(buf)}
}

func newNoLazyIDArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyIDArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

func lazyIDArenaFixture(t *testing.T) ([][]byte, [][]byte, []byte, []byte, *Snapshot) {
	t.Helper()
	partsV1 := make([][]byte, 3)
	partsV2 := make([][]byte, 3)
	combined := newDoc("combined", false, defaultGCFilter, nil, false, WithClientID(999))
	for part := range partsV1 {
		doc := newDoc("part", false, defaultGCFilter, nil, false, WithClientID(part+1))
		text := doc.GetText("t")
		state := uint32(part + 1)
		for i := 0; i < 256; i++ {
			state = state*1664525 + 1013904223
			index := 0
			if length := text.Length(); length != 0 {
				index = int(state % uint32(length+1))
			}
			text.Insert(index, string(rune('a'+part)), Object{})
		}

		var err error
		partsV1[part], err = EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatalf("encode V1 part %d: %v", part, err)
		}
		partsV2[part], err = EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("encode V2 part %d: %v", part, err)
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

func requireSameBytes(t *testing.T, name string, want, got []byte, wantErr, gotErr error) {
	t.Helper()
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("%s error mismatch: arena=%v control=%v", name, wantErr, gotErr)
	}
	if wantErr != nil {
		if wantErr.Error() != gotErr.Error() {
			t.Fatalf("%s errors differ: arena=%q control=%q", name, wantErr, gotErr)
		}
		return
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("%s output differs: arena=%x control=%x", name, want, got)
	}
}

func TestLazyIDArenaPreservesUpdateUtilities(t *testing.T) {
	testLazyArenaPreservesUpdateUtilities(t, newNoLazyIDArenaDecoderV1, newNoLazyIDArenaDecoderV2)
}

func testLazyArenaPreservesUpdateUtilities(t *testing.T, newControlV1, newControlV2 func([]byte) updateDecoder) {
	testLazyArenaPreservesUpdateUtilitiesWithFixture(t, newControlV1, newControlV2, lazyIDArenaFixture)
}

func testLazyArenaPreservesUpdateUtilitiesWithFixture(
	t *testing.T,
	newControlV1, newControlV2 func([]byte) updateDecoder,
	fixture func(*testing.T) ([][]byte, [][]byte, []byte, []byte, *Snapshot),
) {
	t.Helper()
	partsV1, partsV2, fullV1, fullV2, snapshot := fixture(t)
	zeroState := []byte{0}

	arena, arenaErr := encodeStateVectorFromUpdateV2(fullV2)
	control, controlErr := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newControlV2)
	requireSameBytes(t, "state-vector-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = encodeStateVectorFromUpdate(fullV1)
	control, controlErr = encodeStateVectorFromUpdateWith(fullV1, newEncoderV1, newControlV1)
	requireSameBytes(t, "state-vector-v1", arena, control, arenaErr, controlErr)

	arena, arenaErr = ConvertUpdateFormatV2ToV1(fullV2)
	control, controlErr = convertUpdateFormatWith(fullV2, newControlV2, newEncoderV1)
	requireSameBytes(t, "convert-v2-v1", arena, control, arenaErr, controlErr)

	arena, arenaErr = ConvertUpdateFormatV1ToV2(fullV1)
	control, controlErr = convertUpdateFormatWith(fullV1, newControlV1, newEncoderV2)
	requireSameBytes(t, "convert-v1-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = DiffUpdateV2(fullV2, zeroState)
	control, controlErr = diffUpdateWith(fullV2, zeroState, newControlV2, newEncoderV2)
	requireSameBytes(t, "diff-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = DiffUpdate(fullV1, zeroState)
	control, controlErr = diffUpdateWith(fullV1, zeroState, newControlV1, newEncoderV1)
	requireSameBytes(t, "diff-v1", arena, control, arenaErr, controlErr)

	arena, arenaErr = mergeUpdatesWith(partsV2, newDecoderV2, newEncoderV2)
	control, controlErr = mergeUpdatesCore(partsV2, newControlV2, newEncoderV2)
	requireSameBytes(t, "merge-v2", arena, control, arenaErr, controlErr)

	arena, arenaErr = mergeUpdatesWith(partsV1, newDecoderV1, newEncoderV1)
	control, controlErr = mergeUpdatesCore(partsV1, newControlV1, newEncoderV1)
	requireSameBytes(t, "merge-v1", arena, control, arenaErr, controlErr)

	arenaMetaFrom, arenaMetaTo, arenaErr := parseUpdateMetaWith(fullV2, newDecoderV2)
	controlMetaFrom, controlMetaTo, controlErr := parseUpdateMetaWith(fullV2, newControlV2)
	if arenaErr != nil || controlErr != nil || !reflect.DeepEqual(arenaMetaFrom, controlMetaFrom) || !reflect.DeepEqual(arenaMetaTo, controlMetaTo) {
		t.Fatalf("parse-meta-v2 differs: arena=(%v,%v,%v) control=(%v,%v,%v)", arenaMetaFrom, arenaMetaTo, arenaErr, controlMetaFrom, controlMetaTo, controlErr)
	}

	arenaContains, arenaErr := SnapshotContainsUpdateV2(snapshot, fullV2)
	controlContains, controlErr := snapshotContainsUpdateWith(snapshot, fullV2, newControlV2)
	if arenaErr != nil || controlErr != nil || arenaContains != controlContains {
		t.Fatalf("snapshot-contains-v2 differs: arena=(%v,%v) control=(%v,%v)", arenaContains, arenaErr, controlContains, controlErr)
	}
}

func TestLazyIDArenaRetainsIDsAcrossBlockReplacement(t *testing.T) {
	_, _, _, fullV2, _ := lazyIDArenaFixture(t)
	arena, err := decodeUpdateV2(fullV2)
	if err != nil {
		t.Fatalf("arena decode: %v", err)
	}
	control, err := decodeUpdateWith(fullV2, newNoLazyIDArenaDecoderV2)
	if err != nil {
		t.Fatalf("control decode: %v", err)
	}
	if len(arena.structs) < 500 {
		t.Fatalf("fixture decoded only %d structs; want enough to replace several ID blocks", len(arena.structs))
	}
	if !reflect.DeepEqual(arena, control) {
		t.Fatal("decoded structs changed after later lazy ID blocks replaced the decoder slice")
	}
}

func TestLazyIDArenaAllocatesFreshMaxSizedBlocks(t *testing.T) {
	decoder := &updateDecoderV2{}
	decoder.enableLazyIDArena()

	var firstAtMax *ID
	for range lazyIDArenaMax * 3 {
		id := decoder.allocID(-decoder.idArenaPos, -decoder.idArenaPos+1000)
		used := -decoder.idArenaPos - 1
		if len(decoder.idArena) == lazyIDArenaMax && used == 1 {
			firstAtMax = id
		}
		if firstAtMax != nil && used == lazyIDArenaMax {
			break
		}
	}
	if firstAtMax == nil || len(decoder.idArena) != lazyIDArenaMax || -decoder.idArenaPos-1 != lazyIDArenaMax {
		t.Fatalf("fixture did not fill a max-sized lazy ID block: len=%d used=%d", len(decoder.idArena), -decoder.idArenaPos-1)
	}
	wantFirst := *firstAtMax
	firstInReplacement := decoder.allocID(999_999, 888_888)
	if firstInReplacement == firstAtMax {
		t.Fatal("lazy ID arena reused max-sized storage that was already published")
	}
	if *firstAtMax != wantFirst {
		t.Fatalf("replacing a max-sized lazy ID block mutated a retained ID: got %v want %v", *firstAtMax, wantFirst)
	}
}

func TestUpdateDecoderSizeClasses(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit malloc-class assertion")
	}
	if got := unsafe.Sizeof(updateDecoderV1{}); got != 40 {
		t.Fatalf("UpdateDecoderV1 size = %d, want 40", got)
	}
	if got := unsafe.Sizeof(updateDecoderV2{}); got != 144 {
		t.Fatalf("UpdateDecoderV2 size = %d, want 144", got)
	}
	// Pointerful allocations above 512 bytes carry an 8-byte malloc header, so
	// the largest raw struct that still fits the 896-byte class is 888 bytes.
	// The current layout is 880 bytes and deliberately retains one word of room.
	if got := unsafe.Sizeof(updateDecoderV2Allocation{}); got > 888 {
		t.Fatalf("coallocated UpdateDecoderV2 state size = %d, want <= 888 so size plus the 8-byte malloc header stays within the 896-byte class", got)
	}
}

func TestLazyIDArenaAllocatesOnlyWhenIDsAreRead(t *testing.T) {
	doc := newDoc("map", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	for i := 0; i < 512; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode map update: %v", err)
	}
	decoder := newUpdateDecoderV2(update)
	reader := newLazyStructReader(decoder, false)
	for reader.curr != nil {
		reader.nextStruct()
	}
	if err := reader.decodeError(); err != nil {
		t.Fatalf("decode map update: %v", err)
	}
	if len(decoder.idArena) != 0 {
		t.Fatalf("origin-free update allocated an ID block of %d entries", len(decoder.idArena))
	}
}

func TestLazyIDArenaReducesOriginHeavyAllocations(t *testing.T) {
	_, _, _, fullV2, _ := lazyIDArenaFixture(t)
	arenaAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateV2(fullV2); err != nil {
			panic(err)
		}
	})
	controlAllocs := testing.AllocsPerRun(5, func() {
		if _, err := encodeStateVectorFromUpdateWith(fullV2, newEncoderV2, newNoLazyIDArenaDecoderV2); err != nil {
			panic(err)
		}
	})
	if arenaAllocs*5 >= controlAllocs*3 {
		t.Fatalf("lazy ID blocks did not remove most per-ID allocations: arena %.0f control %.0f", arenaAllocs, controlAllocs)
	}
}
