package crdt

import (
	"unicode/utf8"
)

// normalizeTextUTF8 maps an arbitrary Go byte string to the scalar-value string
// that the WHATWG UTF-8 decoder produces in replacement mode. Go strings may
// contain invalid UTF-8 while JavaScript strings cannot; normalizing at the text
// boundary keeps the local document equal to peers after encoding. Yjs's fatal
// UTF-8 decoder rejects an update containing malformed bytes; normalizing before
// integration therefore restores wire compatibility for Go's broader string
// input space rather than letting one bad string poison the document history.
//
// Valid UTF-8 — every ordinary call — returns the original string without an
// allocation. The state machine below follows Encoding Standard §8.1.1 rather
// than strings.ToValidUTF8 or a Go range: both choose different replacement
// boundaries for malformed sequences.
func normalizeTextUTF8WithLength(s string) (string, Number) {
	if isASCIIText(s) {
		return s, Number(len(s))
	}
	return normalizeNonASCIITextUTF8WithLength(s)
}

func isASCIIText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func normalizeTextUTF8(s string) string {
	normalized, _ := normalizeTextUTF8WithLength(s)
	return normalized
}

func normalizeNonASCIITextUTF8WithLength(s string) (string, Number) {
	units := Number(0)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			normalized := replaceMalformedUTF8(s)
			return normalized, stringLength(normalized)
		}
		units++
		if r >= 0x10000 {
			units++
		}
		i += size
	}
	return s, units
}

func replaceMalformedUTF8(s string) string {
	out := make([]byte, 0, len(s))
	var codePoint rune
	bytesNeeded := 0
	bytesSeen := 0
	lower, upper := byte(0x80), byte(0xbf)

	for i := 0; i < len(s); {
		b := s[i]
		if bytesNeeded == 0 {
			switch {
			case b <= 0x7f:
				out = append(out, b)
				i++
				continue
			case b >= 0xc2 && b <= 0xdf:
				bytesNeeded = 1
				codePoint = rune(b & 0x1f)
			case b >= 0xe0 && b <= 0xef:
				if b == 0xe0 {
					lower = 0xa0
				}
				if b == 0xed {
					upper = 0x9f
				}
				bytesNeeded = 2
				codePoint = rune(b & 0x0f)
			case b >= 0xf0 && b <= 0xf4:
				if b == 0xf0 {
					lower = 0x90
				}
				if b == 0xf4 {
					upper = 0x8f
				}
				bytesNeeded = 3
				codePoint = rune(b & 0x07)
			default:
				out = utf8.AppendRune(out, utf8.RuneError)
				i++
				continue
			}
			i++
			continue
		}

		if b < lower || b > upper {
			out = utf8.AppendRune(out, utf8.RuneError)
			codePoint = 0
			bytesNeeded = 0
			bytesSeen = 0
			lower, upper = 0x80, 0xbf
			// The offending byte is restored to the input queue by the WHATWG
			// algorithm, so process it again as a possible leading byte.
			continue
		}

		lower, upper = 0x80, 0xbf
		codePoint = codePoint<<6 | rune(b&0x3f)
		bytesSeen++
		i++
		if bytesSeen == bytesNeeded {
			out = utf8.AppendRune(out, codePoint)
			codePoint = 0
			bytesNeeded = 0
			bytesSeen = 0
		}
	}

	if bytesNeeded != 0 {
		out = utf8.AppendRune(out, utf8.RuneError)
	}
	return string(out)
}
