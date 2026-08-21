package crdt

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
)

// ---------------------------------------------------------------- from update_encoder_decoder_v2.go
// dsEncoderV2 is the delta-coded delete-set encoder shared by UpdateEncoderV2.
// Clocks are written as deltas from a running value and lengths as len-1, so a
// client's sorted delete ranges compress well. (UpdateEncoderV2 itself lives in
// update_encoder_v2.go; the matching decoders live in update_decoder_v2.go.)
type dsEncoderV2 struct {
	rest      *bytes.Buffer
	dsCurrVal Number
}

func (v2 *dsEncoderV2) toBytes() []uint8 {
	return v2.rest.Bytes()
}

// encodeError satisfies DSEncoder. A bare DSEncoderV2 writes the delete set directly to
// the rest buffer (WriteDsLen returns its error inline; there are no deferred
// struct-column sub-encoders), so it has no deferred error and returns nil.
// UpdateEncoderV2 overrides this to surface its column-encoder errors.
func (v2 *dsEncoderV2) encodeError() error {
	return nil
}

// restEncoder returns the underlying rest buffer (satisfies DSEncoder).
func (v2 *dsEncoderV2) restEncoder() *bytes.Buffer {
	return v2.rest
}

// restartRestEncoder returns the current rest bytes and starts a fresh buffer,
// leaving the column sub-encoders (if any) untouched.
func (v2 *dsEncoderV2) restartRestEncoder() []uint8 {
	out := v2.rest.Bytes()
	v2.rest = new(bytes.Buffer)
	return out
}

func (v2 *dsEncoderV2) resetDS() {
	v2.dsCurrVal = 0
}

func (v2 *dsEncoderV2) writeDSClock(clock Number) {
	diff := clock - v2.dsCurrVal
	v2.dsCurrVal = clock
	writeVarUint(v2.rest, uint64(diff))
}

func (v2 *dsEncoderV2) writeDSLength(length Number) error {
	// Delete lengths are always >= 1; Yjs treats len == 0 as an unexpected case.
	// The V2 wire format stores (length-1) as a VarUint, so a length of 0 (or
	// negative) would underflow to a huge value and emit a corrupt delete set
	// silently. Return an error instead of panicking: a V1 update can legally
	// carry a 0-length delete range, and ConvertUpdateFormatV1ToV2 must fail that
	// re-encode gracefully rather than crash the process. (Unreachable for real
	// delete sets, which are built from live document state with length >= 1.)
	if length <= 0 {
		return fmt.Errorf("DSEncoderV2.WriteDsLen: invalid delete length %d (must be >= 1)", length)
	}
	writeVarUint(v2.rest, uint64(length-1))
	v2.dsCurrVal += length
	return nil
}

// ---------------------------------------------------------------- from update_encoder_v1.go
const maxV1EncodePreallocation = 64 << 20

// estimateFullUpdateV1Capacity cheaply sizes the common full-document encoder from the number of
// stored structs. Fragmented text averages just under nine encoded bytes per struct; twelve leaves
// room for framing and wider clocks without walking every item a second time. Content-heavy items
// can still grow the buffer normally. Cap the hint so an already-large live document cannot turn a
// heuristic into an unexpectedly large speculative allocation.
func estimateFullUpdateV1Capacity(store *structStore) int {
	const bytesPerStruct = 9
	structCount := 0
	store.forEachClient(func(_ Number, structs *clientStructList) bool {
		if structs.Len() > (maxV1EncodePreallocation/bytesPerStruct)-structCount {
			structCount = maxV1EncodePreallocation / bytesPerStruct
			return false
		}
		structCount += structs.Len()
		return true
	})
	if structCount >= maxV1EncodePreallocation/bytesPerStruct {
		return maxV1EncodePreallocation
	}
	estimate := structCount * bytesPerStruct
	if estimate > maxV1EncodePreallocation {
		return maxV1EncodePreallocation
	}
	return estimate
}

type dsEncoderV1 struct {
	rest *bytes.Buffer
}

type updateEncoderV1 struct {
	dsEncoderV1
}

// toBytes returns the encoded bytes.
func (v1 *dsEncoderV1) toBytes() []uint8 {
	return v1.rest.Bytes()
}

// encodeError satisfies DSEncoder. The V1 encoder writes clocks/lengths as plain
// VarUints with no column codecs, so it has no deferred encode error: always nil.
func (v1 *dsEncoderV1) encodeError() error {
	return nil
}

// restEncoder returns the underlying rest buffer (satisfies DSEncoder).
func (v1 *dsEncoderV1) restEncoder() *bytes.Buffer {
	return v1.rest
}

// restartRestEncoder returns the current rest bytes and starts a fresh buffer.
func (v1 *dsEncoderV1) restartRestEncoder() []uint8 {
	out := v1.rest.Bytes()
	v1.rest = new(bytes.Buffer)
	return out
}

// resetDS resets the current value of DeleteSet.
func (v1 *dsEncoderV1) resetDS() {
	// nop
}

// writeDSClock writes the clock value of DeleteSet.
func (v1 *dsEncoderV1) writeDSClock(clock Number) {
	writeVarUint(v1.rest, uint64(clock))
}

// writeDSLength writes the length of DeleteSet. V1 stores the raw length as a
// VarUint (no len-1 bias), so any non-negative value is representable; it never
// errors. The error return matches the DSEncoder interface (V2 can reject a
// 0-length range).
func (v1 *dsEncoderV1) writeDSLength(length Number) error {
	writeVarUint(v1.rest, uint64(length))
	return nil
}

// writeID writes the ID of Item.
func (v1 *updateEncoderV1) writeID(id *ID) {
	writeVarUint(v1.rest, uint64(id.Client))
	writeVarUint(v1.rest, uint64(id.Clock))
}

// writeLeftID writes the left ID of Item.
func (v1 *updateEncoderV1) writeLeftID(id *ID) {
	v1.writeID(id)
}

// writeRightID writes the right ID of Item.
func (v1 *updateEncoderV1) writeRightID(id *ID) {
	v1.writeID(id)
}

// writeClient writes the client of Item.
func (v1 *updateEncoderV1) writeClient(client Number) {
	writeVarUint(v1.rest, uint64(client))
}

// writeInfo writes the info of Item.
func (v1 *updateEncoderV1) writeInfo(info uint8) {
	writeByte(v1.rest, info)
}

// writeStringValue writes the string of Item.
func (v1 *updateEncoderV1) writeStringValue(str string) error {
	return writeString(v1.rest, str)
}

// writeParentInfo writes the parent info of Item.
func (v1 *updateEncoderV1) writeParentInfo(isYKey bool) {
	code := uint64(0)
	if isYKey {
		code = 1
	}

	writeVarUint(v1.rest, code)
}

// writeTypeRef writes the type ref of Item.
func (v1 *updateEncoderV1) writeTypeRef(info uint8) {
	writeVarUint(v1.rest, uint64(info))
}

// writeLength write len of a struct - well suited for Opt RLE encoder.
func (v1 *updateEncoderV1) writeLength(length Number) {
	writeVarUint(v1.rest, uint64(length))
}

// writeAnyValue writes the any of Item.
func (v1 *updateEncoderV1) writeAnyValue(value any) error {
	return writeAny(v1.rest, value)
}

// writeBuffer writes the buf of Item.
func (v1 *updateEncoderV1) writeBuffer(buf []uint8) error {
	writeVarUint8Array(v1.rest, buf)
	return nil
}

// writeJSONValue writes the json of Item. Yjs encodes V1 embeds/format values with
// JSON.stringify (insertion-ordered object keys); marshalJSONOrdered preserves
// that order for the ordered Object type so multi-key values are byte-identical
// to the JS stream (plain json.Marshal would sort the keys).
func (v1 *updateEncoderV1) writeJSONValue(embed interface{}) error {
	switch value := embed.(type) {
	case nil, NullType, UndefinedType:
		return writeString(v1.rest, "null")
	case bool:
		if value {
			return writeString(v1.rest, "true")
		}
		return writeString(v1.rest, "false")
	case string:
		writeVarUint(v1.rest, jsonStringJSEncodedLen(value))
		writeJSONStringJS(v1.rest, value)
		return nil
	}
	data, err := marshalJSONOrdered(embed)
	if err != nil {
		return err
	}

	return writeString(v1.rest, string(data))
}

// writeKey writes the key of Item.
func (v1 *updateEncoderV1) writeKey(key string) error {
	return writeString(v1.rest, key)
}

// newUpdateEncoderV1 creates a new UpdateEncoderV1 instance.
func newUpdateEncoderV1() *updateEncoderV1 {
	return &updateEncoderV1{
		dsEncoderV1{
			rest: new(bytes.Buffer),
		},
	}
}

// newEncoder creates a new UpdateEncoderV1 instance.
func newEncoder() *bytes.Buffer {
	return new(bytes.Buffer)
}

// ---------------------------------------------------------------- from update_encoder_v1_fast.go
// fastUpdateEncoderV1 is the internal full-update encoder. Appending directly to a byte slice
// avoids the method and memmove cost bytes.Buffer pays for the several one- and two-byte fields in
// every struct. The exported UpdateEncoderV1 keeps its established bytes.Buffer surface.
//
// GetRestEncoder is a compatibility escape hatch for uncommon writers that need direct buffer
// access. It switches the encoder permanently to bytes.Buffer while preserving everything already
// appended; the ordinary full-document path never takes it.
type fastUpdateEncoderV1 struct {
	buf         []byte
	rest        *bytes.Buffer
	hasDeleted  bool
	scanDeletes bool
}

func newFastUpdateEncoderV1(store *structStore) fastUpdateEncoderV1 {
	return fastUpdateEncoderV1{buf: make([]byte, 0, estimateFullUpdateV1Capacity(store))}
}

func appendVarUint(dst []byte, number uint64) []byte {
	if number < bit8 {
		return append(dst, byte(number))
	}
	if number < 1<<14 {
		return append(dst, byte(number)|bit8, byte(number>>7))
	}
	for number >= bit8 {
		dst = append(dst, byte(number)|bit8)
		number >>= 7
	}
	return append(dst, byte(number))
}

func appendString(dst []byte, value string) []byte {
	dst = appendVarUint(dst, uint64(len(value)))
	return append(dst, value...)
}

func (e *fastUpdateEncoderV1) writeByte(value byte) {
	if e.rest != nil {
		_ = e.rest.WriteByte(value)
		return
	}
	e.buf = append(e.buf, value)
}

func (e *fastUpdateEncoderV1) writeRestVarUint(value uint64) {
	if e.rest != nil {
		writeVarUint(e.rest, value)
		return
	}
	e.buf = appendVarUint(e.buf, value)
}

func (e *fastUpdateEncoderV1) writeString(value string) {
	if e.rest != nil {
		_ = writeString(e.rest, value)
		return
	}
	e.buf = appendVarUint(e.buf, uint64(len(value)))
	e.buf = append(e.buf, value...)
}

func (e *fastUpdateEncoderV1) toBytes() []uint8 {
	if e.rest != nil {
		return e.rest.Bytes()
	}
	return e.buf
}

func (e *fastUpdateEncoderV1) encodeError() error { return nil }

func (e *fastUpdateEncoderV1) resetDS() {}

func (e *fastUpdateEncoderV1) writeDSClock(clock Number) {
	e.writeRestVarUint(uint64(clock))
}

func (e *fastUpdateEncoderV1) writeDSLength(length Number) error {
	e.writeRestVarUint(uint64(length))
	return nil
}

func (e *fastUpdateEncoderV1) restEncoder() *bytes.Buffer {
	if e.rest == nil {
		e.rest = bytes.NewBuffer(e.buf)
		e.buf = nil
	}
	return e.rest
}

func (e *fastUpdateEncoderV1) restartRestEncoder() []uint8 {
	out := e.toBytes()
	e.buf = make([]byte, 0, cap(out))
	e.rest = nil
	return out
}

func (e *fastUpdateEncoderV1) writeID(id *ID) {
	e.writeRestVarUint(uint64(id.Client))
	e.writeRestVarUint(uint64(id.Clock))
}

func (e *fastUpdateEncoderV1) writeLeftID(id *ID)  { e.writeID(id) }
func (e *fastUpdateEncoderV1) writeRightID(id *ID) { e.writeID(id) }

func (e *fastUpdateEncoderV1) writeClient(client Number) {
	e.writeRestVarUint(uint64(client))
}

func (e *fastUpdateEncoderV1) writeInfo(info uint8) { e.writeByte(info) }

func (e *fastUpdateEncoderV1) writeStringValue(str string) error {
	e.writeString(str)
	return nil
}

func (e *fastUpdateEncoderV1) writeParentInfo(isYKey bool) {
	if isYKey {
		e.writeByte(1)
	} else {
		e.writeByte(0)
	}
}

func (e *fastUpdateEncoderV1) writeTypeRef(info uint8) {
	e.writeRestVarUint(uint64(info))
}

func (e *fastUpdateEncoderV1) writeLength(length Number) {
	e.writeRestVarUint(uint64(length))
}

func (e *fastUpdateEncoderV1) writeAnyValue(value any) error {
	return writeAny(e.restEncoder(), value)
}

func (e *fastUpdateEncoderV1) writeBuffer(buf []uint8) error {
	e.writeRestVarUint(uint64(len(buf)))
	if e.rest != nil {
		_, _ = e.rest.Write(buf)
	} else {
		e.buf = append(e.buf, buf...)
	}
	return nil
}

func (e *fastUpdateEncoderV1) writeJSONValue(embed interface{}) error {
	switch value := embed.(type) {
	case nil, NullType, UndefinedType:
		e.writeString("null")
		return nil
	case bool:
		if value {
			e.writeString("true")
		} else {
			e.writeString("false")
		}
		return nil
	case string:
		e.writeRestVarUint(jsonStringJSEncodedLen(value))
		if e.rest != nil {
			writeJSONStringJS(e.rest, value)
		} else {
			e.buf = appendJSONStringJS(e.buf, value)
		}
		return nil
	}
	data, err := marshalJSONOrdered(embed)
	if err != nil {
		return err
	}
	if e.rest != nil {
		writeVarUint(e.rest, uint64(len(data)))
		_, _ = e.rest.Write(data)
	} else {
		e.buf = appendVarUint(e.buf, uint64(len(data)))
		e.buf = append(e.buf, data...)
	}
	return nil
}

func (e *fastUpdateEncoderV1) writeKey(key string) error {
	e.writeString(key)
	return nil
}

func (e *fastUpdateEncoderV1) writeStructs(structs []abstractStruct, start int, firstOffset Number) error {
	if start >= len(structs) {
		return nil
	}
	if e.rest != nil {
		e.scanDeletes = true
		if err := structs[start].writeStruct(e, firstOffset); err != nil {
			return err
		}
		for i := start + 1; i < len(structs); i++ {
			if err := structs[i].writeStruct(e, 0); err != nil {
				return err
			}
		}
		return nil
	}
	if firstOffset != 0 {
		e.scanDeletes = true
		if err := structs[start].writeStruct(e, firstOffset); err != nil {
			return err
		}
		return e.writeStructs(structs, start+1, 0)
	}

	buf := e.buf
	for i := start; i < len(structs); i++ {
		item, ok := structs[i].(*itemStruct)
		if !ok {
			e.scanDeletes = true
			if structs[i].isDeleted() {
				e.hasDeleted = true
			}
			e.buf = buf
			if err := structs[i].writeStruct(e, 0); err != nil {
				return err
			}
			if e.rest != nil {
				return e.writeStructs(structs, i+1, 0)
			}
			buf = e.buf
			continue
		}
		if item.info&bit3 != 0 {
			e.hasDeleted = true
		}
		contentString, isString := item.content.(*contentString)
		if !isString {
			e.scanDeletes = true
			e.buf = buf
			if err := item.writeStruct(e, 0); err != nil {
				return err
			}
			if e.rest != nil {
				return e.writeStructs(structs, i+1, 0)
			}
			buf = e.buf
			continue
		}

		origin := item.origin
		rightOrigin := item.rightOrigin
		parentSub := item.parentSub
		info := uint8(refContentString)
		if origin != nil {
			info |= bit8
		}
		if rightOrigin != nil {
			info |= bit7
		}
		if parentSub != "" {
			info |= bit6
		}
		buf = append(buf, info)
		if origin != nil {
			buf = appendVarUint(buf, uint64(origin.Client))
			buf = appendVarUint(buf, uint64(origin.Clock))
		}
		if rightOrigin != nil {
			buf = appendVarUint(buf, uint64(rightOrigin.Client))
			buf = appendVarUint(buf, uint64(rightOrigin.Clock))
		}

		if origin == nil && rightOrigin == nil {
			parent := item.parent
			switch {
			case isAbstractType(parent) && !isYString(parent) && !isIDPtr(parent):
				parentType := parent.(abstractType)
				parentItem := parentType.getItem()
				if parentItem == nil {
					buf = append(buf, 1)
					buf = appendString(buf, findRootTypeKey(parentType))
				} else {
					buf = append(buf, 0)
					buf = appendVarUint(buf, uint64(parentItem.id.Client))
					buf = appendVarUint(buf, uint64(parentItem.id.Clock))
				}
			case isYString(parent):
				buf = append(buf, 1)
				buf = appendString(buf, parent.(*yString).str)
			case isIDPtr(parent) && parent.(*ID) != nil:
				buf = append(buf, 0)
				parentID := parent.(*ID)
				buf = appendVarUint(buf, uint64(parentID.Client))
				buf = appendVarUint(buf, uint64(parentID.Clock))
			default:
				return fmt.Errorf("write struct: item %v has nil origin/rightOrigin and invalid parent %T", item.id, parent)
			}
			if parentSub != "" {
				buf = appendString(buf, parentSub)
			}
		}

		buf = appendString(buf, contentString.value)
	}
	e.buf = buf
	return nil
}

var _ updateEncoder = (*fastUpdateEncoderV1)(nil)

func encodeFullStateAsUpdateV1(store *structStore) ([]byte, error) {
	encoder := newFastUpdateEncoderV1(store)
	clientCount := store.clientCountValue()
	var clients []Number
	encoder.writeRestVarUint(uint64(clientCount))

	if clientCount == 1 {
		var writeErr error
		store.forEachClient(func(client Number, structs *clientStructList) bool {
			writeErr = structs.writeFullStateV1(&encoder, client)
			return false
		})
		if writeErr != nil {
			return nil, fmt.Errorf("write state as update: %w", writeErr)
		}
	} else if clientCount > 1 {
		clients = store.appendClientIDs(make([]Number, 0, clientCount))
		sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
		for _, client := range clients {
			structs, _ := store.clientStructs(client)
			if err := structs.writeFullStateV1(&encoder, client); err != nil {
				return nil, fmt.Errorf("write state as update: %w", err)
			}
		}
	}

	if !encoder.hasDeleted && !encoder.scanDeletes {
		encoder.writeRestVarUint(0)
	} else if err := writeDeleteSetFromStructStore(&encoder, store, clients); err != nil {
		return nil, fmt.Errorf("write state as update: delete set encode failed: %w", err)
	}
	return encoder.toBytes(), nil
}

// ---------------------------------------------------------------- from update_encoder_v2.go
// update_encoder_v2.go implements the full V2 update encoder, byte-identical to
// Yjs v13.6.31's UpdateEncoderV2 (src/utils/UpdateEncoder.js).
//
// Each struct field category is written into its own column sub-encoder
// optimized for that field's value distribution; ToUint8Array concatenates the
// columns (length-prefixed) followed by the raw rest buffer. See research.md for
// the field -> sub-encoder mapping.

// updateEncoderV2 is the V2 update encoder.
type updateEncoderV2 struct {
	dsEncoderV2

	keyClockEncoder   *intDiffOptRLEEncoder
	clientEncoder     *uintOptRLEEncoder
	leftClockEncoder  *intDiffOptRLEEncoder
	rightClockEncoder *intDiffOptRLEEncoder
	infoEncoder       *rleEncoder
	stringEncoder     *stringEncoder
	parentInfoEncoder *rleEncoder
	typeRefEncoder    *uintOptRLEEncoder
	lenEncoder        *uintOptRLEEncoder

	keyMap     map[string]int
	keyClock   int
	hasDeleted bool
	// trustedFullState is set only for a full encode of the live StructStore.
	// Those Number-sized clocks cannot contain the oversized hostile deltas that
	// conversion/lazy-update paths must reject.
	trustedFullState bool
	// trustedClocksBounded is established by inspecting every client's final
	// state clock before a full encode. When true, all referenced clocks and
	// their deltas fit the V2 signed framing without a per-field bounds check.
	trustedClocksBounded bool

	// err is a sticky, deferred encode error captured from the column
	// sub-encoders. The struct/content write methods on the UpdateEncoder
	// interface (WriteLeftID/WriteRightID/WriteID/WriteClient/WriteKey) cannot
	// return an error, so a clock-column overflow detected at write time is
	// stashed here and surfaced via Error() after ToUint8Array. A hostile clock
	// delta (>2^61, reachable through ConvertUpdateFormatV1ToV2) thus fails the
	// encode gracefully instead of panicking. nil on every valid document.
	err error
}

var _ updateEncoder = (*updateEncoderV2)(nil)

const maxV2ColumnPreallocation = 16 << 20

// newDefaultUpdateEncoderV2 creates a fully-initialized V2 update encoder.
func newDefaultUpdateEncoderV2() *updateEncoderV2 {
	return newUpdateEncoderV2(0)
}

func v2ColumnCapacity(structCount, numerator, denominator int) int {
	if structCount <= 0 {
		return 0
	}
	if structCount > (maxV2ColumnPreallocation/numerator)*denominator {
		return maxV2ColumnPreallocation
	}
	capacity := structCount * numerator / denominator
	if capacity > maxV2ColumnPreallocation {
		return maxV2ColumnPreallocation
	}
	return capacity
}

func newUpdateEncoderV2(structCount int) *updateEncoderV2 {
	clockCapacity := v2ColumnCapacity(structCount, 9, 4)
	textCapacity := v2ColumnCapacity(structCount, 1, 1)
	smallCapacity := 0
	if structCount > 0 {
		smallCapacity = 64
		if textCapacity <= maxV2ColumnPreallocation-smallCapacity {
			textCapacity += smallCapacity
		}
	}
	return &updateEncoderV2{
		dsEncoderV2: dsEncoderV2{
			rest: bytes.NewBuffer(make([]byte, 0, smallCapacity)),
		},
		keyClockEncoder:   newIntDiffOptRleEncoder(smallCapacity),
		clientEncoder:     newUintOptRleEncoder(smallCapacity),
		leftClockEncoder:  newIntDiffOptRleEncoder(clockCapacity),
		rightClockEncoder: newIntDiffOptRleEncoder(clockCapacity),
		infoEncoder:       newRleEncoderWithCapacity(writeByte, smallCapacity),
		stringEncoder:     newStringEncoder(textCapacity, smallCapacity),
		parentInfoEncoder: newRleEncoderWithCapacity(writeByte, smallCapacity),
		typeRefEncoder:    newUintOptRleEncoder(smallCapacity),
		lenEncoder:        newUintOptRleEncoder(smallCapacity),
		keyMap:            make(map[string]int),
	}
}

// encodeError returns the sticky encode error captured from the column sub-encoders
// (e.g. a clock-column diff overflow), or nil. The encode entry points check it
// after ToUint8Array, because the UpdateEncoder write methods that feed the
// columns cannot themselves return an error. Always nil for a valid document.
func (v2 *updateEncoderV2) encodeError() error {
	return v2.err
}

// recordErr stores the first non-nil column error so it is not masked by a later
// success; surfaced via Error().
func (v2 *updateEncoderV2) recordErr(err error) {
	if err != nil && v2.err == nil {
		v2.err = err
	}
}

// flushColumn flushes one column codec, recording any encode error and returning
// the (possibly empty) bytes so ToUint8Array can keep its byte layout intact.
func (v2 *updateEncoderV2) flushColumn(b []uint8, err error) []uint8 {
	v2.recordErr(err)
	return b
}

func varUintEncodedLen(value uint64) int {
	length := 1
	for value >= bit8 {
		length++
		value >>= 7
	}
	return length
}

// toBytes flushes every column and concatenates them in the V2 layout:
// feature flag, nine length-prefixed columns, then the raw rest buffer. A
// column-flush error (clock diff out of range) is recorded on the encoder and
// surfaced via Error(); the entry points fail the encode on it rather than
// emitting a truncated update. On the success path the bytes are unchanged.
func (v2 *updateEncoderV2) toBytes() []uint8 {
	columns := [9][]byte{
		v2.flushColumn(v2.keyClockEncoder.bytes()),
		v2.clientEncoder.bytes(),
		v2.flushColumn(v2.leftClockEncoder.bytes()),
		v2.flushColumn(v2.rightClockEncoder.bytes()),
		v2.infoEncoder.bytes(),
		v2.stringEncoder.bytes(),
		v2.parentInfoEncoder.bytes(),
		v2.typeRefEncoder.bytes(),
		v2.lenEncoder.bytes(),
	}
	total := 1 + len(v2.rest.Bytes())
	for _, column := range columns {
		total += len(column) + varUintEncodedLen(uint64(len(column)))
	}
	encoder := make([]byte, 0, total)
	encoder = append(encoder, 0) // feature flag, reserved for future use
	for _, column := range columns {
		encoder = appendVarUint(encoder, uint64(len(column)))
		encoder = append(encoder, column...)
	}
	// The rest encoder is appended raw (note: no length prefix).
	encoder = append(encoder, v2.rest.Bytes()...)
	return encoder
}

// writeID writes a full ID into the client + leftClock columns. (Yjs has no
// dedicated writeID on the V2 encoder; callers use WriteLeftID/WriteRightID, but
// the interface requires it, so we mirror the V1 client+clock framing here.)
func (v2 *updateEncoderV2) writeID(id *ID) {
	v2.clientEncoder.writeValue(uint64(id.Client))
	v2.recordErr(v2.leftClockEncoder.writeValue(int64(id.Clock)))
}

// writeLeftID writes id.client to the client column and id.clock to leftClock —
// the same columns as WriteID, so delegate (mirrors UpdateEncoderV1).
func (v2 *updateEncoderV2) writeLeftID(id *ID) {
	v2.writeID(id)
}

// writeRightID writes id.client to the client column and id.clock to rightClock.
func (v2 *updateEncoderV2) writeRightID(id *ID) {
	v2.clientEncoder.writeValue(uint64(id.Client))
	v2.recordErr(v2.rightClockEncoder.writeValue(int64(id.Clock)))
}

// writeClient writes a client id to the client column.
func (v2 *updateEncoderV2) writeClient(client Number) {
	v2.clientEncoder.writeValue(uint64(client))
}

// writeInfo writes an info byte to the info column.
func (v2 *updateEncoderV2) writeInfo(info uint8) {
	v2.infoEncoder.writeValue(info)
}

// writeStringValue writes a string to the string column.
func (v2 *updateEncoderV2) writeStringValue(str string) error {
	v2.stringEncoder.writeValue(str)
	return nil
}

// writeParentInfo writes the parent-is-y-key flag to the parentInfo column.
func (v2 *updateEncoderV2) writeParentInfo(isYKey bool) {
	var b uint8
	if isYKey {
		b = 1
	}
	v2.parentInfoEncoder.writeValue(b)
}

// writeTypeRef writes a type reference to the typeRef column.
func (v2 *updateEncoderV2) writeTypeRef(info uint8) {
	v2.typeRefEncoder.writeValue(uint64(info))
}

// writeLength writes a struct length to the len column.
func (v2 *updateEncoderV2) writeLength(length Number) {
	v2.lenEncoder.writeValue(uint64(length))
}

// writeAnyValue writes an arbitrary value into the rest buffer (lib0 any-encoding).
func (v2 *updateEncoderV2) writeAnyValue(value any) error {
	return writeAny(v2.rest, value)
}

// writeBuffer writes a length-prefixed byte buffer into the rest buffer.
func (v2 *updateEncoderV2) writeBuffer(buf []uint8) error {
	writeVarUint8Array(v2.rest, buf)
	return nil
}

// writeJSONValue encodes embed via lib0 any-encoding (the V2-specific difference from
// V1, which used JSON.stringify). It cannot fail, but returns error to satisfy
// the interface.
func (v2 *updateEncoderV2) writeJSONValue(embed interface{}) error {
	return writeAny(v2.rest, embed)
}

// writeKey writes a property key. Per Yjs v13.6.31 the keyMap cache is disabled
// (keyMap.set is commented out), so every key increments keyClock and writes the
// string. We mirror that exactly to stay byte-identical; the cache lookup branch
// is kept for completeness but never hit because keyMap is never populated.
func (v2 *updateEncoderV2) writeKey(key string) error {
	if clock, ok := v2.keyMap[key]; ok {
		return v2.keyClockEncoder.writeValue(int64(clock))
	}
	// NOTE: caching intentionally disabled to match Yjs (keyMap.set commented out).
	if err := v2.keyClockEncoder.writeValue(int64(v2.keyClock)); err != nil {
		return err
	}
	v2.keyClock++
	v2.stringEncoder.writeValue(key)
	return nil
}

func (v2 *updateEncoderV2) writeTrustedClient(client Number) {
	e := v2.clientEncoder
	value := uint64(client)
	if e.count > 0 && e.s == value {
		e.count++
		return
	}
	e.writeValue(value)
}

func (v2 *updateEncoderV2) writeTrustedInfo(info uint8) {
	e := v2.infoEncoder
	if e.started && e.state == info {
		e.count++
		return
	}
	e.writeValue(info)
}

// writeStructs specializes the overwhelmingly common full-state string item at
// offset zero. It writes directly into the V2 columns, avoiding the generic
// UpdateEncoder interface round-trip for every field while retaining Item.Write
// unchanged for partial structs and every other content kind.
func (v2 *updateEncoderV2) writeStructs(structs []abstractStruct, start int, firstOffset Number) error {
	if start >= len(structs) {
		return nil
	}
	if firstOffset != 0 {
		if err := structs[start].writeStruct(v2, firstOffset); err != nil {
			return err
		}
		start++
	}
	for i := start; i < len(structs); i++ {
		item, ok := structs[i].(*itemStruct)
		if !ok {
			if structs[i].isDeleted() {
				v2.hasDeleted = true
			}
			if err := structs[i].writeStruct(v2, 0); err != nil {
				return err
			}
			continue
		}
		if item.info&bit3 != 0 {
			v2.hasDeleted = true
		}
		content, ok := item.content.(*contentString)
		if !ok {
			if err := item.writeStruct(v2, 0); err != nil {
				return err
			}
			continue
		}

		origin := item.origin
		rightOrigin := item.rightOrigin
		parentSub := item.parentSub
		info := uint8(refContentString)
		if origin != nil {
			info |= bit8
		}
		if rightOrigin != nil {
			info |= bit7
		}
		if parentSub != "" {
			info |= bit6
		}
		if v2.trustedFullState {
			v2.writeTrustedInfo(info)
		} else {
			v2.infoEncoder.writeValue(info)
		}
		if origin != nil {
			if v2.trustedFullState {
				v2.writeTrustedClient(origin.Client)
			} else {
				v2.clientEncoder.writeValue(uint64(origin.Client))
			}
			if v2.trustedFullState {
				v2.leftClockEncoder.writeTrusted(int64(origin.Clock))
			} else {
				v2.recordErr(v2.leftClockEncoder.writeValue(int64(origin.Clock)))
			}
		}
		if rightOrigin != nil {
			if v2.trustedFullState {
				v2.writeTrustedClient(rightOrigin.Client)
			} else {
				v2.clientEncoder.writeValue(uint64(rightOrigin.Client))
			}
			if v2.trustedFullState {
				v2.rightClockEncoder.writeTrusted(int64(rightOrigin.Clock))
			} else {
				v2.recordErr(v2.rightClockEncoder.writeValue(int64(rightOrigin.Clock)))
			}
		}

		if origin == nil && rightOrigin == nil {
			parent := item.parent
			switch {
			case isAbstractType(parent) && !isYString(parent) && !isIDPtr(parent):
				parentType := parent.(abstractType)
				parentItem := parentType.getItem()
				if parentItem == nil {
					v2.parentInfoEncoder.writeValue(1)
					v2.stringEncoder.writeValue(findRootTypeKey(parentType))
				} else {
					v2.parentInfoEncoder.writeValue(0)
					v2.clientEncoder.writeValue(uint64(parentItem.id.Client))
					if v2.trustedFullState {
						v2.leftClockEncoder.writeTrusted(int64(parentItem.id.Clock))
					} else {
						v2.recordErr(v2.leftClockEncoder.writeValue(int64(parentItem.id.Clock)))
					}
				}
			case isYString(parent):
				v2.parentInfoEncoder.writeValue(1)
				v2.stringEncoder.writeValue(parent.(*yString).str)
			case isIDPtr(parent) && parent.(*ID) != nil:
				parentID := parent.(*ID)
				v2.parentInfoEncoder.writeValue(0)
				v2.clientEncoder.writeValue(uint64(parentID.Client))
				if v2.trustedFullState {
					v2.leftClockEncoder.writeTrusted(int64(parentID.Clock))
				} else {
					v2.recordErr(v2.leftClockEncoder.writeValue(int64(parentID.Clock)))
				}
			default:
				return fmt.Errorf("write struct: item %v has nil origin/rightOrigin and invalid parent %T", item.id, parent)
			}
			if parentSub != "" {
				v2.stringEncoder.writeValue(parentSub)
			}
		}

		length := item.length
		if !content.hasASCIIWidth(length) {
			length = content.contentLength()
		}
		stringEncoder := v2.stringEncoder
		stringEncoder.text = append(stringEncoder.text, content.value...)
		lens := stringEncoder.lens
		if v2.trustedFullState && lens.count > 0 && lens.s == uint64(length) {
			lens.count++
		} else {
			lens.writeValue(uint64(length))
		}
	}
	return nil
}

// ---------------------------------------------------------------- from update_encoder_v2_fast.go
const maxRetainedFullV2EncoderBytes = 2 << 20

var fullV2EncoderPool sync.Pool

func resetIntDiffOptRleEncoder(e *intDiffOptRLEEncoder) {
	e.buf = e.buf[:0]
	e.s = 0
	e.count = 0
	e.diff = 0
}

func resetUintOptRleEncoder(e *uintOptRLEEncoder) {
	e.buf = e.buf[:0]
	e.s = 0
	e.count = 0
}

func resetRleEncoder(e *rleEncoder) {
	e.buf.Reset()
	e.state = 0
	e.count = 0
	e.started = false
}

func resetFullV2Encoder(e *updateEncoderV2) {
	e.rest.Reset()
	e.dsCurrVal = 0
	resetIntDiffOptRleEncoder(e.keyClockEncoder)
	resetUintOptRleEncoder(e.clientEncoder)
	resetIntDiffOptRleEncoder(e.leftClockEncoder)
	resetIntDiffOptRleEncoder(e.rightClockEncoder)
	resetRleEncoder(e.infoEncoder)
	e.stringEncoder.text = e.stringEncoder.text[:stringEncoderPrefixHeadroom]
	resetUintOptRleEncoder(e.stringEncoder.lens)
	resetRleEncoder(e.parentInfoEncoder)
	resetUintOptRleEncoder(e.typeRefEncoder)
	resetUintOptRleEncoder(e.lenEncoder)
	clear(e.keyMap)
	e.keyClock = 0
	e.hasDeleted = false
	e.trustedFullState = true
	e.trustedClocksBounded = false
	e.err = nil
}

func acquireFullV2Encoder(structCount int) *updateEncoderV2 {
	// This pool is intentionally exclusive to encodeFullStateAsUpdateV2 below:
	// resetFullV2Encoder enables trustedFullState. A partial/target-state caller
	// must use NewUpdateEncoderV2 and retain all hostile-input checks.
	if pooled := fullV2EncoderPool.Get(); pooled != nil {
		encoder := pooled.(*updateEncoderV2)
		resetFullV2Encoder(encoder)
		return encoder
	}
	encoder := newUpdateEncoderV2(structCount)
	encoder.trustedFullState = true
	return encoder
}

func releaseFullV2Encoder(e *updateEncoderV2) {
	retained := e.rest.Cap() +
		cap(e.keyClockEncoder.buf) + cap(e.clientEncoder.buf) +
		cap(e.leftClockEncoder.buf) + cap(e.rightClockEncoder.buf) +
		e.infoEncoder.buf.Cap() + cap(e.stringEncoder.text) +
		cap(e.stringEncoder.lens.buf) + e.parentInfoEncoder.buf.Cap() +
		cap(e.typeRefEncoder.buf) + cap(e.lenEncoder.buf)
	if retained <= maxRetainedFullV2EncoderBytes {
		fullV2EncoderPool.Put(e)
	}
}

func writeTrustedClock(v2 *updateEncoderV2, e *intDiffOptRLEEncoder, value int64) {
	diff := value - e.s
	if e.count > 0 && e.diff == diff {
		e.s = value
		e.count++
		return
	}
	// A live StructStore is not necessarily locally generated: ApplyUpdate can
	// integrate hostile V1 clocks. Preserve the generic encoder's overflow guard
	// so diff*2 below cannot silently wrap and corrupt the emitted V2 stream.
	if !v2.trustedClocksBounded && (diff > maxIntDiffOptRleDiff || diff < -maxIntDiffOptRleDiff) {
		v2.recordErr(fmt.Errorf("IntDiffOptRleEncoder: diff %d out of encodable range", diff))
		return
	}
	if e.count > 0 {
		encodedDiff := e.diff * 2
		if e.count != 1 {
			encodedDiff++
		}
		negative := encodedDiff < 0
		magnitude := uint64(encodedDiff)
		if negative {
			magnitude = uint64(-encodedDiff)
		}
		e.buf = appendVarIntMag(e.buf, magnitude, negative)
		if e.count > 1 {
			e.buf = appendVarUint(e.buf, uint64(e.count-2))
		}
	}
	e.count = 1
	e.diff = diff
	e.s = value
}

// writeFullStateStructs is the trusted live-store counterpart of writeStructs.
// ContentString items with an origin can be emitted without the generic encoder
// interface and hostile-clock checks. Root items and other content kinds retain
// their established Item.Write implementations.
func (v2 *updateEncoderV2) writeFullStateStructs(structs []abstractStruct) error {
	clientEncoder := v2.clientEncoder
	leftClockEncoder := v2.leftClockEncoder
	rightClockEncoder := v2.rightClockEncoder
	infoEncoder := v2.infoEncoder
	stringEncoder := v2.stringEncoder

	for i := 0; i < len(structs); i++ {
		item, ok := structs[i].(*itemStruct)
		if !ok {
			if structs[i].isDeleted() {
				v2.hasDeleted = true
			}
			if err := structs[i].writeStruct(v2, 0); err != nil {
				return err
			}
			continue
		}
		if item.info&bit3 != 0 {
			v2.hasDeleted = true
		}
		content, ok := item.content.(*contentString)
		if !ok || item.origin == nil && item.rightOrigin == nil {
			if err := item.writeStruct(v2, 0); err != nil {
				return err
			}
			continue
		}

		origin := item.origin
		rightOrigin := item.rightOrigin
		info := uint8(refContentString)
		if origin != nil {
			info |= bit8
		}
		if rightOrigin != nil {
			info |= bit7
		}
		if item.parentSub != "" {
			info |= bit6
		}
		if infoEncoder.started && infoEncoder.state == info {
			infoEncoder.count++
		} else {
			infoEncoder.writeValue(info)
		}

		if origin != nil {
			client := uint64(origin.Client)
			if clientEncoder.count > 0 && clientEncoder.s == client {
				clientEncoder.count++
			} else {
				clientEncoder.writeValue(client)
			}
			writeTrustedClock(v2, leftClockEncoder, int64(origin.Clock))
		}
		if rightOrigin != nil {
			client := uint64(rightOrigin.Client)
			if clientEncoder.count > 0 && clientEncoder.s == client {
				clientEncoder.count++
			} else {
				clientEncoder.writeValue(client)
			}
			writeTrustedClock(v2, rightClockEncoder, int64(rightOrigin.Clock))
		}

		length := item.length
		if !content.hasASCIIWidth(length) {
			length = content.contentLength()
		}
		stringEncoder.text = append(stringEncoder.text, content.value...)
		lens := stringEncoder.lens
		if lens.count > 0 && lens.s == uint64(length) {
			lens.count++
		} else {
			lens.writeValue(uint64(length))
		}
	}
	return nil
}

// encodeFullStateAsUpdateV2 handles only the common nil-target, no-pending path.
// It writes the live store directly instead of constructing and then decoding a
// zero state vector, allocating two state maps, and sorting a one-client map.
// Partial updates and conversion/lazy-update paths retain the fully checked
// generic encoder.
func encodeFullStateAsUpdateV2(store *structStore) ([]byte, error) {
	structCount := 0
	clocksBounded := true
	store.forEachClient(func(_ Number, structs *clientStructList) bool {
		structCount += structs.Len()
		clocksBounded = clocksBounded && structs.clocksFitV2FastPath()
		return true
	})
	encoder := acquireFullV2Encoder(structCount)
	defer releaseFullV2Encoder(encoder)
	encoder.trustedClocksBounded = clocksBounded
	rest := encoder.rest
	clientCount := store.clientCountValue()
	var clients []Number
	writeVarUint(rest, uint64(clientCount))

	if clientCount == 1 {
		var writeErr error
		store.forEachClient(func(client Number, structs *clientStructList) bool {
			writeErr = structs.writeFullStateV2(encoder, client)
			return false
		})
		if writeErr != nil {
			return nil, fmt.Errorf("write state as update: %w", writeErr)
		}
	} else if clientCount > 1 {
		clients = store.appendClientIDs(make([]Number, 0, clientCount))
		sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
		for _, client := range clients {
			structs, _ := store.clientStructs(client)
			if err := structs.writeFullStateV2(encoder, client); err != nil {
				return nil, fmt.Errorf("write state as update: %w", err)
			}
		}
	}

	if !encoder.hasDeleted {
		writeVarUint(rest, 0)
	} else if err := writeDeleteSetFromStructStore(encoder, store, clients); err != nil {
		return nil, fmt.Errorf("write state as update: delete set encode failed: %w", err)
	}

	out := encoder.toBytes()
	if err := encoder.encodeError(); err != nil {
		return nil, fmt.Errorf("encode state as update: encode failed: %w", err)
	}
	return out, nil
}
