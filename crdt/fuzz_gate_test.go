package crdt

// Cross-implementation fuzz gate for the V2 encoding work (spec 001). The JS
// half (fuzz/generate.js, pinned yjs@13.6.31) emits NDJSON cases; this test
// replays them through this fork and asserts convergence to byte-identical
// canonical state — exercising BOTH the V1 and V2 apply paths, plus Y.Text
// formatting (the gap the MVP-0 v1 gate did not cover).
//
// Env-driven so it never runs in the normal suite:
//   FUZZ_FILE   path to an NDJSON file produced by fuzz/generate.js  (required)
//   FUZZ_MODE   "single" | "concurrent"                              (default single)
//
// Run, e.g.:
//   FUZZ_FILE=/tmp/single.ndjson FUZZ_MODE=single \
//     go test -gcflags="all=-l" -run TestFuzzGate -timeout 30m .

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---- canonical serializer: MUST match fuzz/canonical.js byte-for-byte ----

// fuzzCanonString serializes a string with JS JSON.stringify semantics, matching
// canonical.js (which uses JSON.stringify). It delegates to the production
// JS-faithful encoder (marshalJSONOrdered), so < > & are literal and U+2028 /
// U+2029 are raw — NOT a Go encoding/json encoder, which (even with
// SetEscapeHTML(false)) always escapes U+2028/U+2029 and would diverge from
// canonical.js once the fuzz charset includes those characters.
func fuzzCanonString(s string) string {
	b, err := marshalJSONOrdered(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func fuzzCanon(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case NullType:
		return "null", nil
	case UndefinedType:
		return "null", nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case string:
		return fuzzCanonString(x), nil
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return fuzzCanonFloat(float64(x))
	case float64:
		return fuzzCanonFloat(x)
	case map[string]any:
		return fuzzCanonObject(x)
	case Object:
		// Canonical state comparison is order-INDEPENDENT (keys sorted), so flatten
		// the ordered Object to a plain map. Byte-order parity is asserted separately
		// by the byte-identity round-trip, not here.
		return fuzzCanonObject(x.ToMap())
	case []any:
		return fuzzCanonArray(x)
	default:
		return "", fmt.Errorf("uncanonicalizable Go type %T: %v", v, v)
	}
}

func fuzzCanonFloat(f float64) (string, error) {
	if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fmt.Errorf("non-integer number reached canon(): %v", f)
	}
	return strconv.FormatInt(int64(f), 10), nil
}

func fuzzCanonObject(m map[string]any) (string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fuzzCanonString(k))
		b.WriteByte(':')
		cv, err := fuzzCanon(m[k])
		if err != nil {
			return "", err
		}
		b.WriteString(cv)
	}
	b.WriteByte('}')
	return b.String(), nil
}

func fuzzCanonArray(a []any) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		cv, err := fuzzCanon(e)
		if err != nil {
			return "", err
		}
		b.WriteString(cv)
	}
	b.WriteByte(']')
	return b.String(), nil
}

func fuzzDocCanon(doc *Doc) (string, error) {
	obj := newObject()
	obj.Set("t", doc.GetText("t").ToString())
	obj.Set("m", doc.GetMap("m").ToJson())
	obj.Set("a", doc.GetArray("a").ToJson())
	obj.Set("x", doc.GetXmlFragment("x").ToJson())
	return fuzzCanon(obj)
}

// fuzzDeltaCanon renders a ToDelta result through the SAME canonical form the JS side uses
// (fuzz/canonical.js), so a delta can be compared byte-for-byte rather than by shape.
//
// A delta operator distinguishes an ABSENT key from a present-but-empty one — yjs omits
// `attributes` entirely when there are none, so emitting `"attributes":{}` would diverge on every
// unattributed run.
//
// This reads through the EventOperator ACCESSORS rather than its fields on purpose. The operator is
// a tagged union in the reference model and its Go representation is being reshaped; the accessors
// are the stable surface. Reading fields here would mean the oracle's canonical form had to be
// edited in the same change as the representation it verifies, which is precisely how a comparison
// stops being independent of the thing it compares.
func fuzzDeltaCanon(ops []EventOperator) (string, error) {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		fields := newObject()
		if op.IsInsert() {
			fields.Set("insert", op.InsertValue())
		}
		if op.HasAttributes() && op.Attributes.Len() > 0 {
			fields.Set("attributes", op.Attributes)
		}
		c, err := fuzzCanon(fields)
		if err != nil {
			return "", err
		}
		parts = append(parts, c)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// fuzzDeleteSetCanon renders a delete set in the canonical form the JS side emits: clients sorted
// ascending, each as [client, [[clock, len], ...]] with ranges in their stored order. Client order
// is SORTED rather than iterated, because Go map iteration is randomised and the comparison would
// otherwise fail at random.
func fuzzDeleteSetCanon(ds *deleteSet) (string, error) {
	if ds == nil {
		return "[]", nil
	}
	clients := make([]Number, 0, len(ds.clients))
	for c := range ds.clients {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })

	parts := make([]string, 0, len(clients))
	for _, c := range clients {
		ranges := make([]string, 0, len(ds.clients[c]))
		for _, it := range ds.clients[c] {
			ranges = append(ranges, fmt.Sprintf("[%d,%d]", it.clock, it.length))
		}
		parts = append(parts, fmt.Sprintf("[%d,[%s]]", c, strings.Join(ranges, ",")))
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

func fuzzB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return b
}

// bytesEqual reports whether two byte slices are identical. Local helper so the
// byte-identity round-trip check needs no extra import.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// applyFn applies one update with the given format (v1 or v2).
type applyFn func(doc *Doc, update []byte)

func applyV1(doc *Doc, update []byte) { _ = ApplyUpdate(doc, update, nil) }
func applyV2(doc *Doc, update []byte) { _ = ApplyUpdateV2(doc, update, nil) }

// fuzzApplyAll applies (update, format) pairs to a fresh doc, recovering panics.
func fuzzApplyAll(steps []fuzzStep) (panicErr any, gotState, gotDelta string, stateErr error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr = r
		}
	}()
	doc := newDoc("guid", true, defaultGCFilter, nil, false)
	for _, s := range steps {
		s.apply(doc, s.payload)
	}
	gotState, stateErr = fuzzDocCanon(doc)
	if stateErr != nil {
		return
	}
	// The corpus has always carried textDelta, and the gate declared the field and then never
	// compared it — so the whole Y.Text delta rendering (formatting runs, attribute merging,
	// embed placement) had no coverage in the update surface at all. ToString equality does NOT
	// imply delta equality: two documents can render the same characters with different
	// formatting runs.
	gotDelta, stateErr = fuzzDeltaCanon(doc.GetText("t").ToDelta(nil, nil, nil))
	return
}

type fuzzStep struct {
	apply   applyFn
	payload []byte
}

type fuzzSingleRec struct {
	Seed      int    `json:"seed"`
	Ops       int    `json:"ops"`
	UpdateV1  string `json:"updateV1"`
	UpdateV2  string `json:"updateV2"`
	State     string `json:"state"`
	TextDelta string `json:"textDelta"`
	// Reference-parity fields for obfuscateUpdate / decodeUpdate / equalDeleteSets / logType.
	ObfuscatedV1            string  `json:"obfuscatedV1"`
	ObfuscatedV2            string  `json:"obfuscatedV2"`
	DecodedStructs          *int    `json:"decodedStructs"`
	DecodedStructsV2        *int    `json:"decodedStructsV2"`
	DecodedDs               *string `json:"decodedDs"`
	DsEqualAcrossFormats    *bool   `json:"dsEqualAcrossFormats"`
	DsEqualAfterExtraClient *bool   `json:"dsEqualAfterExtraClient"`
	LogTypeChildren         *int    `json:"logTypeChildren"`
	LogTypeDeleted          *int    `json:"logTypeDeleted"`
	// Widened-gate surfaces (work item 1.8). The generator emits each for EVERY seed
	// when its FUZZ_* flag is on, so these are pointers: a nil (absent JSON key) means
	// the corpus was generated WITHOUT that surface. The strict checks fail fast on nil
	// instead of silently skipping — otherwise a stale env-provided corpus would read
	// as "covered" when the surface was never actually exercised.
	XmlString     *string `json:"xmlString"`     // STRICT_XML (1.7B)
	PostGcState   *string `json:"postGcState"`   // STRICT_GC (1.2)
	SnapDocV1     *string `json:"snapDocV1"`     // STRICT_SNAPSHOT (1.4): gc=false doc update
	SnapshotV1    *string `json:"snapshotV1"`    // STRICT_SNAPSHOT
	SnapshotV2    *string `json:"snapshotV2"`    // STRICT_SNAPSHOT
	RestoredState *string `json:"restoredState"` // STRICT_SNAPSHOT
	// STRICT_SNAPSHOT: snapshot-AWARE toDelta (track changes). Split out from the doc above
	// because it needs a mid-stream snapshot as `prevSnapshot`, so both the added and removed
	// ychange branches are reached.
	YChangeDocV1       *string `json:"ychangeDocV1"`
	YChangeEarlySnapV1 *string `json:"ychangeEarlySnapV1"`
	YChangeLateSnapV1  *string `json:"ychangeLateSnapV1"`
	YChangeDelta       *string `json:"ychangeDelta"`
	// The six reference operations that had no Go counterpart. Unit tests prove they run; these
	// fields prove they AGREE with the reference (FR-016 bar (b)).
	MapSnapshotAll    *string  `json:"mapSnapshotAll"`
	SnapContainsSelf  *bool    `json:"snapContainsSelf"`
	SnapContainsLater *bool    `json:"snapContainsLater"`
	SnapLaterUpdateV1 *string  `json:"snapLaterUpdateV1"`
	SubdocUpdateV1    *string  `json:"subdocUpdateV1"` // STRICT_SUBDOCS (1.1)
	SubdocGuids       []string `json:"subdocGuids"`    // STRICT_SUBDOCS
}

type fuzzConcRec struct {
	Seed       int    `json:"seed"`
	Ops        int    `json:"ops"`
	BaseV1     string `json:"baseV1"`
	BaseV2     string `json:"baseV2"`
	U1V1       string `json:"u1v1"`
	U2V1       string `json:"u2v1"`
	U1V2       string `json:"u1v2"`
	U2V2       string `json:"u2v2"`
	Full1V2    string `json:"full1V2"`
	Full2V2    string `json:"full2V2"`
	State      string `json:"state"`
	TextDelta  string `json:"textDelta"`
	JsDiverged bool   `json:"jsDiverged"`
	// Diagnostics, not parity cells: JsDiverged is the checked value; S1/S2 make that
	// reference-side convergence failure actionable in the report.
	S1 string `json:"s1"`
	S2 string `json:"s2"`
}

func TestFuzzGate(t *testing.T) {
	path := os.Getenv("FUZZ_FILE")
	if path == "" {
		// Same teeth as oracleCorpus (oracle_corpus_test.go): a bare `go test` may skip, but
		// under ORACLE_REQUIRED=1 (set by .github/workflows/oracle.yml) a missing corpus is a
		// hard FAILURE. Without this the PR job ran green while never exercising the V1/V2
		// update + convergence gate at all — the same hollow-gate failure mode the native-op
		// differentials were explicitly hardened against.
		if os.Getenv("ORACLE_REQUIRED") == "1" {
			t.Fatalf("FUZZ_FILE not set but ORACLE_REQUIRED=1 — the V1/V2 update gate MUST run in CI; " +
				"generate a corpus with `node fuzz/generate.js <mode> <seedStart> <cases> <opsPerCase>`")
		}
		t.Skip("FUZZ_FILE not set; skipping differential fuzz gate")
	}
	mode := os.Getenv("FUZZ_MODE")
	if mode == "" {
		mode = "single"
	}
	// Per-surface strict assertions (work item 1.8), default off. Each later fix
	// flips its flag; the generator emits the corresponding fields.
	strictXML := os.Getenv("FUZZ_STRICT_XML") == "1"
	strictGC := os.Getenv("FUZZ_STRICT_GC") == "1"
	strictSnapshot := os.Getenv("FUZZ_STRICT_SNAPSHOT") == "1"
	strictSubdocs := os.Getenv("FUZZ_STRICT_SUBDOCS") == "1"

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	var cases, passes, failures, totalOps int
	const maxReport = 8
	// Transcript comparison is substantially cheaper than applying a case, but
	// the large local gate can contain hundreds of thousands of cases. A bounded
	// prefix gives the eager/lazy struct grammar hundreds of generated shapes in
	// both gate modes without multiplying the gate's runtime.
	const structTranscriptCaseLimit = 256

	report := func(format string, args ...any) {
		if failures <= maxReport {
			t.Errorf(format, args...)
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		cases++

		switch mode {
		case "single":
			var rec fuzzSingleRec
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("bad single record: %v", err)
			}
			if err := validateFuzzSingleBase(rec); err != nil {
				t.Fatal(err)
			}
			if err := validateFuzzStrictCells(rec, strictXML, strictGC, strictSnapshot, strictSubdocs); err != nil {
				t.Fatal(err)
			}
			totalOps += rec.Ops

			// Both the V1 and V2 payload must apply to a fresh doc and reach the
			// same canonical state — this is the core V2 decode parity check.
			ok := true
			for _, variant := range []struct {
				name string
				fn   applyFn
				b64  string
			}{
				{"v1", applyV1, rec.UpdateV1},
				{"v2", applyV2, rec.UpdateV2},
			} {
				payload := fuzzB64(t, variant.b64)
				if cases <= structTranscriptCaseLimit {
					factory := newDecoderV1
					if variant.name == "v2" {
						factory = newDecoderV2
					}
					requireStructTranscriptParity(t, fmt.Sprintf("single seed=%d %s", rec.Seed, variant.name), payload, factory)
				}
				perr, got, gotDelta, serr := fuzzApplyAll([]fuzzStep{{variant.fn, payload}})
				if perr != nil {
					ok = false
					report("seed=%d %s PANIC: %v", rec.Seed, variant.name, perr)
					break
				}
				if serr != nil {
					ok = false
					report("seed=%d %s serialize error: %v", rec.Seed, variant.name, serr)
					break
				}
				if got != rec.State {
					ok = false
					report("seed=%d %s DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, variant.name, rec.State, got)
					break
				}
				if rec.TextDelta != "" && gotDelta != rec.TextDelta {
					ok = false
					report("seed=%d %s TEXT DELTA DIVERGENCE\n  js =%s\n  go =%s",
						rec.Seed, variant.name, rec.TextDelta, gotDelta)
					break
				}
			}

			// Six operations the reference has and this library did not until now. Each is
			// compared against the reference rather than merely exercised: a unit test proves the
			// Go version runs, only this proves it agrees.
			func() {
				defer func() {
					if r := recover(); r != nil {
						ok = false
						report("seed=%d reference-op PANIC: %v", rec.Seed, r)
					}
				}()
				u1 := fuzzB64(t, rec.UpdateV1)
				u2 := fuzzB64(t, rec.UpdateV2)

				// obfuscateUpdate — byte-exact. The obfuscator is deterministic, so anything
				// weaker than a byte comparison would leave evidence unused.
				if rec.ObfuscatedV1 != "" {
					got, err := obfuscateUpdate(u1)
					if err != nil {
						ok = false
						report("seed=%d obfuscateUpdate: %v", rec.Seed, err)
					} else if base64.StdEncoding.EncodeToString(got) != rec.ObfuscatedV1 {
						ok = false
						report("seed=%d OBFUSCATE V1 BYTE DIVERGENCE", rec.Seed)
					}
				}
				if rec.ObfuscatedV2 != "" {
					got, err := obfuscateUpdateV2(u2)
					if err != nil {
						ok = false
						report("seed=%d obfuscateUpdateV2: %v", rec.Seed, err)
					} else if base64.StdEncoding.EncodeToString(got) != rec.ObfuscatedV2 {
						ok = false
						report("seed=%d OBFUSCATE V2 BYTE DIVERGENCE", rec.Seed)
					}
				}

				// decodeUpdate / decodeUpdateV2 — struct counts and the delete set.
				if rec.DecodedStructs != nil {
					d1, err := decodeUpdate(u1)
					if err != nil {
						ok = false
						report("seed=%d decodeUpdate: %v", rec.Seed, err)
						return
					}
					d2, err := decodeUpdateV2(u2)
					if err != nil {
						ok = false
						report("seed=%d decodeUpdateV2: %v", rec.Seed, err)
						return
					}
					if len(d1.structs) != *rec.DecodedStructs {
						ok = false
						report("seed=%d decodeUpdate structs: js=%d go=%d",
							rec.Seed, *rec.DecodedStructs, len(d1.structs))
					}
					if rec.DecodedStructsV2 != nil && len(d2.structs) != *rec.DecodedStructsV2 {
						ok = false
						report("seed=%d decodeUpdateV2 structs: js=%d go=%d",
							rec.Seed, *rec.DecodedStructsV2, len(d2.structs))
					}
					if rec.DecodedDs != nil {
						if got, err := fuzzDeleteSetCanon(d1.ds); err != nil {
							ok = false
							report("seed=%d decoded ds canon: %v", rec.Seed, err)
						} else if got != *rec.DecodedDs {
							ok = false
							report("seed=%d DECODED DS DIVERGENCE\n  js =%s\n  go =%s",
								rec.Seed, *rec.DecodedDs, got)
						}
					}
					// equalDeleteSets — both the positive case (the two encodings agree) and the
					// negative one (an extra client must NOT compare equal), so a predicate that
					// always returned true could not pass.
					if rec.DsEqualAcrossFormats != nil {
						if got := equalDeleteSets(d1.ds, d2.ds); got != *rec.DsEqualAcrossFormats {
							ok = false
							report("seed=%d equalDeleteSets across formats: js=%v go=%v",
								rec.Seed, *rec.DsEqualAcrossFormats, got)
						}
					}
					if rec.DsEqualAfterExtraClient != nil {
						bumped := newDeleteSet()
						for c, items := range d1.ds.clients {
							cp := make([]*deleteItem, len(items))
							for i, it := range items {
								cp[i] = &deleteItem{clock: it.clock, length: it.length}
							}
							bumped.clients[c] = cp
						}
						bumped.clients[999999] = []*deleteItem{{clock: 0, length: 1}}
						if got := equalDeleteSets(d1.ds, bumped); got != *rec.DsEqualAfterExtraClient {
							ok = false
							report("seed=%d equalDeleteSets with an extra client: js=%v go=%v",
								rec.Seed, *rec.DsEqualAfterExtraClient, got)
						}
					}
				}

				// logType — its rendering is Go-specific by design, but the traversal it reports
				// is comparable, and traversal is the only part that can be wrong.
				if rec.LogTypeChildren != nil && rec.LogTypeDeleted != nil {
					ld := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdate(ld, u1, nil)
					out := logType(ld.GetText("t"))
					wantChildren := fmt.Sprintf("children: %d", *rec.LogTypeChildren)
					if !strings.Contains(out, wantChildren) {
						ok = false
						report("seed=%d logType child count: want %q in output", rec.Seed, wantChildren)
					}
					if got := strings.Count(out, "deleted=true"); got != *rec.LogTypeDeleted {
						ok = false
						report("seed=%d logType deleted count: js=%d go=%d",
							rec.Seed, *rec.LogTypeDeleted, got)
					}
				}
			}()

			// V2 round-trip: decode V2 in Go, re-encode V2 in Go, apply again —
			// the re-encoded payload must still converge (Go V2 encode path).
			if ok {
				doc := newDoc("guid", true, defaultGCFilter, nil, false)
				func() {
					defer func() {
						if r := recover(); r != nil {
							ok = false
							report("seed=%d v2-roundtrip PANIC: %v", rec.Seed, r)
						}
					}()
					_ = ApplyUpdateV2(doc, fuzzB64(t, rec.UpdateV2), nil)
					reenc, eerr := EncodeStateAsUpdateV2(doc, nil)
					if eerr != nil {
						ok = false
						report("seed=%d v2-roundtrip ENCODE ERROR: %v", rec.Seed, eerr)
						return
					}
					doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdateV2(doc2, reenc, nil)
					got, serr := fuzzDocCanon(doc2)
					if serr != nil || got != rec.State {
						ok = false
						report("seed=%d v2-roundtrip DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, rec.State, got)
					}
				}()
			}

			// BYTE-IDENTITY check (the object-key-ordering parity guard).
			//
			// The convergence checks above canonicalize state with SORTED keys, so
			// they pass even when Go re-encodes object keys in a different on-wire
			// order than JS -- they cannot detect the key-ordering bug. This block
			// asserts true byte-parity instead: apply the JS payload to a fresh Go
			// doc, re-encode it through the SAME full-document path JS used
			// (encodeStateAsUpdate / encodeStateAsUpdateV2), and compare the resulting
			// BYTES to the JS original.
			//
			// Direct-encode vs direct-encode is the only apples-to-apples byte
			// comparison: the lazy convert path (convertUpdateFormatV1ToV2)
			// legitimately drops the parentSub flag for origin-bearing items, so even
			// Yjs's own convert output differs from its own direct encode -- a
			// pre-existing property unrelated to object ordering. A single-client doc's
			// full re-encode, by contrast, reproduces JS's bytes exactly when (and only
			// when) every object's keys round-trip in insertion order. Both the V2
			// (lib0 any: ReadObject/WriteObject; format attrs via ReadJson/WriteJson =
			// ReadAny/WriteAny) and V1 (ContentJson + format attrs via JSON) content
			// paths are exercised, so a multi-key object whose key order is not
			// preserved end to end diverges here. With the multi-key objects ops.js
			// generates ({z,a,m}, ...) this FAILS on a map-randomized / sorted-key
			// encoder and passes only when Object is insertion-ordered through decode,
			// encode and JSON.
			if ok {
				func() {
					defer func() {
						if r := recover(); r != nil {
							ok = false
							report("seed=%d byte-identity PANIC: %v", rec.Seed, r)
						}
					}()
					jsV1 := fuzzB64(t, rec.UpdateV1)
					jsV2 := fuzzB64(t, rec.UpdateV2)

					// V2: apply JS V2, re-encode V2 via the full-doc path, compare bytes.
					dV2 := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdateV2(dV2, jsV2, nil)
					goV2, e2 := EncodeStateAsUpdateV2(dV2, nil)
					if e2 != nil {
						ok = false
						report("seed=%d byte-identity V2 ENCODE ERROR: %v", rec.Seed, e2)
						return
					}
					if !bytesEqual(goV2, jsV2) {
						ok = false
						report("seed=%d byte-identity V2 BYTE DIVERGENCE\n  jsV2=%v\n  goV2=%v", rec.Seed, jsV2, goV2)
						return
					}

					// V1: apply JS V1, re-encode V1 via the full-doc path, compare bytes.
					dV1 := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdate(dV1, jsV1, nil)
					goV1, e1 := EncodeStateAsUpdate(dV1, nil)
					if e1 != nil {
						ok = false
						report("seed=%d byte-identity V1 ENCODE ERROR: %v", rec.Seed, e1)
						return
					}
					if !bytesEqual(goV1, jsV1) {
						ok = false
						report("seed=%d byte-identity V1 BYTE DIVERGENCE\n  jsV1=%v\n  goV1=%v", rec.Seed, jsV1, goV1)
					}
				}()
			}

			// ---- widened-gate strict surfaces (work item 1.8) ----

			// STRICT_XML (1.7B): XmlFragment.ToString() byte-equal to yjs over the
			// random XML the ops generated.
			if ok && strictXML {
				func() {
					defer func() {
						if r := recover(); r != nil {
							ok = false
							report("seed=%d xml PANIC: %v", rec.Seed, r)
						}
					}()
					if rec.XmlString == nil {
						ok = false
						report("seed=%d STRICT_XML set but corpus lacks xmlString (stale/mismatched corpus)", rec.Seed)
						return
					}
					d := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdateV2(d, fuzzB64(t, rec.UpdateV2), nil)
					if got := d.GetXmlFragment("x").ToString(); got != *rec.XmlString {
						ok = false
						report("seed=%d xml DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, *rec.XmlString, got)
					}
				}()
			}

			// STRICT_GC (1.2): a gc=true doc GCs on delete; visible state must still
			// equal yjs's post-gc toJSON (the ContentType.GC cascade re-encode parity
			// is covered by the byte-identity block above).
			if ok && strictGC {
				if rec.PostGcState == nil {
					ok = false
					report("seed=%d STRICT_GC set but corpus lacks postGcState (stale/mismatched corpus)", rec.Seed)
				} else {
					d := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdateV2(d, fuzzB64(t, rec.UpdateV2), nil)
					got, e := fuzzDocCanon(d)
					if e != nil {
						ok = false
						report("seed=%d gc canon error: %v", rec.Seed, e)
					} else if got != *rec.PostGcState {
						ok = false
						report("seed=%d gc DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, *rec.PostGcState, got)
					}
				}
			}

			// STRICT_SNAPSHOT (1.4): rebuild the gc=false doc, assert Go's V2 snapshot
			// bytes equal yjs, a yjs V2 snapshot decodes, and the restored doc matches.
			if ok && strictSnapshot {
				func() {
					defer func() {
						if r := recover(); r != nil {
							ok = false
							report("seed=%d snapshot PANIC: %v", rec.Seed, r)
						}
					}()
					if rec.SnapDocV1 == nil || rec.SnapshotV1 == nil || rec.SnapshotV2 == nil || rec.RestoredState == nil ||
						rec.YChangeDocV1 == nil || rec.YChangeEarlySnapV1 == nil || rec.YChangeLateSnapV1 == nil ||
						rec.YChangeDelta == nil || rec.MapSnapshotAll == nil || rec.SnapContainsSelf == nil ||
						rec.SnapContainsLater == nil || rec.SnapLaterUpdateV1 == nil {
						ok = false
						report("seed=%d STRICT_SNAPSHOT set but corpus lacks snapshot fields (stale/mismatched corpus)", rec.Seed)
						return
					}
					sd := newDoc("guid", false, nil, nil, false)
					_ = ApplyUpdate(sd, fuzzB64(t, *rec.SnapDocV1), nil)
					snap := NewSnapshotByDoc(sd)
					goV2, e := EncodeSnapshotV2(snap)
					if e != nil {
						ok = false
						report("seed=%d snapshot encode error: %v", rec.Seed, e)
						return
					}
					if !bytesEqual(goV2, fuzzB64(t, *rec.SnapshotV2)) {
						ok = false
						report("seed=%d snapshot V2 BYTE DIVERGENCE", rec.Seed)
						return
					}
					// V1 snapshot bytes. The generator has always emitted these and the
					// struct has always declared them, but nothing compared them — the
					// field was annotated "unused in checks". EncodeSnapshotV1 is
					// documented as yjs encodeSnapshot, so the cell is meant to be
					// byte-exact; its only other coverage is a single static fixture.
					goV1, e1 := EncodeSnapshotV1(snap)
					if e1 != nil {
						ok = false
						report("seed=%d snapshot V1 encode error: %v", rec.Seed, e1)
						return
					}
					if !bytesEqual(goV1, fuzzB64(t, *rec.SnapshotV1)) {
						ok = false
						report("seed=%d snapshot V1 BYTE DIVERGENCE", rec.Seed)
						return
					}
					decoded, e := DecodeSnapshotV2(fuzzB64(t, *rec.SnapshotV2))
					if e != nil {
						ok = false
						report("seed=%d decode yjs snapshot V2 error: %v", rec.Seed, e)
						return
					}
					// Restore from the DECODED yjs snapshot (not the locally rebuilt `snap`),
					// so this actually verifies decode+restore parity, not just decode success.
					restored, e := CreateDocFromSnapshot(sd, decoded, newDoc("guid", false, nil, nil, false))
					if e != nil {
						ok = false
						report("seed=%d restore error: %v", rec.Seed, e)
						return
					}
					got, e := fuzzDocCanon(restored)
					if e != nil {
						ok = false
						report("seed=%d restored canon error: %v", rec.Seed, e)
					} else if got != *rec.RestoredState {
						ok = false
						report("seed=%d restored DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, *rec.RestoredState, got)
					}

					// Snapshot-aware ToDelta. This path had NO differential coverage and panicked
					// on every call (Transaction.Meta keyed by a func value — unhashable in Go),
					// so the whole track-changes rendering was dead. FR-016 bar (b).
					yd := newDoc("guid", false, nil, nil, false)
					_ = ApplyUpdate(yd, fuzzB64(t, *rec.YChangeDocV1), nil)
					early, e1 := DecodeSnapshotV1(fuzzB64(t, *rec.YChangeEarlySnapV1))
					late, e2 := DecodeSnapshotV1(fuzzB64(t, *rec.YChangeLateSnapV1))
					if e1 != nil || e2 != nil {
						ok = false
						report("seed=%d ychange snapshot decode error: %v / %v", rec.Seed, e1, e2)
						return
					}
					// typeMapGetAllSnapshot — the Y.Map counterpart of the snapshot-aware
					// ToDelta below, which this feature found had never once executed. Values are
					// projected through ToJson on both sides: the raw content of a nested type is
					// a live type with parent back-pointers, which no canonicaliser can walk.
					// Against the MID-STREAM snapshot on the ychange doc, not the
					// end-of-document one: an end-of-document snapshot sees every live value,
					// so the historical walk this function exists for is never taken.
					asOf := typeMapGetAllSnapshot(yd.GetMap("m"), early)
					proj := newObject()
					asOf.Range(func(k string, v any) {
						if t, isType := v.(abstractType); isType && t != nil {
							proj.Set(k, t.ToJson())
						} else {
							proj.Set(k, v)
						}
					})
					if got, e := fuzzCanon(proj); e != nil {
						ok = false
						report("seed=%d map snapshot canon: %v", rec.Seed, e)
					} else if got != *rec.MapSnapshotAll {
						ok = false
						report("seed=%d MAP SNAPSHOT DIVERGENCE\n  js =%s\n  go =%s",
							rec.Seed, *rec.MapSnapshotAll, got)
					}

					// snapshotContainsUpdate — both directions. TRUE for the update the snapshot
					// was taken from, FALSE once the document has moved on; a predicate that always
					// answered the same way could not pass both.
					self, e := EncodeStateAsUpdate(sd, nil)
					if e != nil {
						ok = false
						report("seed=%d snapshot self-encode: %v", rec.Seed, e)
					} else if got, e2 := SnapshotContainsUpdate(snap, self); e2 != nil {
						ok = false
						report("seed=%d SnapshotContainsUpdate: %v", rec.Seed, e2)
					} else if got != *rec.SnapContainsSelf {
						ok = false
						report("seed=%d snapshotContainsUpdate(self): js=%v go=%v",
							rec.Seed, *rec.SnapContainsSelf, got)
					}
					later := fuzzB64(t, *rec.SnapLaterUpdateV1)
					if got, e := SnapshotContainsUpdate(snap, later); e != nil {
						ok = false
						report("seed=%d SnapshotContainsUpdate(later): %v", rec.Seed, e)
					} else if got != *rec.SnapContainsLater {
						ok = false
						report("seed=%d snapshotContainsUpdate(later): js=%v go=%v",
							rec.Seed, *rec.SnapContainsLater, got)
					}

					gotDelta, e := fuzzDeltaCanon(yd.GetText("t").ToDelta(late, early, nil))
					if e != nil {
						ok = false
						report("seed=%d ychange delta canon error: %v", rec.Seed, e)
					} else if gotDelta != *rec.YChangeDelta {
						ok = false
						report("seed=%d ychange delta DIVERGENCE\n  js =%s\n  go =%s",
							rec.Seed, *rec.YChangeDelta, gotDelta)
					}
				}()
			}

			// STRICT_SUBDOCS (1.1, the 5th surface): after applying a doc with embedded
			// subdocs, GetSubdocs() must hold exactly the yjs guid set (structural —
			// embedded subdocs do not round-trip through toJSON).
			if ok && strictSubdocs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							ok = false
							report("seed=%d subdocs PANIC: %v", rec.Seed, r)
						}
					}()
					if rec.SubdocUpdateV1 == nil || rec.SubdocGuids == nil {
						ok = false
						report("seed=%d STRICT_SUBDOCS set but corpus lacks subdocUpdateV1 (stale/mismatched corpus)", rec.Seed)
						return
					}
					d := newDoc("guid", true, defaultGCFilter, nil, false)
					_ = ApplyUpdate(d, fuzzB64(t, *rec.SubdocUpdateV1), nil)
					var guids []string
					for sub := range d.GetSubdocs() {
						guids = append(guids, sub.(*Doc).Guid)
					}
					sort.Strings(guids)
					// Canonicalize BOTH sides: this is a set comparison, so a different
					// emission order on the JS side must not read as a divergence.
					wantGuids := append([]string(nil), rec.SubdocGuids...)
					sort.Strings(wantGuids)
					if strings.Join(guids, ",") != strings.Join(wantGuids, ",") {
						ok = false
						report("seed=%d subdoc guids DIVERGENCE\n  js =%v\n  go =%v", rec.Seed, wantGuids, guids)
					}
				}()
			}

			if ok {
				passes++
			} else {
				failures++
			}

		case "concurrent":
			var rec fuzzConcRec
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("bad concurrent record: %v", err)
			}
			if err := validateFuzzConcurrentBase(rec); err != nil {
				t.Fatal(err)
			}
			if rec.JsDiverged {
				report("seed=%d JS DID NOT CONVERGE (harness):\n  s1=%s\n  s2=%s", rec.Seed, rec.S1, rec.S2)
				failures++
				continue
			}
			totalOps += rec.Ops

			bV1, bV2 := fuzzB64(t, rec.BaseV1), fuzzB64(t, rec.BaseV2)
			u1V1, u2V1 := fuzzB64(t, rec.U1V1), fuzzB64(t, rec.U2V1)
			u1V2, u2V2 := fuzzB64(t, rec.U1V2), fuzzB64(t, rec.U2V2)
			f1V2, f2V2 := fuzzB64(t, rec.Full1V2), fuzzB64(t, rec.Full2V2)
			if cases <= structTranscriptCaseLimit {
				for _, update := range []struct {
					name    string
					payload []byte
					factory func([]byte) updateDecoder
				}{
					{name: "base-v1", payload: bV1, factory: newDecoderV1},
					{name: "u1-v1", payload: u1V1, factory: newDecoderV1},
					{name: "u2-v1", payload: u2V1, factory: newDecoderV1},
					{name: "base-v2", payload: bV2, factory: newDecoderV2},
					{name: "u1-v2", payload: u1V2, factory: newDecoderV2},
					{name: "u2-v2", payload: u2V2, factory: newDecoderV2},
					{name: "full1-v2", payload: f1V2, factory: newDecoderV2},
					{name: "full2-v2", payload: f2V2, factory: newDecoderV2},
				} {
					requireStructTranscriptParity(
						t,
						fmt.Sprintf("concurrent seed=%d %s", rec.Seed, update.name),
						update.payload,
						update.factory,
					)
				}
			}

			// All of these application orders/formats must converge to rec.State.
			orders := []struct {
				name  string
				steps []fuzzStep
			}{
				{"v1: base,u1,u2", []fuzzStep{{applyV1, bV1}, {applyV1, u1V1}, {applyV1, u2V1}}},
				{"v1: base,u2,u1", []fuzzStep{{applyV1, bV1}, {applyV1, u2V1}, {applyV1, u1V1}}},
				{"v2: base,u1,u2", []fuzzStep{{applyV2, bV2}, {applyV2, u1V2}, {applyV2, u2V2}}},
				{"v2: base,u2,u1", []fuzzStep{{applyV2, bV2}, {applyV2, u2V2}, {applyV2, u1V2}}},
				{"v2: full1,full2", []fuzzStep{{applyV2, f1V2}, {applyV2, f2V2}}},
				{"mixed: baseV1,u1V2,u2V1", []fuzzStep{{applyV1, bV1}, {applyV2, u1V2}, {applyV1, u2V1}}},
			}

			caseOK := true
			for _, ord := range orders {
				perr, got, gotDelta, serr := fuzzApplyAll(ord.steps)
				if perr != nil {
					caseOK = false
					report("seed=%d order=%s PANIC: %v", rec.Seed, ord.name, perr)
					break
				}
				if serr != nil {
					caseOK = false
					report("seed=%d order=%s serialize error: %v", rec.Seed, ord.name, serr)
					break
				}
				if got != rec.State {
					caseOK = false
					report("seed=%d order=%s DIVERGENCE\n  js =%s\n  go =%s", rec.Seed, ord.name, rec.State, got)
					break
				}
				// Convergence must hold for the DELTA too, not only the flat string: permuted
				// apply orders that agree on characters can still disagree on formatting runs.
				if rec.TextDelta != "" && gotDelta != rec.TextDelta {
					caseOK = false
					report("seed=%d order=%s TEXT DELTA DIVERGENCE\n  js =%s\n  go =%s",
						rec.Seed, ord.name, rec.TextDelta, gotDelta)
					break
				}
			}
			if caseOK {
				passes++
			} else {
				failures++
			}

		default:
			t.Fatalf("unknown FUZZ_MODE %q", mode)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Empty corpus == compared nothing. Without this the gate reported
	// `cases=0 pass=0 fail=0` and PASSED even under ORACLE_REQUIRED=1 with every
	// FUZZ_STRICT_* flag set — the ORACLE_REQUIRED teeth only cover an UNSET FUZZ_FILE,
	// not a set-but-empty one. All five native-op differentials already guard this
	// (`t.Fatal("empty corpus")`); the corpus-driven gate did not, so a generator step
	// that silently produced a 0-byte file left both the PR gate and the convergence
	// step green while comparing nothing.
	if cases == 0 {
		t.Fatalf("empty corpus (%s) — the gate compared NOTHING; regenerate with `node fuzz/generate.js %s <seedStart> <cases> <opsPerCase>`", path, mode)
	}

	t.Logf("FUZZ GATE mode=%s cases=%d pass=%d fail=%d totalOps=%d", mode, cases, passes, failures, totalOps)
	fmt.Printf("FUZZ_SUMMARY mode=%s cases=%d pass=%d fail=%d totalOps=%d\n", mode, cases, passes, failures, totalOps)
}
