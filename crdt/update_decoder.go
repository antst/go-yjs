package crdt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// ---------------------------------------------------------------- from update_decoder_v1.go
type updateDecoderV1 struct {
	rest       *bytes.Buffer
	idArena    []ID
	idArenaPos int
}

const (
	// Start at one common origin/right-origin pair so tiny updates do not pay a
	// page-sized reservation. Cap each independently retained block at 32 KiB;
	// large streams still allocate O(actual IDs) while wasting at most one block.
	lazyIDArenaInitial = 2
	lazyIDArenaMax     = 2048
)

func (v1 *updateDecoderV1) enableLazyIDArena() {
	enableLazyIDBlocks(v1.idArena, &v1.idArenaPos)
}

func (v1 *updateDecoderV1) reserveIDs(structs uint64) {
	if structs <= uint64(^uint(0)>>1)/2 {
		v1.idArena = make([]ID, int(structs)*2)
		v1.idArenaPos = 0
	}
}

func (v1 *updateDecoderV1) allocID(client, clock Number) *ID {
	return allocDecodedID(&v1.idArena, &v1.idArenaPos, client, clock)
}

func enableLazyIDBlocks(arena []ID, pos *int) {
	if len(arena) == 0 && *pos == 0 {
		// A negative position selects geometric lazy blocks. Encode the used
		// count as -pos-1 so the decoder keeps its historical size; adding a
		// separate mode/capacity field moves UpdateDecoderV2 into the next malloc
		// class on 64-bit systems.
		*pos = -1
	}
}

func allocDecodedID(arena *[]ID, pos *int, client, clock Number) *ID {
	if *pos >= 0 {
		if *pos < len(*arena) {
			index := *pos
			id := &(*arena)[index]
			*pos = index + 1
			id.Client, id.Clock = client, clock
			return id
		}
		return &ID{Client: client, Clock: clock}
	}

	used := -*pos - 1
	if used == len(*arena) {
		next := lazyIDArenaInitial
		if used != 0 {
			next = min(used*2, lazyIDArenaMax)
		}
		// Never rewind or reuse a published block. Replacing this slice header
		// leaves the old fixed array alive through every ID pointer handed to a
		// retained Item, while the next block grows independently.
		*arena = make([]ID, next)
		used = 0
	}
	id := &(*arena)[used]
	*pos = -(used + 2)
	id.Client, id.Clock = client, clock
	return id
}

// restDecoder returns the underlying rest buffer (satisfies DSDecoder).
func (v1 *updateDecoderV1) restDecoder() *bytes.Buffer {
	return v1.rest
}

// RemainingLen reports undecoded bytes left. For V1 every per-struct field lives
// in the single rest buffer, so this is just its length. Used by
// readClientsStructRefs as a progress watchdog against corrupt struct counts.
func (v1 *updateDecoderV1) RemainingLen() int {
	return v1.rest.Len()
}

// resetDS resets the current value of DeleteSet.
func (v1 *updateDecoderV1) resetDS() {
	// nop
}

// readDSClock reads the clock value of DeleteSet.
//
// The decoded varuint is run through toNumber so a value in [2^63, 2^64) is
// rejected rather than cast to a NEGATIVE Number(int). A negative DS clock
// otherwise passes the `clock < state` test in ReadAndApplyDeleteSet for a
// client absent from the store (state==0, neg<0) and crashes on a nil struct
// slice (A#1). Real clocks are < 2^53, so this never false-rejects.
func (v1 *updateDecoderV1) readDSClock() (Number, error) {
	number, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return 0, err
	}

	return toNumber(number)
}

// readDSLength reads the length of DeleteSet. Clamped via toNumber (see ReadDsClock).
func (v1 *updateDecoderV1) readDSLength() (Number, error) {
	number, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return 0, err
	}

	return toNumber(number)
}

// readID reads the ID of Item. Both the client and clock varuints are clamped
// via toNumber so a value in [2^63, 2^64) is rejected rather than wrapped to a
// negative Number (the negative-wrap class, N1).
func (v1 *updateDecoderV1) readID() (*ID, error) {
	rawClient, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return nil, err
	}
	client, err := toNumber(rawClient)
	if err != nil {
		return nil, err
	}

	rawClock, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return nil, err
	}
	clock, err := toNumber(rawClock)
	if err != nil {
		return nil, err
	}

	return v1.allocID(client, clock), nil
}

// readLeftID reads the left ID of Item.
func (v1 *updateDecoderV1) readLeftID() (*ID, error) {
	return v1.readID()
}

// readRightID reads the right ID of Item.
func (v1 *updateDecoderV1) readRightID() (*ID, error) {
	return v1.readID()
}

// readClient reads the client of Item. Clamped via toNumber (see ReadDsClock).
func (v1 *updateDecoderV1) readClient() (Number, error) {
	number, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return 0, err
	}

	return toNumber(number)
}

// readInfo reads the info of Item.
func (v1 *updateDecoderV1) readInfo() (uint8, error) {
	return v1.rest.ReadByte()
}

// readStringValue reads the string of Item.
func (v1 *updateDecoderV1) readStringValue() (string, error) {
	return readString(v1.rest)
}

// readParentInfo reads the parent info of Item.
func (v1 *updateDecoderV1) readParentInfo() (bool, error) {
	info, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return false, err
	}

	return info == 1, nil
}

// readTypeRef reads the type ref of Item.
func (v1 *updateDecoderV1) readTypeRef() (uint8, error) {
	ref, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return 0, err
	}

	return uint8(ref), nil
}

// readLength reads the length of Item. Clamped via toNumber (see ReadDsClock).
func (v1 *updateDecoderV1) readLength() (Number, error) {
	length, err := binary.ReadUvarint(v1.rest)
	if err != nil {
		return 0, err
	}

	return toNumber(length)
}

// readAnyValue reads the any of Item.
func (v1 *updateDecoderV1) readAnyValue() (any, error) {
	return readAny(v1.rest)
}

// readBuffer reads the buf of Item.
func (v1 *updateDecoderV1) readBuffer() ([]uint8, error) {
	data, err := readVarUint8Array(v1.rest)
	if err != nil {
		return nil, err
	}

	return data.([]uint8), nil
}

// readJSONValue reads the json of Item. Parses into the ordered Object type so a
// multi-key object's on-wire key order survives into re-encoding (byte-parity).
func (v1 *updateDecoderV1) readJSONValue() (interface{}, error) {
	data, err := v1.readStringValue()
	if err != nil {
		return nil, err
	}

	return unmarshalJSONOrdered([]byte(data))
}

// readKey reads the key of Item.
func (v1 *updateDecoderV1) readKey() (string, error) {
	return v1.readStringValue()
}

// newUpdateDecoderV1 creates a new UpdateDecoderV1.
func newUpdateDecoderV1(buf []byte) *updateDecoderV1 {
	return &updateDecoderV1{
		rest: bytes.NewBuffer(buf),
	}
}

// newDecoder creates a new decoder.
func newDecoder(buf []byte) *bytes.Buffer {
	return bytes.NewBuffer(buf)
}

// ---------------------------------------------------------------- from update_decoder_v2.go
// update_decoder_v2.go implements the full V2 update decoder, byte-compatible
// with Yjs v13.6.31's UpdateDecoderV2 (src/utils/UpdateDecoder.js).
//
// The constructor splits the input into the nine length-prefixed column buffers
// plus the trailing raw rest buffer, and wraps each in its column decoder. The
// Read* methods then pull from the matching column.

// dsDecoderV2 is the delta-coded delete-set decoder.
type dsDecoderV2 struct {
	rest      *bytes.Buffer
	dsCurrVal Number
}

// newDSDecoderV2 wraps a raw decoder buffer.
func newDSDecoderV2(decoder *bytes.Buffer) *dsDecoderV2 {
	return &dsDecoderV2{rest: decoder}
}

// restDecoder returns the underlying rest buffer (satisfies DSDecoder).
func (v2 *dsDecoderV2) restDecoder() *bytes.Buffer {
	return v2.rest
}

func (v2 *dsDecoderV2) resetDS() {
	v2.dsCurrVal = 0
}

// addDsCurVal advances the delta-coded delete-set accumulator by a NON-NEGATIVE
// delta, rejecting the running-sum overflow (DsCurrVal and delta are both >= 0 by
// induction, so the sum wraps iff it exceeds the bound). It is the single home for
// the accumulate-and-overflow-check shared by ReadDsClock and ReadDsLen (F#4), and
// a thin stateful wrapper over the shared addBounded predicate (F#1/G#1) so the
// V2-DS accumulator and the struct-store clock advance (addClock) reject on the
// IDENTICAL `x > math.MaxInt - y` test. The bound is math.MaxInt — not MaxInt64 —
// to match toNumber/nonNegNumber (== MaxInt64 on the 64-bit build target; on a
// 32-bit build it rejects a sum in (MaxInt32, MaxInt64] that the narrowing
// Number(int) conversion would truncate, C#1/D). addBounded returns the bare
// errNumberOverflow sentinel, so errors.Is(err, errNumberOverflow) classifies it
// alongside toNumber/nonNegNumber; the ReadDsClock/ReadDsLen callers add context.
func (v2 *dsDecoderV2) addDsCurVal(delta Number) (Number, error) {
	v, err := addBounded(v2.dsCurrVal, delta)
	if err != nil {
		return 0, err
	}
	v2.dsCurrVal = v
	return v2.dsCurrVal, nil
}

// readDSClock reads a delta-coded DS clock. The V2 delete-set encodes each clock
// as a diff added to a running accumulator (DsCurrVal). Both the per-read delta
// AND the running sum are guarded against the negative-wrap class (N1): the delta
// is clamped via toNumber (rejecting [2^63,2^64)), and the post-accumulation sum
// is checked >= 0 so a run of valid-looking deltas that sums past 2^63 (wrapping
// DsCurrVal negative) is rejected rather than yielding a negative clock that
// crashes ReadAndApplyDeleteSet on a nil struct slice (A#1).
func (v2 *dsDecoderV2) readDSClock() (Number, error) {
	n, err := binaryReadUvarint(v2.rest)
	if err != nil {
		return 0, err
	}
	delta, err := toNumber(n)
	if err != nil {
		return 0, err
	}
	// Accumulate via the shared addDsCurVal (F#4): the running-sum overflow guard
	// is identical to ReadDsLen's. A DS clock IS the accumulator, so return it.
	return v2.addDsCurVal(delta)
}

// readDSLength reads a delta-coded DS length. The encoded value is len-1, so the
// decoded diff is n+1. Guard BOTH the n+1 increment (nn==MaxInt would overflow)
// and the running-sum accumulation against the negative-wrap class (N1).
func (v2 *dsDecoderV2) readDSLength() (Number, error) {
	n, err := binaryReadUvarint(v2.rest)
	if err != nil {
		return 0, err
	}
	nn, err := toNumber(n)
	if err != nil {
		return 0, err
	}
	// diff = nn + 1; nn==MaxInt would overflow the increment to a negative diff.
	// Bound is math.MaxInt (not MaxInt64) for the same 32-bit consistency as
	// toNumber/nonNegNumber/addDsCurVal (C#1/D); toNumber has already clamped nn to
	// [0, MaxInt], so nn==MaxInt is exactly the boundary where nn+1 overflows the
	// signed Number on every build width. Sentinel wraps errNumberOverflow so
	// errors.Is classifies it.
	if nn == math.MaxInt {
		return 0, fmt.Errorf("%w (ds length %d on +1 framing)", errNumberOverflow, nn)
	}
	diff := nn + 1
	// Accumulate via the shared addDsCurVal (F#4): same running-sum overflow guard
	// as ReadDsClock. Unlike a DS clock, a DS LENGTH returns the DELTA (diff), not
	// the accumulator, so discard addDsCurVal's returned sum and return diff.
	if _, err := v2.addDsCurVal(diff); err != nil {
		return 0, err
	}
	return diff, nil
}

// updateDecoderV2 is the full V2 update decoder.
type updateDecoderV2 struct {
	dsDecoderV2

	keyClockDecoder   *intDiffOptRLEDecoder
	clientDecoder     *uintOptRLEDecoder
	leftClockDecoder  *intDiffOptRLEDecoder
	rightClockDecoder *intDiffOptRLEDecoder
	infoDecoder       *rleDecoder
	stringDecoder     *stringDecoder
	parentInfoDecoder *rleDecoder
	typeRefDecoder    *uintOptRLEDecoder
	lenDecoder        *uintOptRLEDecoder

	keys []string

	idArena    []ID
	idArenaPos int
}

// updateDecoderV2State owns the mutable buffers and column decoders referenced
// by UpdateDecoderV2. It is embedded beside the public decoder in one allocation:
// the returned pointer to allocation.decoder keeps this entire block alive, and
// no state is shared or reused between decoder instances.
type updateDecoderV2State struct {
	restBuffer       bytes.Buffer
	keyClockBuffer   bytes.Buffer
	clientBuffer     bytes.Buffer
	leftClockBuffer  bytes.Buffer
	rightClockBuffer bytes.Buffer
	infoBuffer       bytes.Buffer
	parentInfoBuffer bytes.Buffer
	typeRefBuffer    bytes.Buffer
	lenBuffer        bytes.Buffer
	stringLensBuffer bytes.Buffer

	keyClockDecoder   intDiffOptRLEDecoder
	clientDecoder     uintOptRLEDecoder
	leftClockDecoder  intDiffOptRLEDecoder
	rightClockDecoder intDiffOptRLEDecoder
	infoDecoder       rleDecoder
	parentInfoDecoder rleDecoder
	typeRefDecoder    uintOptRLEDecoder
	lenDecoder        uintOptRLEDecoder
	stringLensDecoder uintOptRLEDecoder
	stringDecoder     stringDecoder
}

type updateDecoderV2Allocation struct {
	decoder updateDecoderV2
	state   updateDecoderV2State
}

func (v2 *updateDecoderV2) enableLazyIDArena() {
	enableLazyIDBlocks(v2.idArena, &v2.idArenaPos)
}

func (v2 *updateDecoderV2) reserveIDs(structs uint64) {
	if structs <= uint64(^uint(0)>>1)/2 {
		v2.idArena = make([]ID, int(structs)*2)
		v2.idArenaPos = 0
	}
}

func (v2 *updateDecoderV2) allocID(client, clock Number) *ID {
	return allocDecodedID(&v2.idArena, &v2.idArenaPos, client, clock)
}

var _ updateDecoder = (*updateDecoderV2)(nil)

// readV2Column frames one column and returns a view of its payload.
//
// The view is sliced out of buf rather than returned from decoder.Next, even
// though Next yields the identical bytes. Next's contract is that its result "is
// only valid until the next call to a read or write method", and the constructor
// frames eight more columns from this same buffer afterwards, so every column but
// the last would be held across exactly the event that invalidates it. Nothing
// currently moves those bytes -- Buffer reads advance an offset -- but that is an
// implementation property, not a promise, and writes DO compact. Slicing buf
// removes the dependency instead of relying on it: buf is the caller's array,
// which the decoder never writes to, so these views stay valid by construction.
//
// decoder is still the cursor, and Next still advances it; only its return value
// is discarded.
func readV2Column(buf []byte, decoder *bytes.Buffer) []byte {
	size, err := binaryReadUvarint(decoder)
	if err != nil {
		return nil
	}
	if size > uint64(decoder.Len()) {
		// Match ReadVarUint8Array's malformed-column behavior: consume the
		// truncated tail, then leave the column decoder empty so its first read
		// surfaces the malformed stream rather than reading bytes from a later
		// column at the wrong offset.
		decoder.Next(decoder.Len())
		return nil
	}
	// Buffer reads only advance an offset into the array it was built from, so
	// the undecoded remainder is always a suffix of buf and the payload begins
	// exactly where that suffix begins.
	start := len(buf) - decoder.Len()
	decoder.Next(int(size))
	return buf[start : start+int(size)]
}

// newUpdateDecoderV2 splits buf into its column buffers + rest buffer and builds
// the column decoders. The numeric columns and rest buffer retain immutable views
// into buf for the decoder's lifetime, matching UpdateDecoderV1's ownership model;
// callers must not mutate buf while the decoder is in use. A malformed buffer
// yields decoders over empty columns rather than panicking; subsequent Read* calls
// then surface errors.
func newUpdateDecoderV2(buf []byte) *updateDecoderV2 {
	// Keep the public decoder and its private pointer-stable state in one heap
	// object. Splitting these fields back into constructor-created wrappers costs
	// roughly one allocation per buffer/decoder; reusing this block across readers
	// would instead corrupt their independent cursors.
	allocation := new(updateDecoderV2Allocation)
	state := &allocation.state
	state.restBuffer = *bytes.NewBuffer(buf)
	decoder := &state.restBuffer

	// feature flag (currently unused)
	_, _ = binaryReadUvarint(decoder)

	keyClock := readV2Column(buf, decoder)
	client := readV2Column(buf, decoder)
	leftClock := readV2Column(buf, decoder)
	rightClock := readV2Column(buf, decoder)
	info := readV2Column(buf, decoder)
	str := readV2Column(buf, decoder)
	parentInfo := readV2Column(buf, decoder)
	typeRef := readV2Column(buf, decoder)
	length := readV2Column(buf, decoder)
	state.keyClockBuffer = *bytes.NewBuffer(keyClock)
	state.clientBuffer = *bytes.NewBuffer(client)
	state.leftClockBuffer = *bytes.NewBuffer(leftClock)
	state.rightClockBuffer = *bytes.NewBuffer(rightClock)
	state.infoBuffer = *bytes.NewBuffer(info)
	state.parentInfoBuffer = *bytes.NewBuffer(parentInfo)
	state.typeRefBuffer = *bytes.NewBuffer(typeRef)
	state.lenBuffer = *bytes.NewBuffer(length)
	state.keyClockDecoder = intDiffOptRLEDecoder{buf: &state.keyClockBuffer}
	state.clientDecoder = uintOptRLEDecoder{buf: &state.clientBuffer}
	state.leftClockDecoder = intDiffOptRLEDecoder{buf: &state.leftClockBuffer}
	state.rightClockDecoder = intDiffOptRLEDecoder{buf: &state.rightClockBuffer}
	state.infoDecoder = rleDecoder{buf: &state.infoBuffer, r: readUint8}
	state.parentInfoDecoder = rleDecoder{buf: &state.parentInfoBuffer, r: readUint8}
	state.typeRefDecoder = uintOptRLEDecoder{buf: &state.typeRefBuffer}
	state.lenDecoder = uintOptRLEDecoder{buf: &state.lenBuffer}
	initStringDecoder(&state.stringDecoder, &state.stringLensDecoder, &state.stringLensBuffer, str)

	d := &allocation.decoder
	*d = updateDecoderV2{
		dsDecoderV2:       dsDecoderV2{rest: &state.restBuffer}, // remaining bytes = rest
		keyClockDecoder:   &state.keyClockDecoder,
		clientDecoder:     &state.clientDecoder,
		leftClockDecoder:  &state.leftClockDecoder,
		rightClockDecoder: &state.rightClockDecoder,
		infoDecoder:       &state.infoDecoder,
		stringDecoder:     &state.stringDecoder,
		parentInfoDecoder: &state.parentInfoDecoder,
		typeRefDecoder:    &state.typeRefDecoder,
		lenDecoder:        &state.lenDecoder,
	}
	return d
}

// RemainingLen reports the total number of undecoded bytes left across every
// column buffer and the rest buffer. readClientsStructRefs uses it as a
// columnar-aware progress watchdog: if a full struct read consumes zero bytes,
// the input is exhausted/corrupt (e.g. an RLE infinite-run sentinel) and the
// decode loop must stop instead of spinning on a bogus struct count.
func (v2 *updateDecoderV2) RemainingLen() int {
	return v2.keyClockDecoder.remaining() +
		v2.clientDecoder.remaining() +
		v2.leftClockDecoder.remaining() +
		v2.rightClockDecoder.remaining() +
		v2.infoDecoder.remaining() +
		v2.stringDecoder.remaining() +
		v2.parentInfoDecoder.remaining() +
		v2.typeRefDecoder.remaining() +
		v2.lenDecoder.remaining() +
		v2.rest.Len()
}

// readID reads a full ID from the client + leftClock columns. The client
// (UintOptRle, uint64) is clamped via toNumber and the clock (IntDiffOptRle,
// int64 accumulator) via nonNegNumber so neither a [2^63,2^64) client nor a
// negative running clock reaches the document logic (the negative-wrap class, N1).
func (v2 *updateDecoderV2) readID() (*ID, error) {
	rawClient, err := v2.clientDecoder.readValue()
	if err != nil {
		return nil, err
	}
	client, err := toNumber(rawClient)
	if err != nil {
		return nil, err
	}
	rawClock, err := v2.leftClockDecoder.readValue()
	if err != nil {
		return nil, err
	}
	clock, err := nonNegNumber(rawClock)
	if err != nil {
		return nil, err
	}
	return v2.allocID(client, clock), nil
}

// readLeftID reads client from the client column and clock from leftClock.
func (v2 *updateDecoderV2) readLeftID() (*ID, error) {
	// In V2 a left ID reads the same two columns as an ID (client + leftClock),
	// so delegate to ReadID rather than duplicate the reads (mirrors UpdateDecoderV1).
	return v2.readID()
}

// readRightID reads client from the client column and clock from rightClock.
// Both fields are clamped (toNumber / nonNegNumber) — see ReadID (N1).
func (v2 *updateDecoderV2) readRightID() (*ID, error) {
	rawClient, err := v2.clientDecoder.readValue()
	if err != nil {
		return nil, err
	}
	client, err := toNumber(rawClient)
	if err != nil {
		return nil, err
	}
	rawClock, err := v2.rightClockDecoder.readValue()
	if err != nil {
		return nil, err
	}
	clock, err := nonNegNumber(rawClock)
	if err != nil {
		return nil, err
	}
	return v2.allocID(client, clock), nil
}

// readClient reads the next client id from the client column. Clamped via
// toNumber so a [2^63,2^64) client id is rejected, not wrapped negative (N1).
func (v2 *updateDecoderV2) readClient() (Number, error) {
	client, err := v2.clientDecoder.readValue()
	if err != nil {
		return 0, err
	}
	return toNumber(client)
}

// readInfo reads the next info byte from the info column.
func (v2 *updateDecoderV2) readInfo() (uint8, error) {
	return v2.infoDecoder.readValue()
}

// readStringValue reads the next string from the string column.
func (v2 *updateDecoderV2) readStringValue() (string, error) {
	return v2.stringDecoder.readValue()
}

// readParentInfo reads the next parent-is-y-key flag from the parentInfo column.
func (v2 *updateDecoderV2) readParentInfo() (bool, error) {
	b, err := v2.parentInfoDecoder.readValue()
	if err != nil {
		return false, err
	}
	return b == 1, nil
}

// readTypeRef reads the next type reference from the typeRef column.
func (v2 *updateDecoderV2) readTypeRef() (uint8, error) {
	ref, err := v2.typeRefDecoder.readValue()
	if err != nil {
		return 0, err
	}
	return uint8(ref), nil
}

// readLength reads the next struct length from the len column. Clamped via toNumber
// so a [2^63,2^64) length is rejected, not wrapped negative (N1).
func (v2 *updateDecoderV2) readLength() (Number, error) {
	length, err := v2.lenDecoder.readValue()
	if err != nil {
		return 0, err
	}
	return toNumber(length)
}

// readAnyValue reads an arbitrary value from the rest buffer (lib0 any-decoding).
func (v2 *updateDecoderV2) readAnyValue() (any, error) {
	return readAny(v2.rest)
}

// readBuffer reads a length-prefixed byte buffer from the rest buffer.
func (v2 *updateDecoderV2) readBuffer() ([]uint8, error) {
	data, err := readVarUint8Array(v2.rest)
	if err != nil {
		return nil, err
	}
	return data.([]uint8), nil
}

// readJSONValue decodes an embed via lib0 any-decoding (V2 difference from V1, which
// used JSON.parse).
func (v2 *updateDecoderV2) readJSONValue() (interface{}, error) {
	return readAny(v2.rest)
}

// readKey reads a property key, using the key cache when the keyClock index
// refers to an already-seen key (matching Yjs UpdateDecoderV2.readKey).
func (v2 *updateDecoderV2) readKey() (string, error) {
	keyClock, err := v2.keyClockDecoder.readValue()
	if err != nil {
		return "", err
	}
	// keyClock indexes the running key cache. A signed IntDiffOptRle column can
	// yield a negative running value on hostile input; reject it instead of
	// indexing v2.keys with a negative subscript (panic). Likewise reject values
	// past the next slot, which would otherwise silently mis-pair keys.
	if keyClock < 0 || keyClock > int64(len(v2.keys)) {
		return "", fmt.Errorf("invalid key clock %d (cache size %d)", keyClock, len(v2.keys))
	}
	if keyClock < int64(len(v2.keys)) {
		return v2.keys[keyClock], nil
	}
	// New key: keyClock == len(v2.keys) (the next index). Read the string and
	// append it, keeping cache index == write order.
	key, err := v2.stringDecoder.readValue()
	if err != nil {
		return "", err
	}
	v2.keys = append(v2.keys, key)
	return key, nil
}
