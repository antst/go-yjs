package crdt

import (
	"bytes"
	"fmt"
	"unicode/utf16"
)

// string_codec.go ports lib0's StringEncoder / StringDecoder used by Yjs
// UpdateEncoderV2 for the string column.
//
// All written strings are concatenated and emitted once as a single VarString;
// the per-string lengths are tracked in a UintOptRleEncoder and appended. The
// lengths are UTF-16 code-unit counts (JS String#length), so the decoder slices
// the concatenated string by UTF-16 code units — not bytes or runes.

// stringEncoder is the optimized string-column encoder.
type stringEncoder struct {
	text []byte
	lens *uintOptRLEEncoder
}

const stringEncoderPrefixHeadroom = 10

// newDefaultStringEncoder creates an empty StringEncoder.
func newDefaultStringEncoder() *stringEncoder {
	return &stringEncoder{
		text: make([]byte, stringEncoderPrefixHeadroom),
		lens: newDefaultUintOptRLEEncoder(),
	}
}

func newStringEncoder(textCapacity, lensCapacity int) *stringEncoder {
	return &stringEncoder{
		text: make([]byte, stringEncoderPrefixHeadroom, stringEncoderPrefixHeadroom+textCapacity),
		lens: newUintOptRleEncoder(lensCapacity),
	}
}

// writeValue appends a string to the column.
func (e *stringEncoder) writeValue(s string) {
	e.writeWithLength(s, stringLength(s))
}

func (e *stringEncoder) writeWithLength(s string, length Number) {
	e.text = append(e.text, s...)
	e.lens.writeValue(uint64(length))
}

// bytes returns the concatenated VarString followed by the length data.
func (e *stringEncoder) bytes() []uint8 {
	lens := e.lens.bytes()
	// The text buffer keeps enough unused bytes at its front for the VarString
	// prefix. Fill that headroom backwards and append the length column in-place,
	// avoiding a second text-sized allocation and copy at finalization.
	textLength := len(e.text) - stringEncoderPrefixHeadroom
	prefixLength := varUintEncodedLen(uint64(textLength))
	start := stringEncoderPrefixHeadroom - prefixLength
	out := appendVarUint(e.text[start:start], uint64(textLength))
	out = append(out, e.text[stringEncoderPrefixHeadroom:]...)
	out = append(out, lens...)
	return out
}

// stringDecoder is the optimized string-column decoder.
type stringDecoder struct {
	units     []uint16 // non-ASCII concatenated string as UTF-16 code units
	ascii     string   // ASCII concatenation; immutable slices are returned directly
	asciiMode bool
	spos      int
	lens      *uintOptRLEDecoder
	initErr   error // deferred error from constructing the column
}

// newStringDecoder creates a decoder over data. A malformed concatenated-string
// header is recorded and surfaced on the first Read rather than silently
// yielding an empty/wrong string.
func newStringDecoder(data []uint8) *stringDecoder {
	d := new(stringDecoder)
	lens := new(uintOptRLEDecoder)
	lensBuffer := new(bytes.Buffer)
	initStringDecoder(d, lens, lensBuffer, data)
	return d
}

func initStringDecoder(d *stringDecoder, lens *uintOptRLEDecoder, lensBuffer *bytes.Buffer, data []uint8) {
	// ReadString advances lensBuffer past the concatenated text. Its remaining
	// bytes are exactly the length stream, so the same buffer can back lens; a
	// second bytes.Buffer wrapper would only duplicate its slice header and cursor.
	*lensBuffer = *bytes.NewBuffer(data)
	str, err := readString(lensBuffer)
	*lens = uintOptRLEDecoder{buf: lensBuffer}
	*d = stringDecoder{lens: lens, initErr: err}
	d.asciiMode = true
	for i := 0; i < len(str); i++ {
		if str[i] >= 0x80 {
			d.asciiMode = false
			break
		}
	}
	if d.asciiMode {
		d.ascii = str
	} else {
		str = normalizeTextUTF8(str)
		d.units = utf16.Encode([]rune(str))
	}
}

// remaining reports the undecoded extent of the string column: the bytes left in
// the length sub-decoder plus the UTF-16 code units not yet sliced out. It lets
// callers (UpdateDecoderV2.RemainingLen) measure progress without reaching into
// this decoder's unexported fields.
func (d *stringDecoder) remaining() int {
	textRemaining := len(d.units) - d.spos
	if d.asciiMode {
		textRemaining = len(d.ascii) - d.spos
	}
	return d.lens.remaining() + textRemaining
}

// readValue returns the next string, slicing the concatenation by UTF-16 length. It
// errors (rather than truncating) when a length would run past the concatenated
// string — a malformed/hostile column — so callers can abort instead of
// fabricating a short string.
func (d *stringDecoder) readValue() (string, error) {
	if d.initErr != nil {
		return "", d.initErr
	}
	n, err := d.lens.readValue()
	if err != nil {
		return "", err
	}
	// Validate the run length using full uint64 math BEFORE narrowing to int. On a
	// 32-bit int an oversized n (e.g. 2^32 + small) would truncate to a small
	// positive value and slip past a post-cast bounds check, then slice out of
	// range. remaining is the number of UTF-16 code units left in the column.
	textLength := len(d.units)
	if d.asciiMode {
		textLength = len(d.ascii)
	}
	remaining := uint64(textLength - d.spos)
	if n > remaining {
		return "", fmt.Errorf("string column overrun: pos %d + len %d > %d", d.spos, n, textLength)
	}
	end := d.spos + int(n)
	if d.asciiMode {
		res := d.ascii[d.spos:end]
		d.spos = end
		return res, nil
	}
	res := string(utf16.Decode(d.units[d.spos:end]))
	d.spos = end
	return res, nil
}
