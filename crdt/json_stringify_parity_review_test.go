package crdt

import (
	"encoding/hex"
	"testing"
)

// json_stringify_parity_review_test.go reproduces FINDING 2 from the full
// code-review of the Go Yjs v2 codec (PR antst/y-crdt#2): marshalJSONOrdered
// delegated string escaping to Go encoding/json, which (a) HTML-escapes < > &
// (-> < > &) and (b) ALWAYS escapes U+2028 / U+2029 even with
// SetEscapeHTML(false) — both DIFFER from JS JSON.stringify, the byte stream this
// codec must reproduce. JSON.stringify emits < > & LITERALLY and U+2028 / U+2029
// RAW (as their UTF-8 bytes), escaping only ", \, and control chars < 0x20 (with
// the short forms \b \t \n \f \r). So Go != JS for any ContentJson value, V1
// embed / format-attribute JSON, or awareness state containing those characters.
//
// The expected bytes below were captured from real node JSON.stringify (see the
// probe in the commit message); they pin marshalJSONOrdered to byte-identity with
// JSON.stringify. This test FAILS on the unpatched tree (Go escapes < > & and
// U+2028/9) and PASSES once marshalJSONOrdered uses a JS-faithful string encoder.

func hexOf(t *testing.T, h string) string {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad hex %q: %v", h, err)
	}
	return string(b)
}

// TestMarshalJSONOrderedStringMatchesJSONStringify pins string escaping to JS.
func TestMarshalJSONOrderedStringMatchesJSONStringify(t *testing.T) {
	// (input string, expected JSON.stringify output as hex).
	cases := []struct {
		name    string
		in      string
		wantHex string
	}{
		// HTML chars: LITERAL in JSON.stringify (Go default escapes them).
		{"less-than", "a<b", "22613c6222"},
		{"greater-than", "a>b", "22613e6222"},
		{"ampersand", "a&b", "2261266222"},
		{"all three <>&", "<>&", "223c3e2622"},
		// Forward slash: literal.
		{"forward slash", "a/b", "22612f6222"},
		// Quote and backslash: escaped.
		{"double quote", "a\"b", "22615c226222"},
		{"backslash", "a\\b", "22615c5c6222"},
		// Control chars: short forms + \u00XX.
		{"newline", "a\nb", "22615c6e6222"},
		{"tab", "a\tb", "22615c746222"},
		{"backspace", "a\bb", "22615c626222"},
		{"formfeed", "a\fb", "22615c666222"},
		{"carriage return", "a\rb", "22615c726222"},
		{"nul", "a\x00b", "22615c75303030306222"},
		{"unit separator 0x1f", "a\x1fb", "22615c75303031666222"},
		{"vertical tab 0x0b", "a\x0bb", "22615c75303030626222"},
		// DEL 0x7f: literal (NOT escaped).
		{"del 0x7f", "a\x7fb", "22617f6222"},
		// U+2028 / U+2029: RAW UTF-8, NOT escaped (Go always escapes these).
		{"U+2028", "x y", "2278e280a87922"},
		{"U+2029", "x y", "2278e280a97922"},
		// Non-ASCII: raw UTF-8.
		{"e-acute", "café", "22636166c3a922"},
		{"emoji", "a\U0001F600b", "2261f09f98806222"},
		// Combined.
		{"combined <a>&\" + U+2028", "<a>&\" ", "223c613e265c22e280a822"},
	}

	for _, c := range cases {
		got, err := marshalJSONOrdered(c.in)
		if err != nil {
			t.Errorf("%s: marshalJSONOrdered error: %v", c.name, err)
			continue
		}
		want := hexOf(t, c.wantHex)
		if string(got) != want {
			t.Errorf("%s: marshalJSONOrdered(%q)\n  got  %x\n  want %x (JSON.stringify)", c.name, c.in, got, want)
		}
	}
}

// Object KEYS must also escape with JS semantics (the key path delegated to
// json.Marshal too). A key containing < and U+2028 must be byte-identical to
// JSON.stringify of {"< >": 1} ... the key portion.
func TestMarshalJSONOrderedObjectKeyMatchesJSONStringify(t *testing.T) {
	o := MakeObject("a<b", 1)
	got, err := marshalJSONOrdered(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// JSON.stringify({"a<b":1}) === '{"a<b":1}' (the < is literal).
	want := `{"a<b":1}`
	if string(got) != want {
		t.Fatalf("object key escaping diverged\n  got  %s\n  want %s", got, want)
	}

	// Key with U+2028 must be raw.
	o2 := MakeObject("k ", true)
	got2, err := marshalJSONOrdered(o2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want2 := hexOf(t, "7b22"+"6b"+"e280a8"+"223a"+"74727565"+"7d") // {"k<U+2028>":true}
	if string(got2) != want2 {
		t.Fatalf("object key U+2028 escaping diverged\n  got  %x\n  want %x", got2, want2)
	}
}

// End-to-end through ContentJson (the V1 JSON content path): a value carrying <>&
// and U+2028 must re-encode byte-identically. We assert via the JSON encoder the
// content path uses (marshalJSONOrdered), since that is exactly what
// ContentJson.Write / WriteJson (V1) emit on the wire.
func TestContentJSONSpecialCharsByteFaithful(t *testing.T) {
	val := MakeObject("html", "<tag>&amp;", "sep", "a b")
	got, err := marshalJSONOrdered(val)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// JSON.stringify({"html":"<tag>&amp;","sep":"a b"}) with literal <>& and raw U+2028.
	want := "{\"html\":\"<tag>&amp;\",\"sep\":\"a b\"}"
	if string(got) != want {
		t.Fatalf("ContentJson special-char bytes diverged\n  got  %q\n  want %q", string(got), want)
	}
}
