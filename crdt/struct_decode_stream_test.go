package crdt

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func decodedKindForTest(t *testing.T, value abstractStruct) decodedStructKind {
	t.Helper()
	switch value.(type) {
	case *gcStruct:
		return decodedStructGC
	case *skipStruct:
		return decodedStructSkip
	case *itemStruct:
		return decodedStructItem
	default:
		t.Fatalf("unknown fixture struct %T", value)
		return decodedStructGC
	}
}

type reserveTrackingDecoder struct {
	updateDecoder
	counts []uint64
}

func (d *reserveTrackingDecoder) reserveIDs(count uint64) {
	d.counts = append(d.counts, count)
	if underlying, ok := d.updateDecoder.(interface{ reserveIDs(uint64) }); ok {
		underlying.reserveIDs(count)
	}
}

func zeroProgressItemStreamFixture(t *testing.T, declared uint64) []byte {
	t.Helper()
	column := func(payload []byte) []byte {
		return append(uvarintBytes(uint64(len(payload))), payload...)
	}
	repeatedZero := func(count uint64) []byte {
		var encoded bytes.Buffer
		writeVarIntMag(&encoded, 0, true)
		writeVarUint(&encoded, count-2)
		return encoded.Bytes()
	}
	repeatedDiffZero := func(count uint64) []byte {
		encoded := newDefaultIntDiffOptRLEEncoder()
		for i := uint64(0); i < count; i++ {
			if err := encoded.writeValue(0); err != nil {
				t.Fatal(err)
			}
		}
		result, err := encoded.bytes()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	var rest bytes.Buffer
	writeVarUint(&rest, 1)
	writeVarUint(&rest, declared)
	writeVarUint(&rest, 0)

	var update bytes.Buffer
	writeVarUint(&update, 0)
	update.Write(column(nil))                        // key clock
	update.Write(column(repeatedZero(declared + 1))) // block client + origins
	update.Write(column(repeatedDiffZero(declared))) // left clocks
	update.Write(column(nil))                        // right clocks
	update.Write(column([]byte{bit8 | refContentDeleted}))
	update.Write(column(nil)) // strings
	update.Write(column(nil)) // parent info
	update.Write(column(nil)) // type refs
	update.Write(column(repeatedZero(declared)))
	update.Write(rest.Bytes())
	return update.Bytes()
}

func TestStructDecodeStreamPreservesEveryClientBlockBoundary(t *testing.T) {
	mixed := transcriptMixedBlocks()
	blocks := []transcriptStructBlock{
		mixed[0],
		{client: 800, clock: 11}, // empty blocks are framing, not yielded values
		{
			client: 750,
			clock:  5,
			structs: []abstractStruct{
				newGC(GenID(750, 5), 0), // consumes a declaration but emits no value
				newGC(GenID(750, 5), 2),
			},
		},
		mixed[1],
	}
	codecs := []struct {
		name    string
		encoder func() updateEncoder
		decoder func([]byte) updateDecoder
	}{
		{name: "v1", encoder: newEncoderV1, decoder: newDecoderV1},
		{name: "v2", encoder: newEncoderV2, decoder: newDecoderV2},
	}

	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			update := encodeTranscriptBlocks(t, codec.encoder(), blocks)
			decoder := &decoderReadTrace{updateDecoder: codec.decoder(update)}
			stream := newStructDecodeStream(decoder, structDecodeStreamOptions{
				mode:                  structDecodeLazy,
				lenientMissingHeader:  true,
				useItemArena:          true,
				useStringContentArena: true,
				useAnyContentArena:    true,
				useFormatContentArena: true,
			})

			var clients []decodedClientTranscript
			for i, wantBlock := range blocks {
				gotBlock, ok, err := stream.NextBlock()
				if err != nil {
					t.Fatalf("block %d header: %v", i, err)
				}
				if !ok {
					t.Fatalf("block %d header missing", i)
				}
				want := structDecodeBlock{
					Client:        wantBlock.client,
					StartClock:    wantBlock.clock,
					DeclaredCount: len(wantBlock.structs),
				}
				if gotBlock != want {
					t.Fatalf("block %d header = %#v, want %#v", i, gotBlock, want)
				}

				for j := Number(0); j < gotBlock.DeclaredCount; j++ {
					result, consumed, err := stream.NextStruct()
					if err != nil {
						t.Fatalf("block %d struct %d: %v", i, j, err)
					}
					if !consumed {
						t.Fatalf("block %d stopped after %d of %d declared structs", i, j, gotBlock.DeclaredCount)
					}
					if wantKind := decodedKindForTest(t, wantBlock.structs[j]); result.Kind != wantKind {
						t.Fatalf("block %d struct %d kind=%d, want %d", i, j, result.Kind, wantKind)
					}
					if result.Value != nil {
						clients = appendClientTranscript(t, clients, result.Value)
					}
				}
				if result, consumed, err := stream.NextStruct(); err != nil || consumed || result.Value != nil {
					t.Fatalf("block %d yielded past its boundary: value=%T consumed=%v err=%v", i, result.Value, consumed, err)
				}
			}

			if block, ok, err := stream.NextBlock(); err != nil || ok {
				t.Fatalf("stream yielded an extra block: block=%#v ok=%v err=%v", block, ok, err)
			}
			remaining := decoderRemaining(decoder)
			if remaining == 0 {
				t.Fatal("stream consumed the trailing delete-set framing")
			}

			eager := eagerStructTranscript(t, update, codec.decoder)
			if eager.Failed {
				t.Fatalf("independent eager parser rejected boundary fixture: %#v", eager)
			}
			if !reflect.DeepEqual(clients, eager.Clients) {
				t.Fatalf("stream values differ from eager parser\n stream: %#v\n eager:  %#v", clients, eager.Clients)
			}
			if !reflect.DeepEqual(decoder.events, eager.Reads) {
				t.Fatalf("stream typed reads differ from eager parser\n stream: %q\n eager:  %q", decoder.events, eager.Reads)
			}
			if remaining != eager.Remaining {
				t.Fatalf("remaining bytes: stream=%d eager=%d", remaining, eager.Remaining)
			}
		})
	}
}

func TestStructDecodeStreamRefusesToDiscardUnreadStructs(t *testing.T) {
	blocks := transcriptMixedBlocks()
	update := encodeTranscriptBlocks(t, newEncoderV1(), blocks)
	stream := newStructDecodeStream(newDecoderV1(update), structDecodeStreamOptions{
		mode:                 structDecodeLazy,
		lenientMissingHeader: true,
	})
	first, ok, err := stream.NextBlock()
	if err != nil || !ok || first.DeclaredCount == 0 {
		t.Fatalf("fixture did not open a non-empty first block: block=%#v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := stream.NextBlock(); err == nil || ok || !strings.Contains(err.Error(), "structs unread") {
		t.Fatalf("advancing an unconsumed block: ok=%v err=%v", ok, err)
	}
}

func TestStructDecodeStreamPreservesModeSpecificStallBoundaries(t *testing.T) {
	const declared = 1_000
	update := zeroProgressItemStreamFixture(t, declared)

	eagerRefs, err := readClientsStructRefs(
		newDecoderV2(update),
		newDoc("eager-stall", false, nil, nil, false),
	)
	if err == nil || !strings.Contains(err.Error(), "decode stalled") {
		t.Fatalf("eager zero-progress rejection: %v", err)
	}
	eagerRef := eagerRefs[0]
	if eagerRef == nil {
		t.Fatal("eager collector did not expose the partially decoded client block")
	}
	// The first Item consumes each column's encoded run header; the following
	// maxStallIterations Items are the tolerated zero-progress window, and the
	// rejecting Item remains in the partial result because the former eager loop
	// appended before checking progress. Exercise the production bulk collector:
	// a one-at-a-time stream test would not guard the hot eager loop that
	// readClientsStructRefs actually uses.
	if got, want := len(eagerRef.refs), maxStallIterations+2; got != want {
		t.Fatalf("eager collected %d Items before rejection, want %d", got, want)
	}

	lazy := newStructDecodeStream(newDecoderV2(update), structDecodeStreamOptions{mode: structDecodeLazy})
	if _, ok, err := lazy.NextBlock(); err != nil || !ok {
		t.Fatalf("lazy block header: ok=%v err=%v", ok, err)
	}
	for i := uint64(0); i < declared; i++ {
		result, consumed, err := lazy.NextStruct()
		if err != nil || !consumed || result.Value == nil {
			t.Fatalf("lazy struct %d: value=%T consumed=%v err=%v", i, result.Value, consumed, err)
		}
	}
	if _, consumed, err := lazy.NextStruct(); err != nil || consumed {
		t.Fatalf("lazy stream did not finish exactly at the declaration: consumed=%v err=%v", consumed, err)
	}
}

func TestEagerStructStreamPreservesExactReservationAndItsBoundary(t *testing.T) {
	blocks := transcriptMixedBlocks()
	update := encodeTranscriptBlocks(t, newEncoderV1(), blocks)
	decoder := &reserveTrackingDecoder{updateDecoder: newDecoderV1(update)}
	refs, err := readClientsStructRefs(decoder, newDoc("eager-reserve", false, nil, nil, false))
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := make([]uint64, len(blocks))
	for i, block := range blocks {
		wantCounts[i] = uint64(len(block.structs))
		ref := refs[block.client]
		if ref == nil {
			t.Fatalf("block %d client %d has no collected refs", i, block.client)
		}
		if len(ref.refs) != len(block.structs) || cap(ref.refs) != len(block.structs) {
			t.Fatalf("block %d client %d refs len/cap=%d/%d, want exact %d", i, block.client, len(ref.refs), cap(ref.refs), len(block.structs))
		}
	}
	if !reflect.DeepEqual(decoder.counts, wantCounts) {
		t.Fatalf("ID reserve calls=%v, want exact block counts %v", decoder.counts, wantCounts)
	}

	var truncated bytes.Buffer
	writeVarUint(&truncated, 1) // client blocks
	writeVarUint(&truncated, 7) // structs
	writeVarUint(&truncated, 9) // client, but no base clock
	malformed := &reserveTrackingDecoder{updateDecoder: newDecoderV1(truncated.Bytes())}
	partial, err := readClientsStructRefs(malformed, newDoc("eager-reserve-error", false, nil, nil, false))
	if err == nil {
		t.Fatal("truncated base clock was accepted")
	}
	// Pinned yjs installs clientRefs only after the base clock succeeds. The old
	// eager parser exposed an empty client entry here, but its sole production
	// caller discarded the map on error; the shared stream deliberately follows
	// the reference instead of preserving that unobservable artifact.
	if len(partial) != 0 {
		t.Fatalf("truncated base clock exposed partial client refs: %#v", partial)
	}
	if want := []uint64{7}; !reflect.DeepEqual(malformed.counts, want) {
		t.Fatalf("reserve timing changed on a truncated base clock: got %v want %v", malformed.counts, want)
	}
}
