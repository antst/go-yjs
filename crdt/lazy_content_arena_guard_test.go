package crdt

import (
	"bytes"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

type noLazyContentArenaDecoder struct {
	updateDecoder
}

func (*noLazyContentArenaDecoder) disableLazyStringContentArena() {}
func (*noLazyContentArenaDecoder) disableLazyAnyContentArena()    {}
func (*noLazyContentArenaDecoder) disableLazyFormatContentArena() {}

func (d *noLazyContentArenaDecoder) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func newNoLazyContentArenaDecoderV2(buf []byte) updateDecoder {
	return &noLazyContentArenaDecoder{updateDecoder: newUpdateDecoderV2(buf)}
}

func lazyAnyContentBlockFixture(t *testing.T, structCount int) []byte {
	t.Helper()
	doc := newDoc("content-arena-threshold", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	for i := 0; i < structCount; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeUpdateWith(update, newNoLazyContentArenaDecoderV2)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(decoded.structs); got != structCount {
		t.Fatalf("threshold fixture decoded %d structs, want exactly %d", got, structCount)
	}
	return update
}

func contentArenaStateVectorAllocs(update []byte, decoder func([]byte) updateDecoder) float64 {
	return testing.AllocsPerRun(20, func() {
		if _, err := encodeStateVectorFromUpdateWith(update, newEncoderV2, decoder); err != nil {
			panic(err)
		}
	})
}

func TestLazyContentArenaDefersUntilSubstantialClientBlock(t *testing.T) {
	below := lazyAnyContentBlockFixture(t, 31)
	atThreshold := lazyAnyContentBlockFixture(t, 32)

	defaultDecoder := func(buf []byte) updateDecoder { return newUpdateDecoderV2(buf) }
	belowArena := contentArenaStateVectorAllocs(below, defaultDecoder)
	belowControl := contentArenaStateVectorAllocs(below, newNoLazyAnyContentArenaDecoderV2)
	if saved := belowControl - belowArena; saved > 2 {
		t.Fatalf("31-struct block unexpectedly activated content arena: arena %.0f control %.0f saved %.0f", belowArena, belowControl, saved)
	}

	thresholdArena := contentArenaStateVectorAllocs(atThreshold, defaultDecoder)
	thresholdControl := contentArenaStateVectorAllocs(atThreshold, newNoLazyAnyContentArenaDecoderV2)
	if saved := thresholdControl - thresholdArena; saved < 16 {
		t.Fatalf("32-struct block did not activate content arena: arena %.0f control %.0f saved %.0f", thresholdArena, thresholdControl, saved)
	}
}

func lazyMixedContentArenaFixture(t *testing.T) []byte {
	t.Helper()
	doc := newDoc("content-arena-concurrent", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	m := doc.GetMap("m")
	for i := 0; i < 64; i++ {
		text.Insert(0, string(rune('a'+i%26)), Object{})
		m.Set("k"+strconv.Itoa(i), i)
	}
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeUpdateWith(update, newNoLazyContentArenaDecoderV2)
	if err != nil {
		t.Fatal(err)
	}
	var strings, anys int
	for _, raw := range decoded.structs {
		item, ok := raw.(*itemStruct)
		if !ok {
			continue
		}
		switch item.content.(type) {
		case *contentString:
			strings++
		case *contentAny:
			anys++
		}
	}
	if len(decoded.structs) < 32 || strings == 0 || anys == 0 {
		t.Fatalf("concurrent fixture does not activate both content arenas: structs=%d strings=%d anys=%d", len(decoded.structs), strings, anys)
	}
	return update
}

func contentReaderPointers(readers []func(updateDecoder) (itemContent, error)) []uintptr {
	pointers := make([]uintptr, len(readers))
	for i, reader := range readers {
		pointers[i] = reflect.ValueOf(reader).Pointer()
	}
	return pointers
}

func TestLazyContentDispatchTableIsReaderLocal(t *testing.T) {
	update := lazyMixedContentArenaFixture(t)
	before := contentReaderPointers(contentRefs)
	want, err := encodeStateVectorFromUpdateV2(update)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	const rounds = 50
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				got, err := encodeStateVectorFromUpdateV2(update)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, want) {
					errs <- &contentDispatchMismatchError{}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if after := contentReaderPointers(contentRefs); !reflect.DeepEqual(after, before) {
		t.Fatal("lazy reader mutated the shared contentRefs dispatch table")
	}
}

type contentDispatchMismatchError struct{}

func (*contentDispatchMismatchError) Error() string {
	return "concurrent lazy reader returned a different state vector"
}
