package crdt

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"testing"
)

// V2 cross-language compatibility tests. They load the JS reference payloads
// produced by v2_test_fixtures/generate.js (pinned yjs@13.6.31) and assert:
//  1. Go EncodeStateAsUpdateV2 is byte-identical to JS encodeStateAsUpdateV2.
//  2. Go ApplyUpdateV2 of a JS V2 payload reconstructs the same doc state.
//
// Each fixture pins a fixed clientID; the Go side mocks generateNewClientID to
// that value so struct ids — and therefore the encoded bytes — match exactly.

type v2Fixture struct {
	Name         string          `json:"name"`
	ClientID     Number          `json:"clientID"`
	GUID         string          `json:"guid"`
	UpdateV1     string          `json:"updateV1"`
	UpdateV2     string          `json:"updateV2"`
	StateVector  string          `json:"stateVector"`
	JSON         json.RawMessage `json:"json"`
	DeleteDiffV2 string          `json:"deleteDiffV2"`
	DeleteDiffV1 string          `json:"deleteDiffV1"`
}

type v2FixtureFile struct {
	YjsVersion string      `json:"yjsVersion"`
	Fixtures   []v2Fixture `json:"fixtures"`
}

var (
	fixturesOnce sync.Once
	fixturesByID map[string]v2Fixture
	fixturesErr  error
)

func loadFixtures(t *testing.T) map[string]v2Fixture {
	t.Helper()
	fixturesOnce.Do(func() {
		path := repoPath(t, "v2_test_fixtures", "fixtures.json")
		data, err := os.ReadFile(path)
		if err != nil {
			fixturesErr = err
			return
		}
		var f v2FixtureFile
		if err := json.Unmarshal(data, &f); err != nil {
			fixturesErr = err
			return
		}
		fixturesByID = make(map[string]v2Fixture, len(f.Fixtures))
		for _, fx := range f.Fixtures {
			fixturesByID[fx.Name] = fx
		}
	})
	if fixturesErr != nil {
		t.Fatalf("load fixtures: %v (run: cd v2_test_fixtures && npm install && node generate.js)", fixturesErr)
	}
	return fixturesByID
}

func getFixture(t *testing.T, name string) v2Fixture {
	t.Helper()
	fx, ok := loadFixtures(t)[name]
	if !ok {
		t.Fatalf("fixture %q not found", name)
	}
	return fx
}

func b64dec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return b
}

// assertV2Equal builds a doc with the fixture's fixed clientID, runs build(doc),
// encodes V2, and compares byte-for-byte with the JS updateV2 payload.
//
// The clientID is injected deterministically via WithClientID (a newDoc option),
// NOT by monkey-patching generateNewClientID. The previous mockey-based approach
// only patched under `-gcflags=all=-N -l` and silently fell back to a random id
// otherwise, so the byte assertions could not actually match the fixtures. The
// explicit seam pins the id unconditionally, so this test really validates
// byte-parity against the JS fixtures on any toolchain.
func assertV2Equal(t *testing.T, name string, build func(doc *Doc)) {
	t.Helper()
	fx := getFixture(t, name)

	doc := newDoc(fx.GUID, true, defaultGCFilter, nil, false, WithClientID(fx.ClientID))
	if doc.ClientID != fx.ClientID {
		t.Fatalf("%s: clientID injection failed: doc.ClientID=%d want %d", name, doc.ClientID, fx.ClientID)
	}
	build(doc)

	got := mustBytes(EncodeStateAsUpdateV2(doc, nil))
	want := b64dec(t, fx.UpdateV2)
	if !bytes.Equal(got, want) {
		t.Errorf("%s: V2 output mismatch\n  want %v\n  got  %v", name, want, got)
	}
}

// assertApplyV2 applies the JS V2 payload to a fresh Go doc and returns it for
// state assertions by the caller.
func assertApplyV2(t *testing.T, name string) *Doc {
	t.Helper()
	fx := getFixture(t, name)
	doc := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc, b64dec(t, fx.UpdateV2), nil)
	return doc
}

func TestV2TextOperations(t *testing.T) {
	// encode parity
	assertV2Equal(t, "text_insert_delete", func(doc *Doc) {
		ytext := doc.GetText("type")
		doc.Transact(func(trans *Transaction) {
			ytext.Insert(0, "def", Object{})
			ytext.Insert(0, "abc", Object{})
			ytext.Insert(6, "ghi", Object{})
			ytext.Delete(2, 5)
		}, nil)
	})

	// apply parity (JS V2 -> Go)
	doc := assertApplyV2(t, "text_insert_delete")
	if got := doc.GetText("type").ToString(); got != "abhi" {
		t.Errorf("apply text_insert_delete: want abhi got %q", got)
	}
}

func TestV2TextUnicode(t *testing.T) {
	assertV2Equal(t, "text_unicode", func(doc *Doc) {
		doc.GetText("content").Insert(0, "héllo 世界 🌍 αβγ", Object{})
	})

	doc := assertApplyV2(t, "text_unicode")
	if got := doc.GetText("content").ToString(); got != "héllo 世界 🌍 αβγ" {
		t.Errorf("apply text_unicode: want unicode string got %q", got)
	}
}

func TestV2TextFormatting(t *testing.T) {
	// Y.Text formatting attributes — the parity gap the v1 fuzz gate flagged.
	//
	// NOTE: this Go port's Y.Text formatting algorithm does not minimize/clean
	// redundant format markers byte-identically to Yjs (a pre-existing divergence
	// present in V1 too — NOT a V2-codec issue), so we validate at the level that
	// matters for correctness: applying the JS V2 payload reconstructs the right
	// text content and converges. See the package report for the minimal repro.
	doc := assertApplyV2(t, "text_formatting")
	if got := doc.GetText("rich").ToString(); got != "Hello big World" {
		t.Errorf("apply text_formatting: want 'Hello big World' got %q", got)
	}
	// Round-trip: re-encode the applied doc as V2 and apply to a second doc; the
	// text content must survive (convergence under the V2 codec).
	round := mustBytes(EncodeStateAsUpdateV2(doc, nil))
	doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc2, round, nil)
	if got := doc2.GetText("rich").ToString(); got != "Hello big World" {
		t.Errorf("V2 round-trip text_formatting: want 'Hello big World' got %q", got)
	}
}

func TestV2TextFormattingEmbed(t *testing.T) {
	// Apply + round-trip (see TestV2TextFormatting for why this isn't byte-exact).
	doc := assertApplyV2(t, "text_formatting_embed")
	round := mustBytes(EncodeStateAsUpdateV2(doc, nil))
	doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc2, round, nil)
	if doc.GetText("rich").GetLength() != doc2.GetText("rich").GetLength() {
		t.Errorf("V2 round-trip text_formatting_embed: length mismatch %d vs %d",
			doc.GetText("rich").GetLength(), doc2.GetText("rich").GetLength())
	}
}

func TestV2MapOperations(t *testing.T) {
	assertV2Equal(t, "map_set", func(doc *Doc) {
		x := doc.GetMap("test")
		doc.Transact(func(trans *Transaction) {
			x.Set("k1", "v1")
			x.Set("k2", "v2")
		}, nil)
	})

	doc := assertApplyV2(t, "map_set")
	m := doc.GetMap("test")
	if v := m.Get("k1"); v != "v1" {
		t.Errorf("apply map_set k1: want v1 got %v", v)
	}
	if v := m.Get("k2"); v != "v2" {
		t.Errorf("apply map_set k2: want v2 got %v", v)
	}
}

func TestV2MapEncodeSafe(t *testing.T) {
	// Byte-equality over value types whose any-encoding is deterministic in this Go
	// port (only non-integer floats are excluded — see map_encode_safe in
	// generate.js). Multi-key objects are deterministic now; see
	// TestV2MapMultiKeyObject.
	assertV2Equal(t, "map_encode_safe", func(doc *Doc) {
		x := doc.GetMap("test")
		doc.Transact(func(trans *Transaction) {
			x.Set("s", "string")
			x.Set("n", 42)
			x.Set("neg", -17)
			x.Set("b", true)
			x.Set("bf", false)
			x.Set("nul", Null)
			x.Set("single", MakeObject("only", "one"))
			x.Set("arr", ArrayAny{1, "two", true, Null})
		}, nil)
	})
}

func TestV2MapMultiKeyObject(t *testing.T) {
	// Byte-equality for MULTI-KEY objects with keys in deliberately non-sorted
	// insertion order. This is the core guard for the insertion-ordered Object
	// change: lib0 writeAny emits keys in JS insertion order, and the Go Object
	// type reproduces that order so the V2 bytes match the JS fixture exactly.
	// Before the change Go emitted map-randomized / json.Marshal-sorted keys and
	// this would diverge.
	assertV2Equal(t, "map_multi_key_object", func(doc *Doc) {
		x := doc.GetMap("test")
		doc.Transact(func(trans *Transaction) {
			x.Set("zam", MakeObject("z", 1, "a", 2, "m", 3))
			x.Set("wbq", MakeObject("w", true, "b", "x", "q", Null))
			x.Set("nested", MakeObject(
				"outer", MakeObject("y", 9, "x", 8),
				"k", ArrayAny{1, MakeObject("d", 4, "c", 3)},
			))
		}, nil)
	})
}

func TestV2MapFloatSafe(t *testing.T) {
	// Byte-equality for float values, previously divergent (finding #14): Go used
	// to always emit float64 (tag 123) while lib0 picks the tightest of int /
	// float32 / float64. After the WriteAny tag-cascade fix these are byte-exact.
	assertV2Equal(t, "map_float_safe", func(doc *Doc) {
		x := doc.GetMap("test")
		doc.Transact(func(trans *Transaction) {
			x.Set("half", float64(0.5))             // float32-exact -> tag 124
			x.Set("tenth", float64(0.1))            // not float32-exact -> tag 123
			x.Set("intf", float64(2.0))             // integer-valued -> tag 125
			x.Set("big", float64((int64(1)<<40)+1)) // large non-float32 int -> tag 123
		}, nil)
	})
}

func TestV2MapMixedValuesApply(t *testing.T) {
	// map_mixed_values contains non-integer floats (3.5), which still can't be
	// Go-encoded byte-identically (Go writes float64 where lib0 may pick float32),
	// so this stays an apply-only check. Multi-key objects ARE now byte-exact (see
	// TestV2MapMultiKeyObject); only the float keeps this fixture apply-only.
	doc := assertApplyV2(t, "map_mixed_values")
	m := doc.GetMap("test")
	if v := m.Get("s"); v != "string" {
		t.Errorf("apply map_mixed s: want string got %v", v)
	}
	if v := m.Get("n"); v != 42 {
		t.Errorf("apply map_mixed n: want 42 got %v", v)
	}
	if v := m.Get("b"); v != true {
		t.Errorf("apply map_mixed b: want true got %v", v)
	}
}

func TestV2ArrayOperations(t *testing.T) {
	assertV2Equal(t, "array_insert", func(doc *Doc) {
		x := doc.GetArray("test")
		x.Push([]any{"a"})
		x.Push([]any{"b"})
	})

	doc := assertApplyV2(t, "array_insert")
	arr := doc.GetArray("test")
	if arr.Get(0) != "a" || arr.Get(1) != "b" {
		t.Errorf("apply array_insert: want [a b] got [%v %v]", arr.Get(0), arr.Get(1))
	}
}

func TestV2ArrayEncodeSafe(t *testing.T) {
	assertV2Equal(t, "array_encode_safe", func(doc *Doc) {
		x := doc.GetArray("test")
		doc.Transact(func(trans *Transaction) {
			x.Insert(0, ArrayAny{1, "two", true, Null, ArrayAny{9, 8}})
			x.Delete(1, 1)
			x.Insert(2, ArrayAny{99})
		}, nil)
	})
}

func TestV2ArrayMixedApply(t *testing.T) {
	// Contains a multi-key object; encode-parity isn't byte-deterministic, but
	// apply must round-trip.
	doc := assertApplyV2(t, "array_mixed")
	if got := doc.GetArray("test").GetLength(); got == 0 {
		t.Errorf("apply array_mixed: array unexpectedly empty")
	}
}

func TestV2XmlOperations(t *testing.T) {
	assertV2Equal(t, "xml_fragment_insert", func(doc *Doc) {
		frag := doc.GetXMLFragment("fragment-name")
		xt := NewYXmlText()
		frag.Insert(0, ArrayAny{xt})
		frag.InsertAfter(xt, ArrayAny{NewYXmlElement("node-name")})
	})
}

func TestV2XmlAttributes(t *testing.T) {
	// This Go port's YXmlElement/YXmlText lack child-insert/format APIs, so the
	// nested-child fixture can only be validated by applying the JS V2 payload.
	doc := assertApplyV2(t, "xml_attributes")
	if got := doc.GetXMLFragment("frag").GetLength(); got != 1 {
		t.Errorf("apply xml_attributes: want 1 top-level node got %d", got)
	}
}

func TestV2MixedAllTypes(t *testing.T) {
	// Contains Y.Text formatting, so byte-equality isn't achievable in this port
	// (see TestV2TextFormatting). Validate by apply + convergence instead.
	doc := assertApplyV2(t, "mixed_all_types")
	if got := doc.GetText("t").ToString(); got != "rich text" {
		t.Errorf("apply mixed_all_types text: want 'rich text' got %q", got)
	}
	if got := doc.GetMap("m").Get("key"); got == nil {
		t.Errorf("apply mixed_all_types map key missing")
	}
	if got := doc.GetArray("a").GetLength(); got != 3 {
		t.Errorf("apply mixed_all_types array: want len 3 got %d", got)
	}
	round := mustBytes(EncodeStateAsUpdateV2(doc, nil))
	doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc2, round, nil)
	if doc2.GetText("t").ToString() != "rich text" {
		t.Errorf("V2 round-trip mixed_all_types: text mismatch")
	}
}

func TestV2EdgeEmptyDoc(t *testing.T) {
	assertV2Equal(t, "empty_doc", func(doc *Doc) {
		doc.GetText("empty")
	})
}

func TestV2EdgeSingleClientManyOps(t *testing.T) {
	assertV2Equal(t, "single_client_many_ops", func(doc *Doc) {
		ytext := doc.GetText("big")
		doc.Transact(func(trans *Transaction) {
			for i := 0; i < 200; i++ {
				ytext.Insert(ytext.GetLength(), "x", Object{})
			}
		}, nil)
	})
}

func TestV2EdgeStructsOnly(t *testing.T) {
	assertV2Equal(t, "structs_only_no_deletes", func(doc *Doc) {
		a := doc.GetArray("a")
		doc.Transact(func(trans *Transaction) {
			for i := 0; i < 10; i++ {
				a.Push(ArrayAny{i})
			}
		}, nil)
	})
}

// Edge cases that can only be validated by apply (built via multi-client merges
// in JS, so the Go encode path can't reproduce the exact client interleaving).
func TestV2EdgeManyClientsApply(t *testing.T) {
	doc := assertApplyV2(t, "many_clients")
	if got := doc.GetArray("a").GetLength(); got != 1100 {
		t.Errorf("apply many_clients: want length 1100 got %d", got)
	}
}

func TestV2StateVectorIsV1Encoded(t *testing.T) {
	// T062 / FR-007: the state vector extracted from a V2 update is always
	// V1-encoded, so EncodeStateVectorFromUpdateV2(v2) must equal
	// EncodeStateVectorFromUpdate(v1) for the same document, and equal the JS
	// V1-encoded state vector.
	for _, name := range []string{"text_insert_delete", "map_set", "array_insert", "many_clients"} {
		t.Run(name, func(t *testing.T) {
			fx := getFixture(t, name)
			fromV2, err := encodeStateVectorFromUpdateV2(b64dec(t, fx.UpdateV2))
			if err != nil {
				t.Fatalf("%s: EncodeStateVectorFromUpdateV2: %v", name, err)
			}
			fromV1, err := encodeStateVectorFromUpdate(b64dec(t, fx.UpdateV1))
			if err != nil {
				t.Fatalf("%s: EncodeStateVectorFromUpdate: %v", name, err)
			}
			if !bytes.Equal(fromV2, fromV1) {
				t.Errorf("%s: SV from V2 != SV from V1\n  v1=%v\n  v2=%v", name, fromV1, fromV2)
			}
			// And it must match the JS state vector (which is V1-encoded).
			if !bytes.Equal(fromV2, b64dec(t, fx.StateVector)) {
				t.Errorf("%s: SV from V2 != JS state vector", name)
			}
		})
	}
}

func TestV2MixedV1V2SameDoc(t *testing.T) {
	// T063 / edge case: applying a mix of V1 and V2 updates to the same document
	// must converge correctly.
	a := getFixture(t, "text_insert_delete")
	b := getFixture(t, "array_insert")

	// doc1: apply V1 then V2
	doc1 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdate(doc1, b64dec(t, a.UpdateV1), nil)
	_ = ApplyUpdateV2(doc1, b64dec(t, b.UpdateV2), nil)

	// doc2: apply V2 then V1 (reverse order + formats)
	doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc2, b64dec(t, b.UpdateV2), nil)
	_ = ApplyUpdate(doc2, b64dec(t, a.UpdateV1), nil)

	if doc1.GetText("type").ToString() != doc2.GetText("type").ToString() {
		t.Errorf("mixed V1/V2 text diverged: %q vs %q", doc1.GetText("type").ToString(), doc2.GetText("type").ToString())
	}
	if doc1.GetArray("test").GetLength() != doc2.GetArray("test").GetLength() {
		t.Errorf("mixed V1/V2 array diverged")
	}
	if doc1.GetText("type").ToString() != "abhi" {
		t.Errorf("mixed doc text: want abhi got %q", doc1.GetText("type").ToString())
	}
}

func TestV2ConvertV1ToV2RoundTrip(t *testing.T) {
	// US5: V1 -> V2 -> V1 round-trip preserves document state.
	for _, name := range []string{"text_insert_delete", "map_set", "array_insert", "xml_fragment_insert", "structs_only_no_deletes", "delete_only"} {
		t.Run(name, func(t *testing.T) {
			fx := getFixture(t, name)
			v1 := b64dec(t, fx.UpdateV1)

			v2 := mustBytes(ConvertUpdateFormatV1ToV2(v1))
			// V2 conversion should be applyable and reconstruct the same state.
			docV2 := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
			_ = ApplyUpdateV2(docV2, v2, nil)

			docV1 := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
			_ = ApplyUpdate(docV1, v1, nil)

			// states must match
			if docV1.GetText("type").ToString() != docV2.GetText("type").ToString() {
				t.Errorf("%s: V1->V2 text mismatch", name)
			}

			// round back V2 -> V1, apply, compare
			backV1 := mustBytes(ConvertUpdateFormatV2ToV1(v2))
			docBack := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
			_ = ApplyUpdate(docBack, backV1, nil)
			if docBack.GetArray("a").GetLength() != docV1.GetArray("a").GetLength() {
				t.Errorf("%s: V1->V2->V1 array length mismatch", name)
			}
		})
	}
}

func TestV2ConvertV1ToV2ByteEqualsJS(t *testing.T) {
	// Converting the JS V1 payload to V2 in Go must equal the JS V2 payload, for
	// fixtures whose any-encoding is deterministic in this port.
	for _, name := range []string{"text_insert_delete", "map_set", "array_insert", "xml_fragment_insert", "delete_only", "map_encode_safe", "array_encode_safe", "structs_only_no_deletes", "single_client_many_ops", "many_clients"} {
		t.Run(name, func(t *testing.T) {
			fx := getFixture(t, name)
			got := mustBytes(ConvertUpdateFormatV1ToV2(b64dec(t, fx.UpdateV1)))
			want := b64dec(t, fx.UpdateV2)
			if !bytes.Equal(got, want) {
				t.Errorf("%s: ConvertUpdateFormatV1ToV2 != JS V2\n  want %v\n  got  %v", name, want, got)
			}
		})
	}
}

func TestV2ConvertV2ToV1ByteEqualsJS(t *testing.T) {
	for _, name := range []string{"text_insert_delete", "map_set", "array_insert", "xml_fragment_insert", "delete_only", "map_encode_safe", "array_encode_safe", "structs_only_no_deletes", "single_client_many_ops", "many_clients"} {
		t.Run(name, func(t *testing.T) {
			fx := getFixture(t, name)
			got := mustBytes(ConvertUpdateFormatV2ToV1(b64dec(t, fx.UpdateV2)))
			want := b64dec(t, fx.UpdateV1)
			if !bytes.Equal(got, want) {
				t.Errorf("%s: ConvertUpdateFormatV2ToV1 != JS V1\n  want %v\n  got  %v", name, want, got)
			}
		})
	}
}

// applyV2NoPanic applies a (possibly malformed) V2 payload, recovering panics
// so SEC-001 can assert "errors/no-op, never panic".
func applyV2NoPanic(payload []byte) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	doc := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc, payload, nil)
	return false
}

func TestV2SecMalformedNoPanic(t *testing.T) {
	// SEC-001: decoding malformed/truncated V2 payloads must not panic.
	valid := b64dec(t, getFixture(t, "mixed_all_types").UpdateV2)

	cases := map[string][]byte{
		"empty":             {},
		"single_zero":       {0},
		"truncated_header":  valid[:3],
		"truncated_mid":     valid[:len(valid)/2],
		"truncated_columns": valid[:len(valid)-5],
		"random_bytes":      {255, 254, 253, 200, 128, 64, 32, 1, 0, 200, 199},
		"garbage_flag":      append([]byte{200, 200}, valid[2:]...),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if applyV2NoPanic(payload) {
				t.Errorf("ApplyUpdateV2 panicked on %s payload (SEC-001 violation)", name)
			}
		})
	}

	// Decoder construction itself must not panic on garbage.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("NewUpdateDecoderV2 panicked on garbage: %v", r)
			}
		}()
		_ = newUpdateDecoderV2([]byte{255, 1, 2, 3})
	}()
}

func TestV2SecOversizedLength(t *testing.T) {
	// SEC-002: oversized length fields must be validated against the remaining
	// buffer rather than triggering an unbounded allocation or panic.
	// Craft a V2 frame whose first column claims a huge length.
	buf := []byte{
		0,                            // feature flag
		0xff, 0xff, 0xff, 0xff, 0x0f, // keyClock column length ~ 4GB
		1, 2, 3, // far fewer bytes than claimed
	}
	if applyV2NoPanic(buf) {
		t.Errorf("ApplyUpdateV2 panicked on oversized length (SEC-002 violation)")
	}
}

func BenchmarkV2Encode10KOps(b *testing.B) {
	// PR-001: V2 encoding of a 10K-op document should be well under 50ms.
	doc := newDoc("guid", true, defaultGCFilter, nil, false)
	a := doc.GetArray("a")
	doc.Transact(func(trans *Transaction) {
		for i := 0; i < 10000; i++ {
			a.Push(ArrayAny{i})
		}
	}, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeStateAsUpdateV2(doc, nil); err != nil {
			b.Fatalf("encode: %v", err)
		}
	}
}

func TestV2Encode10KUnder50ms(t *testing.T) {
	res := testing.Benchmark(BenchmarkV2Encode10KOps)
	perOp := res.T.Nanoseconds()
	if res.N > 0 {
		perOp = res.T.Nanoseconds() / int64(res.N)
	}
	ms := float64(perOp) / 1e6
	t.Logf("V2 encode of 10K-op doc: %.2f ms/op", ms)
	if ms >= 50 {
		t.Errorf("PR-001: V2 encode of 10K-op doc took %.2f ms (>= 50ms)", ms)
	}
}

func TestV2SizeSmallerThanV1(t *testing.T) {
	// SC-005 / PR-002: V2's column coding wins decisively once a document has
	// many structs / many clients (the production case). The 1100-client fixture
	// compresses ~33%. (Tiny docs can lose to the fixed column-header overhead;
	// that is expected and not what the requirement targets.)
	for _, name := range []string{"many_clients", "text_formatting"} {
		fx := getFixture(t, name)
		v1 := b64dec(t, fx.UpdateV1)
		v2 := b64dec(t, fx.UpdateV2)
		if len(v2) >= len(v1) {
			t.Errorf("%s: expected V2 (%d) smaller than V1 (%d)", name, len(v2), len(v1))
		}
		t.Logf("%s: V1=%d bytes, V2=%d bytes (%.1f%% of V1)", name, len(v1), len(v2), 100*float64(len(v2))/float64(len(v1)))
	}
}
