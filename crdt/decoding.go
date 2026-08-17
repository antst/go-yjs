package crdt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/antst/go-yjs/internal/lib0"
)

// boundExceeded reports whether a decoded element count is provably larger than
// the undecoded bytes can hold: each element costs at least minBytesPerElem bytes
// on the wire, so a count above remaining/minBytesPerElem is a truncated or
// hostile frame. It is the single home for the make-prealloc DoS guard shared by
// the delete-set and awareness decoders (mirroring the size>Len() byte-buffer
// checks below); the caller decides the response (error vs cap-and-stop).
func boundExceeded(count uint64, remaining, minBytesPerElem int) bool {
	if minBytesPerElem < 1 {
		minBytesPerElem = 1
	}
	if remaining < 0 {
		remaining = 0
	}
	return count > uint64(remaining/minBytesPerElem)
}

/*
 * Encoding table:
 | Data Type           | Prefix | Encoding Method    | Comment                                                                              |
 | ------------------- | ------ | ------------------ | ------------------------------------------------------------------------------------ |
 | undefined           |    127 |                    | Functions, symbol, and everything that cannot be identified is encoded as undefined  |
 | null                |    126 |                    |                                                                                      |
 | integer             |    125 | WriteVarInt        | Only encodes 32 bit signed integers                                                  |
 | float32             |    124 | WriteFloat32       |                                                                                      |
 | float64             |    123 | WriteFloat64       |                                                                                      |
 | bigint              |    122 | writeBigInt64      |                                                                                      |
 | boolean (false)     |    121 |                    | True and false are different data types so we save the following byte                |
 | boolean (true)      |    120 |                    | - 0b01111000 so the last bit determines whether true or false                        |
 | string              |    119 | writeVarString     |                                                                                      |
 | object<string,any>  |    118 | custom             | Writes {length} then {length} key-value pairs                                        |
 | array<any>          |    117 | custom             | Writes {length} then {length} json values                                            |
 | Uint8Array          |    116 | WriteVarUint8Array | We use Uint8Array for any kind of binary data                                        |
*/

var errOverflow = errors.New("binary: varint overflows a 64-bit integer")

var readAnyLookupTable = []func(decoder *bytes.Buffer) (any, error){
	undefined,     // CASE 127: undefined
	null,          // CASE 126: null
	readVarInt,    // CASE 125: integer
	readFloat32,   // CASE 124: float32
	readFloat64,   // CASE 123: float64
	readBigInt64,  // CASE 122: bigint
	readFalse,     // CASE 121: boolean (false)
	readTrue,      // CASE 120: boolean (true)
	readVarString, // CASE 119: string
}

func init() {
	readAnyLookupTable = append(readAnyLookupTable, readObject)        // CASE 118: object<string,any>
	readAnyLookupTable = append(readAnyLookupTable, readArray)         // CASE 117: array<any>
	readAnyLookupTable = append(readAnyLookupTable, readVarUint8Array) // CASE 116: Uint8Array
}

// undefined returns the Undefined constant indicating an undefined value.
func undefined(decoder *bytes.Buffer) (any, error) {
	return Undefined, nil
}

// null returns the Null constant indicating a null value.
func null(decoder *bytes.Buffer) (any, error) {
	return Null, nil
}

// hasContent checks if the decoder has any content to read.
func hasContent(decoder *bytes.Buffer) bool {
	return decoder.Len() > 0
}

// readVarUintAny decodes a variable-length unsigned integer (Uvarint) from the decoder buffer.
func readVarUintAny(decoder *bytes.Buffer) (any, error) {
	number, err := lib0.ReadVarUint(decoder)
	if err != nil {
		return uint64(0), err
	}

	return number, nil
}

// readVarUintAsNumber decodes a VarUint and converts it to a non-negative Number
// in one step, rejecting any value in [2^63, 2^64) that would wrap to a NEGATIVE
// Number (the negative-wrap class — H#1/H#2/N1). It is the single guarded reader
// the struct/delete-set readers route their untrusted clock / length / count
// varuints through, so a raw decode-then-unchecked-int-cast (which silently
// wraps) cannot be reintroduced by accident. The read error and the overflow
// error are both surfaced to the caller.
func readVarUintAsNumber(rest *bytes.Buffer) (Number, error) {
	// Call binary.ReadUvarint directly rather than readVarUint (G-A): readVarUint
	// returns the decoded value boxed in an `any`, which this wrapper then has to
	// type-assert back to uint64. Reading the uint64 directly drops the boxing and
	// the assertion, lowering the inline cost from 122 to 99. (binary.ReadUvarint is
	// itself a non-inlinable loop costing ~57, so 99 still exceeds the budget of 80
	// — this wrapper does not inline; a hand-rolled uvarint loop measured even worse
	// at 145. Escape analysis already stack-allocated the old `any` box here, so the
	// concrete win is the simpler, assertion-free single call, not fewer allocs.)
	n, err := lib0.ReadVarUint(rest)
	if err != nil {
		return 0, err
	}
	return toNumber(n)
}

// readFalse returns the boolean false value.
func readFalse(decoder *bytes.Buffer) (any, error) {
	return false, nil
}

// readTrue returns the boolean true value.
func readTrue(decoder *bytes.Buffer) (any, error) {
	return true, nil
}

// readUint8 reads and returns a single uint8 from the decoder buffer.
func readUint8(decoder *bytes.Buffer) (uint8, error) {
	data, err := decoder.ReadByte()
	if err != nil {
		return 0, err
	}

	return data, err
}

// readVarInt reads and returns a varint-encoded integer from the decoder buffer.
func readVarInt(decoder *bytes.Buffer) (any, error) {
	data, err := decoder.ReadByte()
	if err != nil {
		return nil, err
	}

	// read the low 6 bits of the byte, the low 6 bits are the number.
	number := data & bits6

	// the first bit is the sign bit, if the first bit is 1, then the number is negative, otherwise it is positive.
	sign := 1
	if data&bit7 > 0 {
		sign = -1
	}

	// the next_flag is the 8th bit, if the next_flag is 0, then the number is done
	if data&bit8 == 0 {
		return sign * Number(number), nil
	}

	n := uint64(number)
	s := uint(6)
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := decoder.ReadByte()
		if err != nil {
			return n, err
		}

		// if the next bit is 0, then the number is done
		if b < bit8 {
			// a 10-byte varint can represent at most a 64-bit integer,
			// where the first 9 bytes each contribute 7 bits,
			// and the 10th byte contributes 1 bit.
			if i == 9 && b > 1 {
				return n, errOverflow
			}

			return sign * Number(n|uint64(b)<<s), nil
		}

		n |= uint64(b&bits7) << s
		s += 7
	}

	return sign * Number(n), errOverflow
}

// readFloat32 reads a 4-byte float32 from the decoder buffer using big-endian encoding.
func readFloat32(decoder *bytes.Buffer) (any, error) {
	var bs [4]byte
	if err := readFixedWidth(decoder, bs[:]); err != nil {
		return float32(0.0), err
	}

	return math.Float32frombits(binary.BigEndian.Uint32(bs[:])), nil
}

// readFloat64 reads an 8-byte float64 from the decoder buffer using big-endian encoding.
func readFloat64(decoder *bytes.Buffer) (any, error) {
	var bs [8]byte
	if err := readFixedWidth(decoder, bs[:]); err != nil {
		return 0.0, err
	}

	return math.Float64frombits(binary.BigEndian.Uint64(bs[:])), nil
}

// readBigInt64 reads an 8-byte int64 from the decoder buffer using big-endian encoding.
func readBigInt64(decoder *bytes.Buffer) (any, error) {
	var buf [8]byte
	if err := readFixedWidth(decoder, buf[:]); err != nil {
		return int64(0), err
	}

	return int64(binary.BigEndian.Uint64(buf[:])), nil
}

// readFixedWidth copies exactly len(dst) bytes or leaves decoder untouched.
// bytes.Buffer.Read is allowed to return a short read with a nil error, which
// made truncated fixed-width scalars look valid after their missing suffix was
// zero-filled. lib0's readFromDataView instead rejects an undersized view before
// advancing its cursor; the length guard preserves that atomic failure shape.
func readFixedWidth(decoder *bytes.Buffer, dst []byte) error {
	if decoder.Len() < len(dst) {
		return io.ErrUnexpectedEOF
	}
	copy(dst, decoder.Next(len(dst)))
	return nil
}

// readVarString decodes a variable-length string from the decoder buffer.
// First reads the string length as a Uvarint, then reads the corresponding bytes.
func readVarString(decoder *bytes.Buffer) (any, error) {
	size, err := binary.ReadUvarint(decoder)
	if err != nil {
		return "", err
	}

	if size == 0 {
		return "", nil
	}

	if size > uint64(decoder.Len()) {
		return "", fmt.Errorf("buffer is not enough, expected %d, got %d", size, decoder.Len())
	}

	buf := make([]byte, size)
	err = binary.Read(decoder, binary.LittleEndian, buf)
	if err != nil {
		return "", err
	}

	return string(buf), nil
}

// readString reads a variable-length string from the decoder buffer.
func readString(decoder *bytes.Buffer) (string, error) {
	return lib0.ReadString(decoder)
}

// maxAnyDepth bounds the nesting depth of object<string,any> / array<any> values
// decoded by ReadAny. lib0/JS recurses natively, so a hostile update encoding
// deeply-nested containers would otherwise overflow the Go stack (an
// unrecoverable fatal error, not a panic). 100 is far beyond anything a real
// document produces while still leaving ample headroom below the goroutine stack
// limit.
const maxAnyDepth = 100

// readObject decodes an object<string, any> from the decoder buffer. It is the
// public, depth-unbounded entry kept for API compatibility; it starts a fresh
// recursion-depth budget.
func readObject(decoder *bytes.Buffer) (any, error) {
	return readObjectDepth(decoder, 0)
}

// readObjectDepth decodes an object<string,any>, threading the recursion depth
// so nested containers cannot blow the stack.
func readObjectDepth(decoder *bytes.Buffer, depth int) (any, error) {
	if depth > maxAnyDepth {
		return nil, fmt.Errorf("read any: nesting depth exceeds %d", maxAnyDepth)
	}

	size, err := binary.ReadUvarint(decoder)
	if err != nil {
		return nil, err
	}

	obj := newObject()
	if size == 0 {
		return obj, nil
	}

	for i := uint64(0); i < size; i++ {
		key, err := readVarString(decoder)
		if err != nil {
			return obj, err
		}

		value, err := readAnyDepth(decoder, depth+1)
		if err != nil {
			return obj, err
		}

		// Set in the order keys appear on the wire, so a JS-produced object
		// round-trips to identical bytes when re-encoded by WriteObject.
		obj.Set(key.(string), value)
	}

	return obj, nil
}

// readArray decodes an array<any> from the decoder buffer. It is the public,
// depth-unbounded entry kept for API compatibility; it starts a fresh
// recursion-depth budget.
func readArray(decoder *bytes.Buffer) (any, error) {
	return readArrayDepth(decoder, 0)
}

// readArrayDepth decodes an array<any>, threading the recursion depth so nested
// containers cannot blow the stack and bounding the declared length against the
// bytes actually available before allocating.
func readArrayDepth(decoder *bytes.Buffer, depth int) (any, error) {
	if depth > maxAnyDepth {
		return nil, fmt.Errorf("read any: nesting depth exceeds %d", maxAnyDepth)
	}

	array := make(ArrayAny, 0)

	size, err := binary.ReadUvarint(decoder)
	if err != nil {
		return array, err
	}

	if size == 0 {
		return array, nil
	}

	// Bound the pre-allocation against the remaining bytes before make(): every
	// array element is at least one byte (its type tag), so a size larger than
	// the buffer holds is malformed/hostile and would otherwise trigger an
	// unbounded allocation. Routed through boundExceeded (the single home for the
	// make-prealloc DoS guard) for parity with the delete-set / awareness decoders.
	if boundExceeded(size, decoder.Len(), 1) {
		return array, fmt.Errorf("read array: declared length %d exceeds remaining %d", size, decoder.Len())
	}

	array = make(ArrayAny, size)
	for i := uint64(0); i < size; i++ {
		value, err := readAnyDepth(decoder, depth+1)
		if err != nil {
			return array, err
		}

		array[i] = value
	}

	return array, nil
}

// ReadVarUnit8Array decodes a Uint8Array (byte slice) from the decoder buffer.
//
// SEC-002: the declared length is bounded by the bytes the decoder actually
// holds before allocating, so an untrusted oversized length field can never
// trigger an unbounded allocation. But an oversized length is also genuinely
// malformed/truncated input: rather than silently returning a short buffer with
// a nil error (which lets parsing continue over corrupted state), we surface
// io.ErrUnexpectedEOF while still capping the allocation to the remaining bytes.
// The return type stays []byte (never nil) so existing callers that type-assert
// the result keep working even on the error path.
func readVarUint8Array(decoder *bytes.Buffer) (any, error) {
	return lib0.ReadVarUint8Array(decoder)
}

// readVarUint decodes a VarUint and surfaces the underlying read error,
// for callers (e.g. message framing) that must distinguish a real 0 from a
// truncated/empty buffer.
func readVarUint(decoder *bytes.Buffer) (uint64, error) {
	value, err := readVarUintAny(decoder)
	if err != nil {
		return 0, err
	}
	number, _ := value.(uint64)
	return number, nil
}

// readAny is the general decoding dispatcher that uses ReadAnyLookupTable. It is
// the public entry; it starts a fresh recursion-depth budget.
func readAny(decoder *bytes.Buffer) (any, error) {
	return readAnyDepth(decoder, 0)
}

// readAnyDepth dispatches one any-encoded value, carrying the current recursion
// depth so the container readers (object/array) can bound nesting. The leaf
// types go through ReadAnyLookupTable as before; object (tag 118) and array
// (tag 117) are dispatched to the depth-aware readers so the budget threads
// through nested containers instead of resetting at each level.
func readAnyDepth(decoder *bytes.Buffer, depth int) (any, error) {
	if depth > maxAnyDepth {
		return nil, fmt.Errorf("read any: nesting depth exceeds %d", maxAnyDepth)
	}

	tag, err := readUint8(decoder)
	if err != nil {
		return nil, err
	}

	refID := 127 - tag
	if int(refID) >= len(readAnyLookupTable) {
		return nil, fmt.Errorf("index out of range. tag:%d refID:%d len:%d", tag, refID, len(readAnyLookupTable))
	}

	switch tag {
	case 118: // object<string,any>
		return readObjectDepth(decoder, depth)
	case 117: // array<any>
		return readArrayDepth(decoder, depth)
	default:
		return readAnyLookupTable[127-tag](decoder)
	}
}
