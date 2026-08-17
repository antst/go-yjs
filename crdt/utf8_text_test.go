package crdt

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unsafe"
)

func TestNormalizeTextUTF8ExhaustiveOneAndTwoByteOracle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node unavailable: %v", err)
	}
	// One decoder call per input is intentional: TextDecoder state must be reset
	// between strings exactly as lib0's non-streaming VarString decode resets it.
	script := `
const d = new TextDecoder('utf-8', { fatal: false, ignoreBOM: true })
for (let a = 0; a < 256; a++) process.stdout.write(Buffer.from(d.decode(Uint8Array.of(a))).toString('hex') + '\n')
for (let a = 0; a < 256; a++) for (let b = 0; b < 256; b++) process.stdout.write(Buffer.from(d.decode(Uint8Array.of(a, b))).toString('hex') + '\n')
`
	out, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatalf("TextDecoder oracle: %v", err)
	}
	scan := bufio.NewScanner(bytes.NewReader(out))
	check := func(input []byte) {
		t.Helper()
		if !scan.Scan() {
			t.Fatalf("missing oracle output at %x", input)
		}
		want, err := hex.DecodeString(scan.Text())
		if err != nil {
			t.Fatal(err)
		}
		got := []byte(normalizeTextUTF8(string(input)))
		if !bytes.Equal(got, want) {
			t.Fatalf("normalizeTextUTF8(%x) = %x, native TextDecoder gives %x", input, got, want)
		}
	}
	for a := 0; a < 256; a++ {
		check([]byte{byte(a)})
	}
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			check([]byte{byte(a), byte(b)})
		}
	}
	if scan.Scan() {
		t.Fatal("extra TextDecoder oracle output")
	}
}

func TestNormalizeTextUTF8MatchesWHATWGReplacementBoundaries(t *testing.T) {
	tests := []struct {
		in   []byte
		want string
	}{
		{[]byte{0xff}, "�"},
		{[]byte{0xc3}, "�"},
		{[]byte{0xff, 0xff}, "��"},
		{[]byte{0xc3, 0xc3}, "��"},
		{[]byte{0xe0, 0x80, 0x80}, "���"},
		{[]byte{0xed, 0xa0, 0x80}, "���"},
		{[]byte{0xf0, 0x80, 0x80, 0x80}, "����"},
		{[]byte{0xf4, 0x90, 0x80, 0x80}, "����"},
		{[]byte{0x80, 0x80}, "��"},
		{[]byte{0xe2, 0x82}, "�"},
		{[]byte{0xe2, 0x28, 0xa1}, "�(�"},
		{[]byte{0xf0, 0x9f, 0x92}, "�"},
		{[]byte{0xc2, 0x41}, "�A"},
		{[]byte{0xe1, 0x80, 0x41}, "�A"},
	}
	for _, tc := range tests {
		in := string(tc.in)
		if got := normalizeTextUTF8(in); got != tc.want {
			t.Errorf("normalizeTextUTF8(%x) = %q (%x), want %q (%x)",
				tc.in, got, []byte(got), tc.want, []byte(tc.want))
		}
	}
}

func TestNormalizeTextUTF8ValidFastPathReturnsOriginalString(t *testing.T) {
	for _, input := range []string{"", "plain ASCII", "café 世界 😀"} {
		got := normalizeTextUTF8(input)
		if got != input {
			t.Fatalf("normalizeTextUTF8(%q) = %q", input, got)
		}
		if len(input) > 0 && unsafe.StringData(got) != unsafe.StringData(input) {
			t.Fatalf("valid input %q was copied", input)
		}
		if allocs := testing.AllocsPerRun(1000, func() { _ = normalizeTextUTF8(input) }); allocs != 0 {
			t.Fatalf("valid input %q allocated %.0f times", input, allocs)
		}
	}
}

func TestYTextNormalizesMalformedUTF8BeforeMutation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single-invalid", "\xff", "�"},
		{"truncated", "a\xe2\x82b", "a�b"},
		{"separate-invalid", "\xff\xff", "��"},
		{"invalid-continuation", "x\xe2(\xa1y", "x�(�y"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("source", true, defaultGCFilter, nil, false, WithClientID(1))
			text := doc.GetText("text")
			text.Insert(0, "prefix", Object{})
			// A second tail insertion exercises the sole-ContentString append fast path.
			text.Insert(text.Length(), tc.in, Object{})
			if got := text.ToString(); got != "prefix"+tc.want {
				t.Fatalf("live text = %q (%x), want %q", got, []byte(got), "prefix"+tc.want)
			}

			for _, wire := range []struct {
				name   string
				encode func(*Doc, []byte) ([]uint8, error)
				apply  func(*Doc, []uint8, any) error
			}{
				{"v1", EncodeStateAsUpdate, ApplyUpdate},
				{"v2", EncodeStateAsUpdateV2, ApplyUpdateV2},
			} {
				update, err := wire.encode(doc, nil)
				if err != nil {
					t.Fatalf("%s encode: %v", wire.name, err)
				}
				peer := newDoc("peer", true, defaultGCFilter, nil, false)
				if err := wire.apply(peer, update, nil); err != nil {
					t.Fatalf("%s apply: %v", wire.name, err)
				}
				if got := peer.GetText("text").ToString(); got != "prefix"+tc.want {
					t.Fatalf("%s peer text = %q (%x), want %q", wire.name, got, []byte(got), "prefix"+tc.want)
				}
			}
		})
	}
}

func TestApplyDeltaNormalizesBothStringInsertArms(t *testing.T) {
	doc := newDoc("delta", true, defaultGCFilter, nil, false)
	text := doc.GetText("text")
	text.ApplyDelta([]EventOperator{
		NewTextDeltaOp("a\xe2\x82", Object{}),
		NewValueDeltaOp("\xffb", Object{}),
	}, true)
	if got, want := text.ToString(), "a��b"; got != want {
		t.Fatalf("ApplyDelta text = %q (%x), want %q", got, []byte(got), want)
	}
}

func TestDecodedContentStringNormalizesMalformedUTF8(t *testing.T) {
	for _, raw := range []string{"\xff", "\xe2\x82", "a\xffb"} {
		enc := newUpdateEncoderV1()
		if err := enc.writeStringValue(raw); err != nil {
			t.Fatal(err)
		}
		content, err := readContentString(newUpdateDecoderV1(enc.toBytes()))
		if err != nil {
			t.Fatal(err)
		}
		content.contentLength() // Integration establishes the valid-text invariant.
		got := content.(*contentString).value
		if want := normalizeTextUTF8(raw); got != want {
			t.Fatalf("decoded %x = %q (%x), want %q", []byte(raw), got, []byte(got), want)
		}
	}
}

func TestV2StringColumnNormalizesMalformedUTF8(t *testing.T) {
	enc := newDefaultStringEncoder()
	enc.writeValue("\xff")
	dec := newStringDecoder(enc.bytes())
	got, err := dec.readValue()
	if err != nil {
		t.Fatal(err)
	}
	if got != "�" {
		t.Fatalf("decoded V2 string = %q (%x), want replacement", got, []byte(got))
	}
}

func yjsDecodeTextUpdate(t *testing.T, update []uint8, v2 bool) string {
	t.Helper()
	yjsPath := repoPath(t, "fuzz", "node_modules", "yjs")
	if _, err := os.Stat(yjsPath); err != nil {
		t.Skipf("yjs dependency unavailable: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node unavailable: %v", err)
	}
	script := `
const Y = require('./fuzz/node_modules/yjs')
const doc = new Y.Doc()
const update = Uint8Array.from(Buffer.from(process.argv[1], 'base64'))
if (process.argv[2] === 'v2') Y.applyUpdateV2(doc, update)
else Y.applyUpdate(doc, update)
process.stdout.write(doc.getText('text').toString())
`
	mode := "v1"
	if v2 {
		mode = "v2"
	}
	cmd := exec.Command("node", "-e", script, base64.StdEncoding.EncodeToString(update), mode)
	// The script requires ./fuzz/node_modules/yjs, so node must run from the
	// repository root rather than from this package's directory.
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("yjs rejected %s update: %v", mode, err)
	}
	return string(out)
}

func TestMalformedTextAlwaysEmitsYjsDecodableUpdates(t *testing.T) {
	inputs := []string{"\xff", "\xff\xff", "a\xe2\x82b", "x\xe2(\xa1y"}
	for _, input := range inputs {
		t.Run(base64.RawStdEncoding.EncodeToString([]byte(input)), func(t *testing.T) {
			doc := newDoc("source", true, defaultGCFilter, nil, false, WithClientID(1))
			doc.GetText("text").Insert(0, input, Object{})
			want := normalizeTextUTF8(input)
			v1, err := EncodeStateAsUpdate(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			v2, err := EncodeStateAsUpdateV2(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := yjsDecodeTextUpdate(t, v1, false); got != want {
				t.Fatalf("yjs V1 text = %q, want %q", got, want)
			}
			if got := yjsDecodeTextUpdate(t, v2, true); got != want {
				t.Fatalf("yjs V2 text = %q, want %q", got, want)
			}
		})
	}
}

func TestMalformedDecodedTextCannotPoisonReencodedUpdate(t *testing.T) {
	for _, wire := range []struct {
		name   string
		encode func(*Doc, []byte) ([]uint8, error)
		apply  func(*Doc, []uint8, any) error
		v2     bool
	}{
		{"v1", EncodeStateAsUpdate, ApplyUpdate, false},
		{"v2", EncodeStateAsUpdateV2, ApplyUpdateV2, true},
	} {
		t.Run(wire.name, func(t *testing.T) {
			// Deliberately bypass the public insertion boundary to model a malformed
			// update emitted by a non-conforming implementation. Two invalid bytes
			// preserve the source item's two UTF-16 units in both wire formats.
			hostile := newDoc("hostile", true, defaultGCFilter, nil, false, WithClientID(1))
			text := hostile.GetText("text")
			text.Insert(0, "xx", Object{})
			text.start.content.(*contentString).value = "\xff\xff"
			bad, err := wire.encode(hostile, nil)
			if err != nil {
				t.Fatal(err)
			}

			receiver := newDoc("receiver", true, defaultGCFilter, nil, false, WithClientID(2))
			if err := wire.apply(receiver, bad, nil); err != nil {
				t.Fatalf("apply malformed source update: %v", err)
			}
			if got := receiver.GetText("text").ToString(); got != "��" {
				t.Fatalf("normalized received text = %q (%x)", got, []byte(got))
			}
			clean, err := wire.encode(receiver, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := yjsDecodeTextUpdate(t, clean, wire.v2); got != "��" {
				t.Fatalf("yjs re-decoded text = %q, want replacements", got)
			}
		})
	}
}

func TestNormalizeTextUTF8DoesNotChangeValidCorpus(t *testing.T) {
	valid := strings.Repeat("ASCII ", 100) + "café 世界 😀\x00\u2028\u2029"
	if got := normalizeTextUTF8(valid); got != valid {
		t.Fatalf("valid corpus changed: %q", got)
	}
}
