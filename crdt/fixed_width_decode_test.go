package crdt

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
)

// TestFixedWidthScalarsRejectEveryTruncatedPrefix pins the lib0 decoder used by
// yjs 13.6.31: decoding.readFromDataView throws on an undersized fixed-width
// value before its cursor advances. bytes.Buffer.Read's (n, nil) short-read
// behaviour previously accepted every non-empty prefix and zero-filled the
// missing bytes.
func TestFixedWidthScalarsRejectEveryTruncatedPrefix(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		read    func(*bytes.Buffer) (any, error)
	}{
		{name: "float32", payload: []byte{0x3f, 0x80, 0x00, 0x00}, read: readFloat32},
		{name: "float64", payload: []byte{0x3f, 0xf0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, read: readFloat64},
		{name: "bigint64", payload: []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, read: readBigInt64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for size := 0; size < len(tc.payload); size++ {
				prefix := bytes.Clone(tc.payload[:size])
				decoder := bytes.NewBuffer(bytes.Clone(prefix))
				if _, err := tc.read(decoder); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("prefix length %d: error=%v, want io.ErrUnexpectedEOF", size, err)
				}
				if got := decoder.Bytes(); !bytes.Equal(got, prefix) {
					t.Errorf("prefix length %d: decoder advanced to %x, want unchanged %x", size, got, prefix)
				}
			}
		})
	}
}

// TestFixedWidthAnyDispatchPropagatesTruncation checks the actual any-encoded
// wire boundary, not only the leaf helpers. ReadAny consumes the one-byte tag,
// while the rejected scalar payload itself must remain untouched.
func TestFixedWidthAnyDispatchPropagatesTruncation(t *testing.T) {
	tests := []struct {
		name    string
		tag     byte
		payload []byte
	}{
		{name: "float32", tag: 124, payload: []byte{0x3f, 0x80, 0x00, 0x00}},
		{name: "float64", tag: 123, payload: []byte{0x3f, 0xf0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{name: "bigint64", tag: 122, payload: []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for size := 0; size < len(tc.payload); size++ {
				prefix := bytes.Clone(tc.payload[:size])
				encoded := append([]byte{tc.tag}, prefix...)
				decoder := bytes.NewBuffer(encoded)
				if _, err := readAny(decoder); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("prefix length %d: error=%v, want io.ErrUnexpectedEOF", size, err)
				}
				if got := decoder.Bytes(); !bytes.Equal(got, prefix) {
					t.Errorf("prefix length %d: payload advanced to %x, want unchanged %x", size, got, prefix)
				}
			}
		})
	}
}

func TestFixedWidthScalarsConsumeOnlyTheirPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		read    func(*bytes.Buffer) (any, error)
		want    any
	}{
		{name: "float32", payload: []byte{0x3f, 0x80, 0x00, 0x00}, read: readFloat32, want: float32(1)},
		{name: "float64", payload: []byte{0x3f, 0xf0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, read: readFloat64, want: float64(1)},
		{name: "bigint64", payload: []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, read: readBigInt64, want: int64(math.MaxInt64)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decoder := bytes.NewBuffer(append(bytes.Clone(tc.payload), 0xa5))
			got, err := tc.read(decoder)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("value=%v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
			if trailing, err := decoder.ReadByte(); err != nil || trailing != 0xa5 {
				t.Fatalf("trailing byte=%#x error=%v, want 0xa5", trailing, err)
			}
		})
	}
}
