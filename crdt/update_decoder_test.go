package crdt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"unsafe"
)

// ---------------------------------------------------------------- from update_decoder_id_arena_test.go
func TestDecoderIDArenaDoesNotReuseLiveStorage(t *testing.T) {
	t.Parallel()

	type arenaCase struct {
		name    string
		reserve func(uint64)
		alloc   func(Number, Number) *ID
	}
	v1 := &updateDecoderV1{}
	v2 := &updateDecoderV2{}
	cases := []arenaCase{
		{name: "v1", reserve: v1.reserveIDs, alloc: v1.allocID},
		{name: "v2", reserve: v2.reserveIDs, alloc: v2.allocID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.reserve(1)
			old := tc.alloc(1, 2)
			_ = tc.alloc(3, 4)
			fallback := tc.alloc(5, 6)
			if fallback.Client != 5 || fallback.Clock != 6 {
				t.Fatalf("fallback ID = %v, want {5 6}", fallback)
			}

			tc.reserve(1)
			fresh := tc.alloc(7, 8)
			if old == fresh {
				t.Fatal("new reservation reused storage backing a live ID")
			}
			if old.Client != 1 || old.Clock != 2 {
				t.Fatalf("old ID changed after reservation: %v", old)
			}
		})
	}
}

// ---------------------------------------------------------------- from update_decoder_test.go
// TestWriteReadDsClock verifies encoding/decoding of DeleteSet clock values
func TestWriteReadDsClock(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalClock := Number(12345)
	encoder.writeDSClock(originalClock)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedClock, err := decoder.readDSClock()
	if err != nil {
		t.Fatalf("ReadDsClock failed: %v", err)
	}

	if decodedClock != originalClock {
		t.Errorf("DsClock mismatch: got %d, want %d", decodedClock, originalClock)
	}
}

// TestWriteReadDsLen verifies encoding/decoding of DeleteSet length values
func TestWriteReadDsLen(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalLen := Number(67890)
	if err := encoder.writeDSLength(originalLen); err != nil {
		t.Fatalf("WriteDsLen failed: %v", err)
	}
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedLen, err := decoder.readDSLength()
	if err != nil {
		t.Fatalf("ReadDsLen failed: %v", err)
	}

	if decodedLen != originalLen {
		t.Errorf("DsLen mismatch: got %d, want %d", decodedLen, originalLen)
	}
}

// TestWriteReadID verifies encoding/decoding of ID structs
func TestWriteReadID(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalID := &ID{Client: 42, Clock: 100}
	encoder.writeID(originalID)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedID, err := decoder.readID()
	if err != nil {
		t.Fatalf("ReadID failed: %v", err)
	}

	if decodedID.Client != originalID.Client || decodedID.Clock != originalID.Clock {
		t.Errorf("ID mismatch: got %+v, want %+v", decodedID, originalID)
	}
}

// TestWriteReadClient verifies encoding/decoding of client numbers
func TestWriteReadClient(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalClient := Number(99)
	encoder.writeClient(originalClient)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedClient, err := decoder.readClient()
	if err != nil {
		t.Fatalf("ReadClient failed: %v", err)
	}

	if decodedClient != originalClient {
		t.Errorf("Client mismatch: got %d, want %d", decodedClient, originalClient)
	}
}

// TestWriteReadInfo verifies encoding/decoding of info bytes
func TestWriteReadInfo(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalInfo := uint8(0xAB)
	encoder.writeInfo(originalInfo)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedInfo, err := decoder.readInfo()
	if err != nil {
		t.Fatalf("ReadInfo failed: %v", err)
	}

	if decodedInfo != originalInfo {
		t.Errorf("Info mismatch: got 0x%X, want 0x%X", decodedInfo, originalInfo)
	}
}

func TestReadInfoEmptyInputReturnsEOF(t *testing.T) {
	decoder := newUpdateDecoderV1(nil)
	if _, err := decoder.readInfo(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadInfo empty error = %v, want io.EOF", err)
	}
}

// TestWriteReadString verifies encoding/decoding of strings
func TestWriteReadString(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalStr := "test string content"
	if err := encoder.writeStringValue(originalStr); err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedStr, err := decoder.readStringValue()
	if err != nil {
		t.Fatalf("ReadString failed: %v", err)
	}

	if decodedStr != originalStr {
		t.Errorf("String mismatch: got '%s', want '%s'", decodedStr, originalStr)
	}
}

// TestWriteReadParentInfo verifies encoding/decoding of parent info flags
func TestWriteReadParentInfo(t *testing.T) {
	tests := []struct {
		name     string
		input    bool
		expected bool
	}{
		{"true value", true, true},
		{"false value", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder := newUpdateEncoderV1()
			encoder.writeParentInfo(tt.input)
			data := encoder.toBytes()

			decoder := newUpdateDecoderV1(data)
			decoded, err := decoder.readParentInfo()
			if err != nil {
				t.Fatalf("ReadParentInfo failed: %v", err)
			}

			if decoded != tt.expected {
				t.Errorf("ParentInfo mismatch: got %v, want %v", decoded, tt.expected)
			}
		})
	}
}

// TestWriteReadTypeRef verifies encoding/decoding of type references
func TestWriteReadTypeRef(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalRef := uint8(0x12)
	encoder.writeTypeRef(originalRef)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedRef, err := decoder.readTypeRef()
	if err != nil {
		t.Fatalf("ReadTypeRef failed: %v", err)
	}

	if decodedRef != originalRef {
		t.Errorf("TypeRef mismatch: got 0x%X, want 0x%X", decodedRef, originalRef)
	}
}

// TestWriteReadLen verifies encoding/decoding of length values
func TestWriteReadLen(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalLen := Number(1024)
	encoder.writeLength(originalLen)
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedLen, err := decoder.readLength()
	if err != nil {
		t.Fatalf("ReadLen failed: %v", err)
	}

	if decodedLen != originalLen {
		t.Errorf("Len mismatch: got %d, want %d", decodedLen, originalLen)
	}
}

// TestWriteReadAny verifies encoding/decoding of arbitrary data types
func TestWriteReadAny(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected any
	}{
		{"string type", "test any string", "test any string"},
		{"integer type", Number(42), Number(42)},
		{"boolean true", true, true},
		{"boolean false", false, false},
		{"byte array", []uint8{0x01, 0x02, 0x03}, []uint8{0x01, 0x02, 0x03}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoder := newUpdateEncoderV1()
			if err := encoder.writeAnyValue(tt.input); err != nil {
				t.Fatalf("WriteAny failed: %v", err)
			}
			data := encoder.toBytes()

			decoder := newUpdateDecoderV1(data)
			decoded, err := decoder.readAnyValue()
			if err != nil {
				t.Fatalf("ReadAny failed: %v", err)
			}

			if fmt.Sprintf("%v", decoded) != fmt.Sprintf("%v", tt.expected) {
				t.Errorf("Any mismatch: got %v, want %v", decoded, tt.expected)
			}
		})
	}
}

// TestWriteReadBuf verifies encoding/decoding of byte buffers
func TestWriteReadBuf(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalBuf := []uint8{0x01, 0x02, 0x03, 0x04}
	if err := encoder.writeBuffer(originalBuf); err != nil {
		t.Fatalf("WriteBuf failed: %v", err)
	}
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedBuf, err := decoder.readBuffer()
	if err != nil {
		t.Fatalf("ReadBuf failed: %v", err)
	}

	if !bytes.Equal(decodedBuf, originalBuf) {
		t.Errorf("Buf mismatch: got %v, want %v", decodedBuf, originalBuf)
	}
}

// TestWriteReadJson verifies encoding/decoding of JSON objects
func TestWriteReadJson(t *testing.T) {
	type TestStruct struct {
		Key   string
		Value int
	}
	originalObj := TestStruct{Key: "test", Value: 123}

	encoder := newUpdateEncoderV1()
	if err := encoder.writeJSONValue(originalObj); err != nil {
		t.Fatalf("WriteJson failed: %v", err)
	}
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedObj, err := decoder.readJSONValue()
	if err != nil {
		t.Fatalf("ReadJson failed: %v", err)
	}

	// Convert both to JSON for comparison
	originalJSON, _ := json.Marshal(originalObj)
	decodedJSON, _ := json.Marshal(decodedObj)
	if string(originalJSON) != string(decodedJSON) {
		t.Errorf("Json mismatch: got %s, want %s", decodedJSON, originalJSON)
	}
}

// TestWriteReadKey verifies encoding/decoding of key strings
func TestWriteReadKey(t *testing.T) {
	encoder := newUpdateEncoderV1()
	originalKey := "test_key_123"
	if err := encoder.writeKey(originalKey); err != nil {
		t.Fatalf("WriteKey failed: %v", err)
	}
	data := encoder.toBytes()

	decoder := newUpdateDecoderV1(data)
	decodedKey, err := decoder.readKey()
	if err != nil {
		t.Fatalf("ReadKey failed: %v", err)
	}

	if decodedKey != originalKey {
		t.Errorf("Key mismatch: got '%s', want '%s'", decodedKey, originalKey)
	}
}

// ---------------------------------------------------------------- from update_decoder_v2_column_view_test.go
var updateDecoderV2AllocationSink *updateDecoderV2

func TestReadV2ColumnMatchesCopyingDecoder(t *testing.T) {
	check := func(raw []byte) {
		t.Helper()
		copying := bytes.NewBuffer(append([]byte(nil), raw...))
		wantRaw, wantErr := readVarUint8Array(copying)
		var want []byte
		if wantErr == nil {
			want, _ = wantRaw.([]byte)
		}

		viewingRaw := append([]byte(nil), raw...)
		viewing := bytes.NewBuffer(viewingRaw)
		got := readV2Column(viewingRaw, viewing)
		if !bytes.Equal(got, want) || !bytes.Equal(viewing.Bytes(), copying.Bytes()) {
			t.Fatalf("column framing differs for %x: got data=%x rest=%x, want data=%x rest=%x (copy error %v)", raw, got, viewing.Bytes(), want, copying.Bytes(), wantErr)
		}
	}

	for first := 0; first < 256; first++ {
		check([]byte{byte(first)})
		for second := 0; second < 256; second++ {
			check([]byte{byte(first), byte(second)})
		}
	}
	check(append([]byte{3}, []byte("abc-tail")...))
	check(append([]byte{0x80, 0x01}, make([]byte, 129)...))
	check([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02})
}

func TestReadV2ColumnReturnsInputView(t *testing.T) {
	framed := []byte{3, 'a', 'b', 'c', 'z'}
	decoder := bytes.NewBuffer(framed)
	column := readV2Column(framed, decoder)
	if len(column) != 3 || &column[0] != &framed[1] {
		t.Fatalf("column was copied instead of viewed: data=%q", column)
	}
	if got := decoder.Bytes(); !bytes.Equal(got, []byte{'z'}) {
		t.Fatalf("column view left wrong remainder: %x", got)
	}
}

func TestUpdateDecoderV2ColumnViewsAvoidPerColumnAllocations(t *testing.T) {
	doc := newDoc("decoder-columns", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	m.Set("key", "value")
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(20, func() {
		updateDecoderV2AllocationSink = newUpdateDecoderV2(update)
	})
	if allocations > 2 {
		t.Fatalf("NewUpdateDecoderV2 allocated %.0f objects, want at most 2; column payloads and decoder state must remain zero-copy and coallocated", allocations)
	}
}

func TestUpdateDecoderV2StateIsReaderLocal(t *testing.T) {
	doc := newDoc("decoder-local-state", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "alpha bravo charlie", Object{})
	attrs := newObject()
	attrs.Set("bold", true)
	txt.Format(2, 5, attrs)
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}

	first := newUpdateDecoderV2(update)
	second := newUpdateDecoderV2(update)
	if first == second {
		t.Fatal("NewUpdateDecoderV2 reused the public decoder across readers")
	}

	firstPointers := []unsafe.Pointer{
		unsafe.Pointer(first.rest),
		unsafe.Pointer(first.keyClockDecoder),
		unsafe.Pointer(first.clientDecoder),
		unsafe.Pointer(first.leftClockDecoder),
		unsafe.Pointer(first.rightClockDecoder),
		unsafe.Pointer(first.infoDecoder),
		unsafe.Pointer(first.stringDecoder),
		unsafe.Pointer(first.parentInfoDecoder),
		unsafe.Pointer(first.typeRefDecoder),
		unsafe.Pointer(first.lenDecoder),
		unsafe.Pointer(first.stringDecoder.lens),
	}
	secondPointers := []unsafe.Pointer{
		unsafe.Pointer(second.rest),
		unsafe.Pointer(second.keyClockDecoder),
		unsafe.Pointer(second.clientDecoder),
		unsafe.Pointer(second.leftClockDecoder),
		unsafe.Pointer(second.rightClockDecoder),
		unsafe.Pointer(second.infoDecoder),
		unsafe.Pointer(second.stringDecoder),
		unsafe.Pointer(second.parentInfoDecoder),
		unsafe.Pointer(second.typeRefDecoder),
		unsafe.Pointer(second.lenDecoder),
		unsafe.Pointer(second.stringDecoder.lens),
	}
	for i := range firstPointers {
		if firstPointers[i] == secondPointers[i] {
			t.Fatalf("decoder state pointer %d is shared across readers", i)
		}
	}

	// The string length cursor is one level deeper than the decoder pointers
	// above. Pin it separately: sharing only this buffer corrupts readers when
	// their string columns advance at different rates.
	if first.stringDecoder.lens.buf == second.stringDecoder.lens.buf {
		t.Fatal("string length buffer is shared across readers")
	}
}
