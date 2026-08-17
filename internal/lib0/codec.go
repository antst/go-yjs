// Package lib0 implements the byte primitives shared by the core update codec
// and the public protocol package. It lives under internal so transport users
// cannot build against the low-level framing details directly.
package lib0

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

const continuationBit = 0x80

// WriteVarUint writes a lib0 variable-length unsigned integer.
func WriteVarUint(dst *bytes.Buffer, value uint64) {
	for value >= continuationBit {
		_ = dst.WriteByte(byte(value) | continuationBit)
		value >>= 7
	}
	_ = dst.WriteByte(byte(value))
}

// ReadVarUint reads a lib0 variable-length unsigned integer and preserves the
// error classes returned by encoding/binary for truncation and overflow.
func ReadVarUint(src *bytes.Buffer) (uint64, error) {
	return binary.ReadUvarint(src)
}

// WriteVarUint8Array writes a length-prefixed byte slice.
func WriteVarUint8Array(dst *bytes.Buffer, value []byte) {
	WriteVarUint(dst, uint64(len(value)))
	_, _ = dst.Write(value)
}

// ReadVarUint8Array reads a length-prefixed byte slice. A declared length that
// exceeds the remaining frame drains only the bounded remainder and reports
// io.ErrUnexpectedEOF rather than allocating from hostile input.
func ReadVarUint8Array(src *bytes.Buffer) ([]byte, error) {
	size, err := ReadVarUint(src)
	if err != nil {
		return []byte{}, err
	}
	if size > uint64(src.Len()) {
		value := make([]byte, src.Len())
		_, _ = io.ReadFull(src, value)
		return value, io.ErrUnexpectedEOF
	}
	value := make([]byte, size)
	_, err = io.ReadFull(src, value)
	return value, err
}

// WriteString writes a byte-length-prefixed UTF-8 string.
func WriteString(dst *bytes.Buffer, value string) error {
	WriteVarUint(dst, uint64(len(value)))
	if len(value) == 1 {
		return dst.WriteByte(value[0])
	}
	_, err := dst.WriteString(value)
	return err
}

// ReadString reads a byte-length-prefixed UTF-8 string. The string conversion
// makes the one immutable copy required by the result.
func ReadString(src *bytes.Buffer) (string, error) {
	size, err := ReadVarUint(src)
	if err != nil {
		return "", err
	}
	if size == 0 {
		return "", nil
	}
	if size > uint64(src.Len()) {
		return "", errors.New("buffer is not enough")
	}
	return string(src.Next(int(size))), nil
}
