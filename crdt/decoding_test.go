package crdt

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

func mustReadVarUint(t testing.TB, decoder *bytes.Buffer) uint64 {
	t.Helper()
	value, err := readVarUint(decoder)
	if err != nil {
		t.Fatalf("read varuint: %v", err)
	}
	return value
}

func TestHasContent(t *testing.T) {
	decoder := bytes.NewBuffer([]byte{0x7F})
	if !hasContent(decoder) {
		t.Errorf("Expected decoder to have content, but it did not")
	}
}

func TestReadVarUnit(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeVarUint(encoder, 255)

	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readVarUintAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if value.(uint64) != 255 {
		t.Errorf("Expected value to be 255, got %d", value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarUint(encoder, 256)
	decoder = bytes.NewBuffer(encoder.Bytes())
	decoded := mustReadVarUint(t, decoder)
	if decoded != 256 {
		t.Errorf("Expected value to be 256, got %d", decoded)
	}
}

func TestReadUnit8(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeByte(encoder, 128)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readUint8(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value != 128 {
		t.Errorf("Expected value to be 128, got %d", value)
	}
}

func TestReadVarInt(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeVarInt(encoder, 128)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readVarInt(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if value.(int) != 128 {
		t.Errorf("Expected value to be 128, got %d", value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarInt(encoder, -128)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarInt(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int) != -128 {
		t.Errorf("Expected value to be -128, got %d", value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarInt(encoder, math.MaxInt32)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarInt(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int) != math.MaxInt32 {
		t.Errorf("Expected value to be %d, got %d", math.MaxInt32, value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarInt(encoder, 64)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarInt(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int) != 64 {
		t.Errorf("Expected value to be 64, got %d", value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarInt(encoder, 63)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarInt(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int) != 63 {
		t.Errorf("Expected value to be 63, got %d", value)
	}
}

func TestReadFloat32(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeFloat32(encoder, 1.0)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readFloat32(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if value.(float32) != 1.0 {
		t.Errorf("Expected value to be 1.0, got %f", value)
	}
}

func TestReadFloat64(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeFloat64(encoder, 1.0)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readFloat64(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(float64) != 1.0 {
		t.Errorf("Expected value to be 1.0, got %f", value)
	}
}

func TestReadBigInt64(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	writeInt64(encoder, math.MaxInt64)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readBigInt64(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int64) != math.MaxInt64 {
		t.Errorf("Expected value to be %d, got %d", math.MaxInt64, value)
	}
}

func TestReadVarString(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	_ = writeString(encoder, "hello")

	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readVarString(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if value.(string) != "hello" {
		t.Errorf("Expected value to be 'hello', got '%s'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeString(encoder, "")
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarString(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(string) != "" {
		t.Errorf("Expected value to be '', got '%s'", value)
	}
}

func TestReadString(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	_ = writeString(encoder, "hello")
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readString(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value != "hello" {
		t.Errorf("Expected value to be 'hello', got '%s'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeString(encoder, "")
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readString(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value != "" {
		t.Errorf("Expected value to be '', got '%s'", value)
	}
}

func TestReadObject(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	obj := newObject()
	obj.Set("hello", "world")
	_ = writeObject(encoder, obj)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readObject(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Object).GetOr("hello") != "world" {
		t.Errorf("Expected value to be 'world', got '%s'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeObject(encoder, newObject())
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readObject(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	obj = value.(Object)
	if obj.Len() != 0 {
		t.Errorf("Expected value to be empty, got '%d'", obj.Len())
	}
}

func TestReadArray(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	array := []any{"hello", "world"}
	_ = writeArray(encoder, array)

	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readArray(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(ArrayAny)[0] != "hello" {
		t.Errorf("Expected value to be 'hello', got '%s'", value)
	}
	if value.(ArrayAny)[1] != "world" {
		t.Errorf("Expected value to be 'world', got '%s'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeArray(encoder, []any{})
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readArray(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(value.(ArrayAny)) != 0 {
		t.Errorf("Expected value to be empty, got '%d'", len(value.(ArrayAny)))
	}
}

func TestReadVarUint8Array(t *testing.T) {
	encoder := bytes.NewBuffer(nil)
	array := []uint8{1, 2, 3}
	writeVarUint8Array(encoder, array)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readVarUint8Array(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.([]uint8)[0] != 1 {
		t.Errorf("Expected value to be 1, got '%d'", value)
	}
	if value.([]uint8)[1] != 2 {
		t.Errorf("Expected value to be 2, got '%d'", value)
	}
	if value.([]uint8)[2] != 3 {
		t.Errorf("Expected value to be 3, got '%d'", value)
	}

	encoder = bytes.NewBuffer(nil)
	writeVarUint8Array(encoder, []uint8{})
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readVarUint8Array(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if len(value.([]uint8)) != 0 {
		t.Errorf("Expected value to be empty, got '%d'", len(value.([]uint8)))
	}
}

// TestReadVarUint8ArrayTruncated asserts that a declared length larger than the
// bytes actually present is reported as a truncation (io.ErrUnexpectedEOF)
// rather than silently capped to a short buffer with a nil error. The allocation
// must still be bounded by the remaining bytes (SEC-002), and the bytes that
// were present are returned so callers that type-assert keep working.
func TestReadVarUint8ArrayTruncated(t *testing.T) {
	// VarUint length = 10, but only 3 payload bytes follow.
	decoder := bytes.NewBuffer(nil)
	writeVarUint(decoder, 10)
	decoder.Write([]byte{1, 2, 3})

	value, err := readVarUint8Array(decoder)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF on truncated payload, got %v", err)
	}
	got, ok := value.([]byte)
	if !ok {
		t.Fatalf("expected []byte result even on error, got %T", value)
	}
	// allocation bounded to the 3 bytes that were present (not the declared 10).
	if len(got) != 3 {
		t.Fatalf("expected the 3 present bytes back, got %d bytes", len(got))
	}
}

func TestReadAny(t *testing.T) {
	// write Undefined value.
	encoder := bytes.NewBuffer(nil)
	_ = writeAny(encoder, nil)
	decoder := bytes.NewBuffer(encoder.Bytes())
	value, err := readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	_, ok := value.(UndefinedType)
	if !ok {
		t.Errorf("Expected value to be undefined, got '%v'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, Undefined)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	_, ok = value.(UndefinedType)
	if !ok {
		t.Errorf("Expected value to be undefined, got '%v'", value)
	}

	// write Null value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, Null)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	_, ok = value.(NullType)
	if !ok {
		t.Errorf("Expected value to be null, got '%v'", value)
	}

	encoder = bytes.NewBuffer(nil)
	var a *int
	_ = writeAny(encoder, a)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	_, ok = value.(NullType)
	if !ok {
		t.Errorf("Expected value to be null, got '%v'", value)
	}

	// write string value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, "hello")
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(string) != "hello" {
		t.Errorf("Expected value to be 'hello', got '%s'", value)
	}

	// write int8 value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, int8(math.MaxInt8))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Number) != math.MaxInt8 {
		t.Errorf("Expected value to be %d, got %d", math.MaxInt8, value)
	}

	// write int16 value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, int16(math.MaxInt16))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Number) != math.MaxInt16 {
		t.Errorf("Expected value to be %d, got %d", math.MaxInt16, value)
	}

	// write int64 value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, int64(math.MaxInt64))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(int64) != math.MaxInt64 {
		t.Errorf("Expected value to be %d, got %d", math.MaxInt64, value)
	}

	// write number value within the 31-bit varint range: stays a varint integer
	// (tag 125) and decodes back to Number.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, Number(bits31))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Number) != bits31 {
		t.Errorf("Expected value to be %d, got %d", bits31, value)
	}

	// write a large integer (|n| > 2^31-1) that is not float32-exact: per lib0,
	// it falls through to float64 (tag 123), so it decodes back as float64 — not
	// a varint integer. (Mirrors lib0 writeAny: numbers above bits31 are floats.)
	largeInt := (Number(1) << 40) + 1
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, largeInt)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if f, ok := value.(float64); !ok || f != float64(largeInt) {
		t.Errorf("Expected large int to round-trip as float64 %v, got %v (%T)", float64(largeInt), value, value)
	}

	// write float32 value. Per lib0 writeAny, an *integer-valued* float (1.0) is
	// encoded as a varint integer (tag 125); to exercise the float32 tag (124) we
	// use a non-integer that is exactly float32-representable.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, float32(0.5))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(float32) != 0.5 {
		t.Errorf("Expected value to be 0.5, got '%f'", value)
	}

	// write float64 value. 0.1 is not exactly float32-representable, so lib0
	// emits float64 (tag 123) and it round-trips as float64.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, 0.1)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(float64) != 0.1 {
		t.Errorf("Expected value to be 0.1, got '%f'", value)
	}

	// write boolean value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, false)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(bool) != false {
		t.Errorf("Expected value to be false, got '%t'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, true)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(bool) != true {
		t.Errorf("Expected value to be true, got '%t'", value)
	}

	// write uint8 array value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, []uint8{1, 2, 3})
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.([]uint8)[0] != 1 {
		t.Errorf("Expected value to be 1, got '%d'", value)
	}
	if value.([]uint8)[1] != 2 {
		t.Errorf("Expected value to be 2, got '%d'", value)
	}
	if value.([]uint8)[2] != 3 {
		t.Errorf("Expected value to be 3, got '%d'", value)
	}

	// write object array value.
	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, []any{"hello", "world"})
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(ArrayAny)[0] != "hello" {
		t.Errorf("Expected value to be 'hello', got '%s'", value)
	}
	if value.(ArrayAny)[1] != "world" {
		t.Errorf("Expected value to be 'world', got '%s'", value)
	}

	encoder = bytes.NewBuffer(nil)
	_ = writeAny(encoder, MakeObject("hello", "world"))
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Object).GetOr("hello") != "world" {
		t.Errorf("Expected value to be 'world', got '%s'", value)
	}

	// write object value.
	encoder = bytes.NewBuffer(nil)
	obj := newObject()
	obj.Set("hello", "world")
	_ = writeAny(encoder, obj)
	decoder = bytes.NewBuffer(encoder.Bytes())
	value, err = readAny(decoder)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if value.(Object).GetOr("hello") != "world" {
		t.Errorf("Expected value to be 'world', got '%s'", value)
	}
}
