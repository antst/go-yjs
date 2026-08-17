package crdt

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Order-preserving JSON for the JSON.stringify / JSON.parse paths.
//
// Yjs serializes ContentJson values and (under V1) ContentFormat attribute
// values, plus awareness states, with JSON.stringify, which emits object keys in
// JS INSERTION order. Go's encoding/json marshals map keys SORTED, so a multi-key
// object would diverge from the JS byte stream. These helpers encode/decode JSON
// while threading insertion order through the ordered Object type:
//
//   - marshalJSONOrdered emits objects (our Object type) in key insertion order.
//     Scalars, arrays, and any non-Object value delegate to encoding/json so their
//     byte representation is unchanged from the previous json.Marshal path (only
//     object key ordering changes).
//   - unmarshalJSONOrdered parses a JSON document into ordered Objects (for JSON
//     objects), []any (arrays), and the same scalar Go types encoding/json would
//     produce, walking the token stream so on-wire key order is preserved.
//
// Together they make the JSON round-trip (decode a JS-produced payload, re-encode)
// byte-identical for multi-key objects.

// marshalJSONOrdered returns the JSON encoding of v, emitting Object keys in
// insertion order. It matches encoding/json byte-for-byte for everything except
// object key ordering (which it fixes to insertion order to match JSON.stringify).
func marshalJSONOrdered(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := marshalJSONValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func marshalJSONValue(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case Object:
		return marshalOrderedObject(buf, val)
	case *Object:
		if val == nil {
			buf.WriteString("null")
			return nil
		}
		return marshalOrderedObject(buf, *val)
	case NullType:
		// lib0 null and JSON null both serialize to the literal "null".
		buf.WriteString("null")
		return nil
	case UndefinedType:
		// JSON has no undefined; JSON.stringify(undefined) inside a value position
		// yields null. (At the top level JSON.stringify(undefined) is undefined, but
		// our content paths never marshal a bare undefined — ContentJson writes the
		// "undefined" keyword sentinel separately before reaching here.)
		buf.WriteString("null")
		return nil
	case string:
		// Strings must escape EXACTLY as JS JSON.stringify, not as Go encoding/json:
		// Go HTML-escapes < > & and always escapes U+2028/U+2029, both of which
		// diverge from the JS byte stream this codec reproduces. Use the JS-faithful
		// encoder.
		writeJSONStringJS(buf, val)
		return nil
	case []any:
		buf.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := marshalJSONValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		// A plain (unordered) Go map can still reach here via externally-constructed
		// values. Emit it with sorted keys exactly as
		// encoding/json would (delegated), so behavior is unchanged for the maps
		// that never carried order in the first place.
		data, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	default:
		// Scalars (string/number/bool/null), []byte, json.Number, etc.: delegate to
		// encoding/json so the bytes are identical to the previous json.Marshal path.
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	}
}

// jsHexDigits is the lowercase hex alphabet for \u00XX escapes (JSON.stringify
// emits lowercase hex, e.g. ).
const jsHexDigits = "0123456789abcdef"

// writeJSONStringJS writes s as a JSON string literal using the EXACT escaping of
// ECMAScript JSON.stringify (the QuoteJSONString algorithm), so the bytes match
// the JS stream this codec reproduces. It deliberately differs from Go
// encoding/json, which HTML-escapes < > & (when SetEscapeHTML is on) and ALWAYS
// escapes U+2028 / U+2029.
//
// Escaped: " -> \" , \ -> \\ , and every control char U+0000..U+001F, using the
// short forms \b \t \n \f \r where they exist and \u00XX otherwise. EVERYTHING
// else — including < > & / , DEL (0x7F), U+2028, U+2029 and all other non-ASCII —
// is emitted as its raw UTF-8 bytes, exactly as JSON.stringify does. The input is
// a Go string (already valid UTF-8 for our value domain), so multi-byte runes are
// copied through verbatim; we only need to special-case the ASCII control range
// and the two mandatory escapes, which are all single bytes < 0x80 and so never
// occur inside a multi-byte UTF-8 sequence.
func writeJSONStringJS(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	// Emit raw runs between escapes to avoid per-byte WriteByte for common text.
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			// Raw byte (covers all ASCII text incl. < > & /, and every byte >= 0x80,
			// i.e. all multi-byte UTF-8 continuation/lead bytes including U+2028/9).
			continue
		}
		if start < i {
			buf.WriteString(s[start:i])
		}
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			// Other control chars (0x00-0x1f without a short form): \u00XX, lowercase.
			buf.WriteString(`\u00`)
			buf.WriteByte(jsHexDigits[c>>4])
			buf.WriteByte(jsHexDigits[c&0xF])
		}
		start = i + 1
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
}

// appendJSONStringJS is the byte-slice counterpart of writeJSONStringJS. The full-state V1
// encoder owns a pre-sized output slice, so writing string format values into it directly avoids
// constructing a temporary bytes.Buffer for every ContentFormat item.
func appendJSONStringJS(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			dst = append(dst, '\\', 'u', '0', '0', jsHexDigits[c>>4], jsHexDigits[c&0xF])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func jsonStringJSEncodedLen(s string) uint64 {
	length := uint64(len(s) + 2)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 0x20 && c != '"' && c != '\\':
		case c == '"' || c == '\\' || c == '\b' || c == '\t' || c == '\n' || c == '\f' || c == '\r':
			length++ // one raw byte becomes a two-byte escape
		default:
			length += 5 // one raw byte becomes six bytes: \\u00XX
		}
	}
	return length
}

func marshalOrderedObject(buf *bytes.Buffer, o Object) error {
	buf.WriteByte('{')
	first := true
	var rangeErr error
	o.Range(func(key string, value any) {
		if rangeErr != nil {
			return
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		// Encode the key with JS JSON.stringify semantics (NOT Go encoding/json,
		// which HTML-escapes < > & and always escapes U+2028/U+2029), so special-
		// character keys are byte-identical to the JS object-key stream.
		writeJSONStringJS(buf, key)
		buf.WriteByte(':')
		if err := marshalJSONValue(buf, value); err != nil {
			rangeErr = err
		}
	})
	if rangeErr != nil {
		return rangeErr
	}
	buf.WriteByte('}')
	return nil
}

// unmarshalJSONOrdered parses one JSON value from data into order-preserving Go
// values: JSON objects become Object (keys in document order), JSON arrays become
// []any, and scalars become the same Go types encoding/json yields (string,
// float64, bool, nil). It rejects trailing garbage after the value.
func unmarshalJSONOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // decode numbers as json.Number first; normalized below.
	v, err := decodeJSONValue(dec, 0)
	if err != nil {
		return nil, err
	}
	// Ensure there is no trailing non-whitespace content (matches json.Unmarshal,
	// which errors on trailing data).
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON: trailing data after value")
	}
	// A JSON null (at any level) decodes to lib0 Null (NullType), so it re-encodes
	// as lib0 null (tag 126), not undefined (tag 127) — see decodeJSONFromToken.
	// Callers that need a bare Go nil for a cleared/absent value (awareness) map a
	// top-level Null to nil themselves (see jsonObject).
	return v, nil
}

// decodeJSONValue decodes one JSON value, carrying the current container nesting
// depth so deeply-nested input cannot blow the native stack (DoS 2). The
// streaming json.Decoder.Token() API does NOT enforce json.Unmarshal's nesting
// limit, so without this bound a ~2M-deep document (reachable via the public
// ApplyAwarenessUpdate / V1 ApplyUpdate ContentJson / ContentFormat) drives
// unbounded recursion to an unrecoverable `fatal error: stack overflow`. The cap
// is maxAnyDepth (100), matching the binary readAnyDepth/readObjectDepth bound.
func decodeJSONValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeJSONFromToken(dec, tok, depth)
}

func decodeJSONFromToken(dec *json.Decoder, tok json.Token, depth int) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		// DoS 2: entering an object/array is one nesting level. Bound it the same
		// way readArrayDepth/readObjectDepth bound the binary any-decoder. Use `>`
		// (not `>=`) so the JSON and binary paths admit the IDENTICAL maximum depth:
		// the binary decoders (decoding.go) check `depth > maxAnyDepth` at the same
		// structural point (entering a container at the current depth, recursing
		// children at depth+1), so `>=` here rejected one level shallower than the
		// binary path and the two disagreed on a max-depth value.
		if depth > maxAnyDepth {
			return nil, fmt.Errorf("invalid JSON: nesting depth exceeds %d", maxAnyDepth)
		}
		switch t {
		case '{':
			obj := newObject()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("invalid JSON object key: %v", keyTok)
				}
				val, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj.Set(key, val)
			}
			// consume closing '}'
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := make([]any, 0)
			for dec.More() {
				val, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			// consume closing ']'
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter: %v", t)
		}
	case json.Number:
		return normalizeJSONNumber(t)
	case nil:
		// JSON null. Decode to lib0 Null (NullType), NOT a bare Go nil: lib0's
		// any-decoder yields Null for a null (tag 126), and a bare Go nil would
		// re-encode through WriteAny as undefined (tag 127) because IsUndefined(nil)
		// is true — diverging from the JS byte stream. Returning Null keeps the
		// JSON.parse path consistent with the any path and byte-faithful on
		// re-encode.
		return Null, nil
	default:
		// string, bool
		return tok, nil
	}
}

// normalizeJSONNumber converts a json.Number to the Go scalar encoding/json would
// produce with default settings: float64. (We use UseNumber only to capture the
// raw token; the previous json.Unmarshal-into-any path yielded float64 for every
// number, so we reproduce that to keep downstream behavior identical.)
func normalizeJSONNumber(n json.Number) (any, error) {
	f, err := n.Float64()
	if err != nil {
		return nil, err
	}
	return f, nil
}
