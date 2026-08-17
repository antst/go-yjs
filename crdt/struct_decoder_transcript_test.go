package crdt

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// decoderReadTrace records the typed UpdateDecoder calls made by a struct
// parser. Raw client-block framing lives in RestDecoder, so block boundaries are
// checked separately through the decoded struct transcript below.
//
// Keeping this as an UpdateDecoder wrapper rather than copying either parser is
// load-bearing: the oracle observes the production eager and lazy algorithms;
// it does not reproduce their shared assumptions in a third parser.
type decoderReadTrace struct {
	updateDecoder
	events []string
}

func (d *decoderReadTrace) record(name string, value interface{}, err error) {
	if err != nil {
		d.events = append(d.events, fmt.Sprintf("%s:error:%v", name, err))
		return
	}
	switch value := value.(type) {
	case *ID:
		if value == nil {
			d.events = append(d.events, name+":<nil>")
		} else {
			d.events = append(d.events, fmt.Sprintf("%s:%d:%d", name, value.Client, value.Clock))
		}
	case []byte:
		d.events = append(d.events, fmt.Sprintf("%s:%x", name, value))
	case string:
		d.events = append(d.events, fmt.Sprintf("%s:%q", name, value))
	default:
		encoded, encodeErr := marshalJSONOrdered(value)
		if encodeErr == nil {
			d.events = append(d.events, fmt.Sprintf("%s:%s", name, encoded))
		} else {
			d.events = append(d.events, fmt.Sprintf("%s:%T:%v", name, value, value))
		}
	}
}

func (d *decoderReadTrace) RemainingLen() int { return decoderRemaining(d.updateDecoder) }

func (d *decoderReadTrace) reserveIDs(count uint64) {
	if reserver, ok := d.updateDecoder.(interface{ reserveIDs(uint64) }); ok {
		reserver.reserveIDs(count)
	}
}

func (d *decoderReadTrace) enableLazyIDArena() {
	if arena, ok := d.updateDecoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
}

func (d *decoderReadTrace) readID() (*ID, error) {
	value, err := d.updateDecoder.readID()
	d.record("id", value, err)
	return value, err
}

func (d *decoderReadTrace) readLeftID() (*ID, error) {
	value, err := d.updateDecoder.readLeftID()
	d.record("left-id", value, err)
	return value, err
}

func (d *decoderReadTrace) readRightID() (*ID, error) {
	value, err := d.updateDecoder.readRightID()
	d.record("right-id", value, err)
	return value, err
}

func (d *decoderReadTrace) readClient() (Number, error) {
	value, err := d.updateDecoder.readClient()
	d.record("client", value, err)
	return value, err
}

func (d *decoderReadTrace) readInfo() (uint8, error) {
	value, err := d.updateDecoder.readInfo()
	d.record("info", value, err)
	return value, err
}

func (d *decoderReadTrace) readStringValue() (string, error) {
	value, err := d.updateDecoder.readStringValue()
	d.record("string", value, err)
	return value, err
}

func (d *decoderReadTrace) readParentInfo() (bool, error) {
	value, err := d.updateDecoder.readParentInfo()
	d.record("parent-info", value, err)
	return value, err
}

func (d *decoderReadTrace) readTypeRef() (uint8, error) {
	value, err := d.updateDecoder.readTypeRef()
	d.record("type-ref", value, err)
	return value, err
}

func (d *decoderReadTrace) readLength() (Number, error) {
	value, err := d.updateDecoder.readLength()
	d.record("len", value, err)
	return value, err
}

func (d *decoderReadTrace) readAnyValue() (interface{}, error) {
	value, err := d.updateDecoder.readAnyValue()
	d.record("any", value, err)
	return value, err
}

func (d *decoderReadTrace) readBuffer() ([]uint8, error) {
	value, err := d.updateDecoder.readBuffer()
	d.record("buf", value, err)
	return value, err
}

func (d *decoderReadTrace) readJSONValue() (interface{}, error) {
	value, err := d.updateDecoder.readJSONValue()
	d.record("json", value, err)
	return value, err
}

func (d *decoderReadTrace) readKey() (string, error) {
	value, err := d.updateDecoder.readKey()
	d.record("key", value, err)
	return value, err
}

type decodedStructTranscript struct {
	Client     Number
	Clock      Number
	Length     Number
	Kind       string
	ContentRef int
	BodyV1     string
}

type decodedClientTranscript struct {
	Client  Number
	Structs []decodedStructTranscript
}

type structParserTranscript struct {
	Clients   []decodedClientTranscript
	Reads     []string
	Remaining int
	Failed    bool
	Error     string
}

func canonicalDecodedStruct(t *testing.T, value abstractStruct) decodedStructTranscript {
	t.Helper()
	encoder := newUpdateEncoderV1()
	if err := value.writeStruct(encoder, 0); err != nil {
		t.Fatalf("canonical re-encode %T %v: %v", value, value.getID(), err)
	}
	id := value.getID()
	record := decodedStructTranscript{
		Client: id.Client,
		Clock:  id.Clock,
		Length: value.structLength(),
		// Keep the pre-privatization semantic names in the frozen transcript.
		// Go identifier casing is not part of the wire grammar this digest pins.
		Kind:       historicalStructKind(value),
		ContentRef: -1,
		BodyV1:     fmt.Sprintf("%x", encoder.toBytes()),
	}
	if item, ok := value.(*itemStruct); ok {
		record.ContentRef = int(item.content.contentRef())
	}
	return record
}

func historicalStructKind(value abstractStruct) string {
	switch value.(type) {
	case *itemStruct:
		return "*y_crdt.Item"
	case *gcStruct:
		return "*y_crdt.GC"
	case *skipStruct:
		return "*y_crdt.Skip"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func appendClientTranscript(
	t *testing.T,
	clients []decodedClientTranscript,
	value abstractStruct,
) []decodedClientTranscript {
	t.Helper()
	record := canonicalDecodedStruct(t, value)
	if len(clients) == 0 || clients[len(clients)-1].Client != record.Client {
		clients = append(clients, decodedClientTranscript{Client: record.Client})
	}
	last := &clients[len(clients)-1]
	last.Structs = append(last.Structs, record)
	return clients
}

func eagerStructTranscript(t *testing.T, update []byte, factory func([]byte) updateDecoder) structParserTranscript {
	t.Helper()
	decoder := &decoderReadTrace{updateDecoder: factory(update)}
	refs, err := readClientsStructRefs(decoder, newDoc("transcript-eager", false, nil, nil, false, WithClientID(9001)))
	clients := make([]decodedClientTranscript, 0, len(refs))
	clientIDs := make([]Number, 0, len(refs))
	for client, ref := range refs {
		if len(ref.refs) > 0 {
			clientIDs = append(clientIDs, client)
		}
	}
	// Valid updates write client blocks in descending order. The eager API is a
	// map and intentionally discards wire block order; canonicalize that API in
	// the same order so only parser semantics are compared here. The read trace
	// independently checks the actual ReadClient order.
	sort.Sort(sort.Reverse(numberSlice(clientIDs)))
	for _, client := range clientIDs {
		block := decodedClientTranscript{Client: client}
		for _, value := range refs[client].refs {
			block.Structs = append(block.Structs, canonicalDecodedStruct(t, value))
		}
		clients = append(clients, block)
	}
	transcript := structParserTranscript{
		Clients:   clients,
		Reads:     append([]string(nil), decoder.events...),
		Remaining: decoderRemaining(decoder),
		Failed:    err != nil,
	}
	if err != nil {
		transcript.Error = err.Error()
	}
	return transcript
}

func lazyStructTranscript(t *testing.T, update []byte, factory func([]byte) updateDecoder) structParserTranscript {
	t.Helper()
	decoder := &decoderReadTrace{updateDecoder: factory(update)}
	reader := newLazyStructReader(decoder, false)
	clients := make([]decodedClientTranscript, 0)
	seen := make(map[Number]bool)
	for value := reader.curr; value != nil; value = reader.nextStruct() {
		client := value.getID().Client
		if (len(clients) == 0 || clients[len(clients)-1].Client != client) && seen[client] {
			t.Fatalf("fixture has repeated client block %d; eager map API cannot preserve that boundary", client)
		}
		if len(clients) == 0 || clients[len(clients)-1].Client != client {
			seen[client] = true
		}
		clients = appendClientTranscript(t, clients, value)
	}
	err := reader.decodeError()
	transcript := structParserTranscript{
		Clients:   clients,
		Reads:     append([]string(nil), decoder.events...),
		Remaining: decoderRemaining(decoder),
		Failed:    err != nil,
	}
	if err != nil {
		transcript.Error = err.Error()
	}
	return transcript
}

type transcriptStructBlock struct {
	client  Number
	clock   Number
	structs []abstractStruct
}

func transcriptMixedBlocks() []transcriptStructBlock {
	root := newYString("transcript-root")
	jsonObject := newObject()
	jsonObject.Set("z", 1)
	jsonObject.Set("a", "two")
	formatObject := newObject()
	formatObject.Set("color", "red")
	subdoc := newDoc("transcript-subdoc", false, nil, nil, true, WithClientID(771))

	contents := []itemContent{
		newContentDeleted(2),
		newContentJSON(ArrayAny{Undefined, jsonObject}),
		newContentBinary([]byte{0, 1, 2, 255}),
		newContentString("ascii-λ-😀"),
		newContentEmbed(jsonObject),
		newContentFormat("bold", formatObject),
		newContentType(NewYArray()),
		newContentAny(ArrayAny{nil, true, 17, "any", jsonObject}),
		newContentDoc(subdoc),
	}

	client := Number(900)
	clock := Number(4)
	first := make([]abstractStruct, 0, len(contents)+2)
	for i, content := range contents {
		var origin, rightOrigin *ID
		parent := interface{}(root)
		parentSub := ""
		switch i {
		case 0:
			parentSub = "map-key"
		case 1:
			parent = &ID{Client: 12, Clock: 3}
		case 2:
			id := ID{Client: 44, Clock: 5}
			origin = &id
		case 3:
			id := ID{Client: 45, Clock: 6}
			rightOrigin = &id
		case 4:
			left := ID{Client: 46, Clock: 7}
			right := ID{Client: 47, Clock: 8}
			origin, rightOrigin = &left, &right
		}
		item := newItem(GenID(client, clock), nil, origin, nil, rightOrigin, parent, parentSub, content)
		first = append(first, item)
		clock += item.structLength()
	}
	first = append(first, newGC(GenID(client, clock), 3))
	clock += 3
	first = append(first, newSkip(GenID(client, clock), 2))
	clock += 2
	// Cross the lazy dispatch-table threshold so the transcript covers the
	// reader-local arena implementations as well as the shared contentRefs table.
	for i := 0; i < 21; i++ {
		if i%2 == 0 {
			first = append(first, newGC(GenID(client, clock), 1))
		} else {
			first = append(first, newSkip(GenID(client, clock), 1))
		}
		clock++
	}

	secondClient := Number(700)
	second := []abstractStruct{
		newItem(GenID(secondClient, 0), nil, nil, nil, nil, newYString("other-root"), "", newContentString("tail")),
	}
	return []transcriptStructBlock{
		{client: client, clock: 4, structs: first},
		{client: secondClient, clock: 0, structs: second},
	}
}

func encodeTranscriptBlocks(t *testing.T, encoder updateEncoder, blocks []transcriptStructBlock) []byte {
	t.Helper()
	writeEncoderRestVarUint(encoder, uint64(len(blocks)))
	for _, block := range blocks {
		writeEncoderRestVarUint(encoder, uint64(len(block.structs)))
		encoder.writeClient(block.client)
		writeEncoderRestVarUint(encoder, uint64(block.clock))
		for _, value := range block.structs {
			if err := value.writeStruct(encoder, 0); err != nil {
				t.Fatalf("encode transcript fixture %T: %v", value, err)
			}
		}
	}
	writeEncoderRestVarUint(encoder, 0) // empty delete set
	result := append([]byte(nil), encoder.toBytes()...)
	if err := encoder.encodeError(); err != nil {
		t.Fatalf("encode transcript fixture: %v", err)
	}
	return result
}

func requireEquivalentStructTranscripts(t *testing.T, eager, lazy structParserTranscript) {
	t.Helper()
	if !reflect.DeepEqual(eager.Clients, lazy.Clients) {
		t.Fatalf("decoded struct transcript differs\n  eager: %#v\n  lazy:  %#v", eager.Clients, lazy.Clients)
	}
	if !reflect.DeepEqual(eager.Reads, lazy.Reads) {
		t.Fatalf("decoder read transcript differs\n  eager: %q\n  lazy:  %q", eager.Reads, lazy.Reads)
	}
	if eager.Remaining != lazy.Remaining {
		t.Fatalf("decoder remaining bytes: eager=%d lazy=%d", eager.Remaining, lazy.Remaining)
	}
	if eager.Failed != lazy.Failed {
		t.Fatalf("parser failure differs: eager=%v lazy=%v", eager.Failed, lazy.Failed)
	}
}

// requireStructTranscriptParity runs on a bounded prefix of the yjs-generated
// differential corpus (see TestFuzzGate). Keeping it here means future parser
// refactors are checked against both the adversarial fixture below and update
// shapes that neither implementation's author selected.
func requireStructTranscriptParity(
	t *testing.T,
	label string,
	update []byte,
	factory func([]byte) updateDecoder,
) {
	t.Helper()
	eager := eagerStructTranscript(t, update, factory)
	lazy := lazyStructTranscript(t, update, factory)
	if !reflect.DeepEqual(eager, lazy) {
		t.Fatalf("%s eager/lazy struct transcript differs\n  eager: %#v\n  lazy:  %#v", label, eager, lazy)
	}
	if eager.Failed {
		t.Fatalf("%s valid corpus update failed struct decoding: %#v", label, eager)
	}
}

func requirePreExtractionMixedTranscript(t *testing.T, transcript structParserTranscript) {
	t.Helper()
	payload, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("marshal struct transcript: %v", err)
	}
	const want = "f48d59717d8e3672af4f2a700e76d84c4ec9ed6eb653f4ac56b2c896ecdbf58c"
	got := fmt.Sprintf("%x", sha256.Sum256(payload))
	if got != want {
		t.Fatalf("mixed transcript changed from the two-parser baseline: digest=%s want=%s\ntranscript=%#v", got, want, transcript)
	}
}

func requirePreExtractionMixedFixture(t *testing.T, codec string, update []byte) {
	t.Helper()
	wants := map[string]string{
		"v1": "ba828e0099cc730f1c027686df3b5b837f241353fbef797482fdf02c9bc380ce",
		"v2": "7b06035b9f465fd46c2757a525e4346eb81cb0c804096b51c1c127043a1351b1",
	}
	want, ok := wants[codec]
	if !ok {
		t.Fatalf("no frozen mixed-fixture digest for codec %q", codec)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(update))
	if got != want {
		t.Fatalf("mixed fixture bytes changed: codec=%s digest=%s want=%s", codec, got, want)
	}
}

func requirePreStreamEagerTruncations(t *testing.T, codec string, transcripts []structParserTranscript) {
	t.Helper()
	wants := map[string]string{
		"v1": "eeec4e9ed78ddd8872dc7100f4cb7463df599f370a824ebbbefd582b22911dba",
		"v2": "fa4364c96c863072e6e06d71a5f3946a433b8010d8d085b2b032ffc130209c9c",
	}
	want, ok := wants[codec]
	if !ok {
		t.Fatalf("no frozen eager-truncation digest for codec %q", codec)
	}
	// Error wording belongs to the collector adapter, not the wire grammar. The
	// frozen boundary covers every yielded value, typed read, remaining byte and
	// success/failure decision while allowing the adapter to add its own context.
	for i := range transcripts {
		transcripts[i].Error = ""
	}
	payload, err := json.Marshal(transcripts)
	if err != nil {
		t.Fatalf("marshal eager truncations: %v", err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(payload))
	if got != want {
		t.Fatalf("eager truncation boundaries changed from the independent-parser baseline: codec=%s digest=%s want=%s", codec, got, want)
	}
}

func TestEagerAndLazyStructDecodersHaveIdenticalTranscripts(t *testing.T) {
	blocks := transcriptMixedBlocks()
	codecs := []struct {
		name    string
		encoder func() updateEncoder
		decoder func([]byte) updateDecoder
	}{
		{name: "v1", encoder: newEncoderV1, decoder: newDecoderV1},
		{name: "v2", encoder: newEncoderV2, decoder: newDecoderV2},
	}
	codecClients := make(map[string][]decodedClientTranscript, len(codecs))
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			update := encodeTranscriptBlocks(t, codec.encoder(), blocks)
			requirePreExtractionMixedFixture(t, codec.name, update)
			eager := eagerStructTranscript(t, update, codec.decoder)
			lazy := lazyStructTranscript(t, update, codec.decoder)
			requireEquivalentStructTranscripts(t, eager, lazy)
			requirePreExtractionMixedTranscript(t, eager)
			if eager.Failed {
				t.Fatal("valid mixed fixture failed to decode")
			}
			if eager.Remaining == 0 {
				t.Fatal("struct parser consumed the delete-set framing")
			}
			codecClients[codec.name] = eager.Clients

			refs := make(map[uint8]bool)
			kinds := make(map[string]bool)
			for _, block := range eager.Clients {
				for _, value := range block.Structs {
					kinds[value.Kind] = true
					if value.ContentRef >= 0 {
						refs[uint8(value.ContentRef)] = true
					}
				}
			}
			for ref := uint8(refContentDeleted); ref <= refContentDoc; ref++ {
				if !refs[ref] {
					t.Fatalf("fixture does not cover content ref %d", ref)
				}
			}
			if !kinds["*y_crdt.GC"] || !kinds["*y_crdt.Skip"] || !kinds["*y_crdt.Item"] {
				t.Fatalf("fixture struct kinds = %v, want Item, GC and Skip", kinds)
			}
			for _, required := range []string{
				"left-id", "right-id", "client", "info", "string", "parent-info",
				"type-ref", "len", "any", "buf", "json", "key",
			} {
				found := false
				for _, event := range eager.Reads {
					if len(event) >= len(required)+1 && event[:len(required)+1] == required+":" {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("fixture never exercised decoder method %s; reads=%q", required, eager.Reads)
				}
			}
		})
	}
	if v1, ok := codecClients["v1"]; ok {
		if v2, ok := codecClients["v2"]; ok && !reflect.DeepEqual(v1, v2) {
			t.Fatalf("V1/V2 canonical decoded structs differ\n  v1: %#v\n  v2: %#v", v1, v2)
		}
	}
}

func TestEagerAndLazyStructDecodersPreserveMissingHeaderPolicy(t *testing.T) {
	codecs := []struct {
		name    string
		decoder func([]byte) updateDecoder
	}{
		{name: "v1", decoder: newDecoderV1},
		{name: "v2", decoder: newDecoderV2},
	}
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			eager := eagerStructTranscript(t, nil, codec.decoder)
			lazy := lazyStructTranscript(t, nil, codec.decoder)
			if !eager.Failed || lazy.Failed {
				t.Fatalf("missing header policy changed: eager=%#v lazy=%#v", eager, lazy)
			}
			if len(eager.Clients) != 0 || len(lazy.Clients) != 0 ||
				len(eager.Reads) != 0 || len(lazy.Reads) != 0 || eager.Remaining != lazy.Remaining {
				t.Fatalf("missing header produced parser events: eager=%#v lazy=%#v", eager, lazy)
			}
		})
	}
}

func TestEagerAndLazyStructDecodersRejectTruncationAtTheSamePrimitive(t *testing.T) {
	blocks := transcriptMixedBlocks()
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
			lenientMissingHeader := 0
			eagerTruncations := make([]structParserTranscript, 0, len(update))
			for cut := 1; cut <= len(update); cut++ {
				prefix := append([]byte(nil), update[:cut]...)
				eager := eagerStructTranscript(t, prefix, codec.decoder)
				eagerTruncations = append(eagerTruncations, eager)
				lazy := lazyStructTranscript(t, prefix, codec.decoder)
				// Lazy update utilities historically accept a decoder with no outer
				// struct header as an empty struct stream; eager Apply rejects it. The
				// shared stream must keep this as an adapter policy, not accidentally
				// erase it while unifying the grammar.
				if eager.Failed && !lazy.Failed && len(eager.Clients) == 0 && len(lazy.Clients) == 0 &&
					len(eager.Reads) == 0 && len(lazy.Reads) == 0 && eager.Remaining == lazy.Remaining {
					lenientMissingHeader++
					continue
				}
				if !reflect.DeepEqual(eager.Clients, lazy.Clients) ||
					!reflect.DeepEqual(eager.Reads, lazy.Reads) ||
					eager.Remaining != lazy.Remaining || eager.Failed != lazy.Failed {
					t.Fatalf("cut %d/%d differs\n  eager: %#v\n  lazy:  %#v", cut, len(update), eager, lazy)
				}
			}
			if codec.name == "v2" && lenientMissingHeader == 0 {
				t.Fatal("V2 truncation fixture never exercised the lazy missing-header policy")
			}
			requirePreStreamEagerTruncations(t, codec.name, eagerTruncations)
		})
	}
}

func TestStructDecoderTranscriptMasksInfoBeforeClassification(t *testing.T) {
	// High bits are meaningless on a Skip, but a hostile sender can set them.
	// Both parsers must classify the low five bits first. A former lazy parser
	// compared the full byte and tried to read this as an Item instead.
	var rest bytes.Buffer
	writeVarUint(&rest, 1) // client blocks
	writeVarUint(&rest, 1) // structs
	writeVarUint(&rest, 9) // client
	writeVarUint(&rest, 0) // clock
	writeByte(&rest, structSkipRefNumber|bit8)
	writeVarUint(&rest, 3) // Skip length
	writeVarUint(&rest, 0) // empty delete set

	update := rest.Bytes()
	eager := eagerStructTranscript(t, update, newDecoderV1)
	lazy := lazyStructTranscript(t, update, newDecoderV1)
	requireEquivalentStructTranscripts(t, eager, lazy)
	if eager.Failed || len(eager.Clients) != 1 || len(eager.Clients[0].Structs) != 1 ||
		eager.Clients[0].Structs[0].Kind != "*y_crdt.Skip" {
		t.Fatalf("hostile info did not decode as one Skip: %#v", eager)
	}
}

func TestStructDecoderTranscriptRejectsInfoBitOutsideContentMask(t *testing.T) {
	// bit5 is the first bit outside bits4 but inside the five-bit struct/content
	// discriminator. Masking with bits4 would misclassify this hostile ref 16 as
	// GC and accept the following length instead of rejecting the invalid ref.
	var rest bytes.Buffer
	writeVarUint(&rest, 1) // client blocks
	writeVarUint(&rest, 1) // structs
	writeVarUint(&rest, 9) // client
	writeVarUint(&rest, 0) // clock
	writeByte(&rest, bit5)
	writeVarUint(&rest, 1) // would be a GC length under the wrong bits4 mask
	writeVarUint(&rest, 0) // empty delete set

	update := rest.Bytes()
	eager := eagerStructTranscript(t, update, newDecoderV1)
	lazy := lazyStructTranscript(t, update, newDecoderV1)
	if !eager.Failed || !lazy.Failed {
		t.Fatalf("invalid content ref was accepted: eager=%#v lazy=%#v", eager, lazy)
	}
	if len(eager.Clients) != 0 || len(lazy.Clients) != 0 {
		t.Fatalf("invalid content ref yielded structs: eager=%#v lazy=%#v", eager, lazy)
	}
}

func TestStructDecodeStreamDocumentsOversizedCountBoundaries(t *testing.T) {
	// Pinned yjs 13.6.31 rejects a per-client count at or above 2^32 at
	// new Array(count), before reading client or clock, so the shared stream's
	// JavaScript-array-length clamp is reference-faithful across the whole range,
	// not only at the Number safe-integer ceiling. Yjs reaches the first
	// inner-count read for an oversized outer count; Go deliberately rejects that
	// count earlier at the same JavaScript safe-integer ceiling. The earlier
	// boundary rejects nothing Yjs can represent faithfully, and neither path
	// yields a struct or performs a typed field read. Both eager and lazy adapters
	// must expose these same stream decisions rather than reviving either former
	// loop's local behavior.
	tests := []struct {
		name       string
		writeFrame func(*bytes.Buffer)
		error      string
		remaining  int
	}{
		{
			name: "outer-client-count",
			writeFrame: func(rest *bytes.Buffer) {
				writeVarUint(rest, 1<<53)
			},
			error: "number of state updates",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rest bytes.Buffer
			tt.writeFrame(&rest)
			eager := eagerStructTranscript(t, rest.Bytes(), newDecoderV1)
			lazy := lazyStructTranscript(t, rest.Bytes(), newDecoderV1)
			if !eager.Failed || !lazy.Failed {
				t.Fatalf("oversized count was accepted: eager=%#v lazy=%#v", eager, lazy)
			}
			if !strings.Contains(eager.Error, tt.error) || !strings.Contains(lazy.Error, tt.error) {
				t.Fatalf("unexpected rejection boundary: eager=%#v lazy=%#v", eager, lazy)
			}
			if len(eager.Reads) != 0 || len(lazy.Reads) != 0 ||
				eager.Remaining != tt.remaining || lazy.Remaining != tt.remaining {
				t.Fatalf("unexpected consumption boundary: eager=%#v lazy=%#v", eager, lazy)
			}
		})
	}

	for _, count := range []uint64{1 << 32, 1 << 52, 1 << 53} {
		t.Run(fmt.Sprintf("per-client-struct-count-%d", count), func(t *testing.T) {
			var rest bytes.Buffer
			writeVarUint(&rest, 1)
			writeVarUint(&rest, count)
			writeVarUint(&rest, 9)
			writeVarUint(&rest, 0)
			eager := eagerStructTranscript(t, rest.Bytes(), newDecoderV1)
			lazy := lazyStructTranscript(t, rest.Bytes(), newDecoderV1)
			if !eager.Failed || !lazy.Failed ||
				!strings.Contains(eager.Error, "number of structs") ||
				!strings.Contains(lazy.Error, "number of structs") {
				t.Fatalf("oversized per-client count was not rejected at its header: eager=%#v lazy=%#v", eager, lazy)
			}
			if len(eager.Reads) != 0 || len(lazy.Reads) != 0 || eager.Remaining != 2 || lazy.Remaining != 2 {
				t.Fatalf("oversized per-client count consumed client/clock fields: eager=%#v lazy=%#v", eager, lazy)
			}
		})
	}

	t.Run("per-client-array-limit-is-inclusive", func(t *testing.T) {
		var rest bytes.Buffer
		writeVarUint(&rest, 1)
		writeVarUint(&rest, uint64(maxYjsClientStructCount))
		writeVarUint(&rest, 9)
		writeVarUint(&rest, 0)
		eager := eagerStructTranscript(t, rest.Bytes(), newDecoderV1)
		lazy := lazyStructTranscript(t, rest.Bytes(), newDecoderV1)
		if !eager.Failed || !lazy.Failed || len(eager.Reads) == 0 || len(lazy.Reads) == 0 {
			t.Fatalf("largest valid JavaScript array length did not advance past the count: eager=%#v lazy=%#v", eager, lazy)
		}
	})
}
