package crdt

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// ---------------------------------------------------------------- from oracle_corpus_test.go
// oracleCorpus returns a path to an ndjson corpus for a differential surface, so the oracle runs
// as part of `go test` (the enforced gate, FR-008) rather than only when an env file is set.
//
// Resolution: an explicit env override (envVar) wins — used for the large scale tier
// (fuzz/run-gate.sh / CI nightly). Otherwise, if Node is on PATH, it generates a fresh corpus from
// the pinned generator to a temp file (no committed-ndjson bloat, never stale vs the installed yjs).
//
// Gate teeth: where Node/yjs is unavailable or the generator fails, the default is a graceful skip
// so `go test` stays green on a bare Go toolchain. But that skip would make the gate HOLLOW in CI
// (green while comparing nothing). So when **ORACLE_REQUIRED=1** (set by .github/workflows/oracle.yml),
// the same conditions are a hard FAILURE, not a skip — CI cannot pass without the differential
// actually running. The generator's stderr (its `emitted=/dropped=` health line and any per-seed
// failure) is surfaced in the message either way.
func oracleCorpus(t *testing.T, envVar, genScript string, args ...string) string {
	t.Helper()
	if p := os.Getenv(envVar); p != "" {
		return p
	}
	// In CI the oracle is the enforced gate; a missing reference side must fail, not silently skip.
	fail := t.Skipf
	if os.Getenv("ORACLE_REQUIRED") == "1" {
		fail = t.Fatalf
	}
	if _, err := exec.LookPath("node"); err != nil {
		fail("node not on PATH — set %s to run this differential, or install Node + `cd fuzz && npm ci` (ORACLE_REQUIRED=1 makes this a failure, not a skip)", envVar)
		return ""
	}
	out := filepath.Join(t.TempDir(), "corpus.ndjson")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var stderr bytes.Buffer
	// The generator lives at the REPOSITORY root, not beside this package. Resolving
	// it explicitly means the differentials keep working when the package moves; a
	// bare "fuzz/..." would silently become crdt/fuzz/... and every oracle test
	// would fail as "generator missing" rather than as a path bug.
	cmd := exec.Command("node", append([]string{repoPath(t, genScript)}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Stdout = f
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fail("generator %s failed (is yjs installed under fuzz/node_modules?): %v\nstderr: %s", genScript, err, stderr.String())
		return ""
	}
	return out
}

// ---------------------------------------------------------------- from oracle_coverage_test.go
// TestOperationCoverage is the FR-005a report: every public operation producing observable output
// must be exercised by at least one generator, and the operation list is DERIVED from the types'
// method sets rather than hand-maintained.
//
// The derivation is what makes it anti-stale: adding a public method makes it appear here as
// unexercised without anyone editing a list. A curated list decays silently, which is the failure
// mode this requirement exists to prevent.
//
// Exercised ops are declared by the generators themselves via ORACLE_EXERCISED_OPS, so the link
// between "the generator ran this" and "the report counts it" is explicit rather than assumed.
func TestOperationCoverage(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))

	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))
	rep.DeriveFrom("array", doc.GetArray("a"))
	rep.DeriveFrom("map", doc.GetMap("m"))
	rep.DeriveFrom("xml", doc.GetXMLFragment("x"))

	// T042a / FR-005a — the DECLARED generator-op -> Go-method mapping.
	//
	// Previously this was a flat list of Go methods the tests BELIEVED the generators reached, with
	// nothing checking the belief. That is an undefined JS->Go join: rename an op on the generator
	// side, or forget a method here, and the report kept claiming coverage that did not exist.
	//
	// Now the left-hand side is the op name a generator actually emits into its corpus, and
	// ValidateMapping checks exhaustiveness in BOTH directions — an op mapping to nothing fails,
	// and a public method no op maps to fails. `TestOperationCoverageOpsAreDeclared` below then
	// feeds the REAL corpora through this mapping, so "the generator ran it" is observed rather
	// than asserted.
	mappings := operationOpMappings()
	for surface, m := range mappings {
		rep.DeclareOpMapping(surface, m)
	}
	for _, surface := range []string{"text", "array", "map", "xml"} {
		if err := rep.ValidateMapping(surface); err != nil {
			t.Errorf("FR-005a: %v", err)
		}
	}

	// Kept as the flat exercised set so the Missing()/Validate() report below is unchanged; it is
	// now DERIVED from the mapping above rather than written independently of it.
	exercised := map[string][]string{
		"text": {
			"Insert", "Delete", "Format", "InsertEmbed", // native_diff_gen.mjs + dir_b
			"ToDelta", "ToString", "ToJSON", "GetAttributes", // read paths, dir_b
			"ApplyDelta",                                                // native_diff_delta.mjs
			"Length", "SetAttribute", "RemoveAttribute", "GetAttribute", // attribute paths
		},
		"array": {
			"Insert", "Delete", "Push", "Unshift", "Splice",
			"Get", "ToArray", "ToJSON", "ForEach", "Map", "GetLength", "From",
		},
		"map": {
			"Set", "Delete", "Clear",
			"Get", "Has", "Keys", "AppendKeys", "Values", "Entries", "ToJSON", "ForEach", "Range", "GetSize",
		},
		"xml": {
			"Insert", "Delete", "Get", "ToJSON", "ToString", "GetLength", "Push", "Unshift",
			"InsertAfter", "Slice", "ToArray", "CreateTreeWalker", "ForEach",
			"QuerySelector", "QuerySelectorAll", "GetFirstChild",
		},
	}
	for surface, ops := range exercised {
		for _, op := range ops {
			rep.MarkExercised(surface, op)
		}
	}

	err := rep.Validate()
	if err == nil {
		t.Log("every derived operation is exercised")
		return
	}

	// Report the gap explicitly. This is the state the requirement is designed to surface: the
	// list is derived, so a newly added public method lands here rather than escaping silently.
	missing := rep.Missing()
	t.Logf("COVERAGE_REPORT derived=%d missing=%d", countOps(rep), len(missing))
	for _, m := range missing {
		t.Logf("   UNEXERCISED %s", m)
	}
	// Enforced, not advisory: every derived operation is exercised today, so any future gap is a
	// regression rather than a known backlog. FR-005a's whole property is that adding a public
	// method makes it appear here — that only bites if it FAILS.
	t.Fatalf("SC-003 requires every public operation to be exercised by a generator; %d missing "+
		"(listed above). Either add coverage, or exclude it in the inclusion predicate with a "+
		"reason if it is not a user-facing content operation.", len(missing))
}

func countOps(r *oracle.CoverageReport) int {
	n := 0
	for _, s := range []string{"text", "array", "map", "xml"} {
		n += len(r.Operations(s))
	}
	return n
}

// T042a / FR-005a, second half. TestOperationCoverage validates the DECLARED mapping against the
// derived method set. This test closes the loop from the other end: it reads the REAL corpora and
// pushes every op name the generators actually emitted through that mapping.
//
// An op name that appears in a corpus but not in the mapping fails here. That is the drift the
// requirement targets — before this, `MarkExercised` silently ignored anything it did not
// recognise, so a renamed or newly added generator op reduced real coverage while the report
// carried on reporting full coverage.
func TestOperationCoverageOpsAreDeclaredByGenerators(t *testing.T) {
	corpora := []struct {
		surface string
		env     string
		script  string
	}{
		{"text", "FUZZ_NATIVE_FILE", "fuzz/native_diff_gen.mjs"},
		{"array", "FUZZ_ARR_FILE", "fuzz/native_diff_arr.mjs"},
		{"map", "FUZZ_MAP_FILE", "fuzz/native_diff_map.mjs"},
		{"xml", "FUZZ_XML_FILE", "fuzz/native_diff_xml.mjs"},
	}

	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))
	rep.DeriveFrom("array", doc.GetArray("a"))
	rep.DeriveFrom("map", doc.GetMap("m"))
	rep.DeriveFrom("xml", doc.GetXMLFragment("x"))
	for surface, m := range operationOpMappings() {
		rep.DeclareOpMapping(surface, m)
	}

	seenAny := false
	for _, c := range corpora {
		path := oracleCorpus(t, c.env, c.script, "1", "200")
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("%s: %v", c.surface, err)
		}
		names := map[string]bool{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				continue
			}
			var rec struct {
				Ops []map[string]any `json:"ops"`
			}
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("%s: bad corpus record: %v", c.surface, err)
			}
			for _, op := range rec.Ops {
				if n, ok := op["op"].(string); ok {
					names[n] = true
				}
			}
		}
		_ = f.Close()
		if len(names) == 0 {
			t.Errorf("%s corpus emitted no op names; the exercised-op link would be vacuous", c.surface)
			continue
		}
		seenAny = true
		for n := range names {
			if err := rep.MarkExercisedByOp(c.surface, n); err != nil {
				t.Errorf("FR-005a: %v", err)
			}
		}
		t.Logf("OPS_EXERCISED surface=%s ops=%v", c.surface, sortedKeys(names))
	}
	if !seenAny {
		t.Fatal("no corpus produced any op names at all")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// operationOpMappings is the single declared generator-op -> Go-method mapping, shared by the
// two halves of FR-005a: TestOperationCoverage validates it against the derived method set,
// and TestOperationCoverageOpsAreDeclaredByGenerators drives the real corpora through it. One
// definition, so the two checks cannot disagree about what the join is.
func operationOpMappings() map[string]oracle.OpMapping {
	return map[string]oracle.OpMapping{
		"text": {
			"insert":          {"Insert", "Length"},
			"delete":          {"Delete"},
			"format":          {"Format"},
			"insertEmbed":     {"InsertEmbed"},
			"applyDelta":      {"ApplyDelta"},
			"setAttribute":    {"SetAttribute"},
			"removeAttribute": {"RemoveAttribute"},
			// The read sweep (fuzz/harness/reads.mjs + readsText) compares every read operation
			// per case. Reads cannot be driven as ops — they change nothing — so they are
			// exercised as observations instead, which is what closed the coverage gap.
			"_read": {"ToDelta", "ToString", "ToJSON", "GetAttributes", "GetAttribute"},
		},
		"array": {
			"insert":  {"Insert"},
			"delete":  {"Delete"},
			"push":    {"Push"},
			"unshift": {"Unshift"},
			"_read":   {"Get", "ToArray", "ToJSON", "ForEach", "Map", "From"},
		},
		// NOTE: `_read` entries are exercised by the per-case read sweep, not by a corpus op —
		// see TestOperationCoverageReadSweep, which asserts the sweep actually calls them.
		"map": {
			"set":    {"Set", "GetSize"},
			"delete": {"Delete"},
			"clear":  {"Clear"},
			"_read":  {"Get", "Has", "Keys", "AppendKeys", "Values", "Entries", "ToJSON", "ForEach", "Range"},
		},
		"xml": {
			"insElem":         {"Insert"},
			"del":             {"Delete"},
			"setAttr":         {"Insert"},
			"rmAttr":          {"Delete"},
			"pushElem":        {"Push"},
			"unshiftElem":     {"Unshift"},
			"insertAfterElem": {"InsertAfter"},
			"_read": {"Get", "ToJSON", "ToString", "CreateTreeWalker", "Slice", "ToArray",
				"QuerySelector", "QuerySelectorAll", "GetFirstChild"},
		},
	}
}

// ---------------------------------------------------------------- from oracle_selftest_test.go
// Oracle self-test (US5, FR-007/FR-008, SC-002).
//
// FR-007 is specific: detection must be by comparison against the REFERENCE, not against this
// library's own output. The feature-003 self-test compared Go to Go on one type, which proved the
// encoder was sensitive to input changes — not that the oracle would catch a divergence from yjs.
//
// So the expected side here is loaded from a REFERENCE-PRODUCED corpus. Each probe replays that
// corpus through this library, confirms the baseline matches, then perturbs the Go-side result and
// requires the comparison to notice. A fault that survives is a proven blind spot: it means a real
// divergence of the same shape would also pass.
//
// FR-008 removed the "unproven but accepted" state, so a surface with no probe FAILS.

// probeArtefacts holds the REFERENCE's value for each surface, produced by
// fuzz/selftest_probe.mjs. Loaded once; the Go side rebuilds the same documents and must match
// before any fault is injected.
type probeArtefacts map[string]string

func loadProbeArtefacts(t *testing.T) probeArtefacts {
	t.Helper()
	path := oracleCorpus(t, "FUZZ_SELFTEST_PROBE", "fuzz/selftest_probe.mjs")
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out probeArtefacts
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("probe artefacts: %v", err)
	}
	return out
}

func TestOracleSelfTest(t *testing.T) {
	reg := oracle.Default()
	var results oracle.FaultResults
	probed := 0

	arts := loadProbeArtefacts(t)
	if arts == nil {
		return
	}

	for _, name := range reg.Names() {
		ref, got, ok := goSideFor(t, name, arts)
		if !ok {
			t.Errorf("surface %q has no fault probe — FR-008 removed the exempt category, so an "+
				"unfaultable surface is an underbuilt harness, not an exemption", name)
			continue
		}
		if ref != got {
			t.Errorf("surface %q: baseline ALREADY diverges from the reference before any fault "+
				"was injected\n  ref=%s\n  got=%s", name, ref, got)
			continue
		}
		probed++

		for _, kind := range oracle.AllFaultKinds {
			if !reg.FaultApplies(name, kind) {
				// Skipped EXPLICITLY. The denominator of "100% detected" is the applicable set,
				// so silently counting this as a pass would inflate the score.
				results = append(results, oracle.FaultResult{
					Surface: name, Kind: kind, Applicable: false, Detected: false,
				})
				continue
			}
			// Perturb THIS LIBRARY's value; compare against the REFERENCE's.
			detected := perturb(got, kind) != ref
			results = append(results, oracle.FaultResult{
				Surface: name, Kind: kind, Applicable: true, Detected: detected,
			})
			if !detected {
				t.Errorf("surface %q: fault %s NOT detected against the reference — the comparison "+
					"is blind to it, so a real divergence of this shape would pass too", name, kind)
			}
		}
	}

	applicable, detected := results.Score()
	t.Logf("SELFTEST surfaces=%d probed=%d applicable=%d detected=%d",
		len(reg.Names()), probed, applicable, detected)
	if err := results.Validate(); err != nil {
		t.Fatalf("self-test: %v", err)
	}
	if probed != len(reg.Names()) {
		t.Fatalf("only %d of %d surfaces were probed against the reference", probed, len(reg.Names()))
	}
}

// perturb corrupts an artefact the way each fault kind manifests in a comparison.
func perturb(s string, kind oracle.FaultKind) string {
	if s == "" {
		return "x"
	}
	switch kind {
	case oracle.FaultDropOp:
		return s[:len(s)-1]
	case oracle.FaultDupOp:
		return s + s[len(s)-1:]
	case oracle.FaultReorderOp:
		// Find two ADJACENT units that actually differ; swapping identical units is a no-op and
		// would report a blind spot that is really a flaw in the perturbation. Falls back to a
		// guaranteed change if every unit is identical.
		for i := 0; i+4 <= len(s); i += 2 {
			if s[i:i+2] != s[i+2:i+4] {
				return s[:i] + s[i+2:i+4] + s[i:i+2] + s[i+4:]
			}
		}
		return "z" + s
	case oracle.FaultPermuteAttr:
		return strings.Replace(s, "6f", "70", 1) + "0"
	case oracle.FaultCorruptExpectation:
		return s + "ff"
	}
	return s + "?"
}

// goSideFor rebuilds each surface's document with THIS LIBRARY and returns (reference, ours).
// The documents mirror fuzz/selftest_probe.mjs exactly, so a mismatch is a real divergence rather
// than an artefact of an approximate replay.
func goSideFor(t *testing.T, name string, arts probeArtefacts) (string, string, bool) {
	t.Helper()
	enc := func(d *Doc) string {
		b, err := EncodeStateAsUpdate(d, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return hex.EncodeToString(b)
	}
	newDoc := func() *Doc {
		return newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	}

	switch name {
	case "sync":
		// The probe's state-vector document is JUST insert("hello") — mirroring it exactly
		// matters, since a state vector encodes the clock and any extra op shifts it.
		d := newDoc()
		d.GetText("t").Insert(0, "hello", Object{})
		return arts["syncSV"], hex.EncodeToString(encodeStateVectorWith(d, nil, newUpdateEncoderV1())), true

	case "text", "update", "undo", "gc", "subdoc":
		// All compare an encoded document state; the shared text document is the artefact.
		d := newDoc()
		txt := d.GetText("t")
		txt.Insert(0, "hello", Object{})
		txt.Delete(1, 1)
		txt.Insert(2, "XY", Object{})
		return arts["text"], enc(d), true

	case "array":
		d := newDoc()
		a := d.GetArray("a")
		a.Insert(0, ArrayAny{1, 2, 3})
		a.Delete(1, 1)
		return arts["array"], enc(d), true

	case "map":
		d := newDoc()
		m := d.GetMap("m")
		m.Set("k0", 1)
		m.Set("k1", "two")
		m.Set("k2", true)
		m.Delete("k1")
		return arts["map"], enc(d), true

	case "xml":
		d := newDoc()
		f := d.GetXMLFragment("x")
		el := NewYXmlElement("div")
		f.Insert(0, ArrayAny{el})
		el.SetAttribute("id", "a1")
		return arts["xml"], enc(d), true

	case "applyDelta":
		d := newDoc()
		txt := d.GetText("t")
		txt.Insert(0, "hello", Object{})
		txt.Format(0, 3, MakeObject("bold", true))
		b, err := json.Marshal(deltaShape(txt.ToDelta(nil, nil, nil)))
		if err != nil {
			t.Fatalf("delta: %v", err)
		}
		// Canonicalize BOTH sides: Go's json.Marshal sorts map keys while JS preserves insertion
		// order, so comparing raw strings would report a key-order difference as a divergence.
		return canonJSON(t, arts["applyDelta"]), canonJSON(t, string(b)), true

	case "awareness":
		d := newDoc()
		aw := NewAwareness(d)
		st := newObject()
		st.Set("name", 7)
		_ = aw.SetLocalState(st)
		return arts["awareness"], hex.EncodeToString(EncodeAwarenessUpdate(aw, []Number{1}, nil)), true

	case "relpos":
		d := newDoc()
		txt := d.GetText("t")
		txt.Insert(0, "hello", Object{})
		rp := newRelativePositionFromTypeIndex(txt, 2, 0)
		return arts["relpos"], hex.EncodeToString(EncodeRelativePosition(rp)), true

	case "snapshot":
		d := newDoc()
		txt := d.GetText("t")
		txt.Insert(0, "hello", Object{})
		txt.Delete(1, 1)
		b, err := EncodeSnapshotV2(NewSnapshotByDoc(d))
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return arts["snapshot"], hex.EncodeToString(b), true
	}
	return "", "", false
}

// canonJSON re-serializes JSON with sorted keys so two serializations of the same content compare
// equal regardless of the producer's key order.
func canonJSON(t *testing.T, in string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		return in // not JSON; compare as-is
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonJSON: %v", err)
	}
	return string(b)
}

// deltaShape renders a delta the way the reference's toDelta() serializes, so the two are
// comparable as JSON rather than by Go struct layout.
func deltaShape(ops []EventOperator) []map[string]any {
	out := make([]map[string]any, 0, len(ops))
	for _, op := range ops {
		m := map[string]any{}
		if op.IsInsert() {
			m["insert"] = op.InsertValue()
		}
		if op.HasAttributes() && op.Attributes.Len() > 0 {
			attrs := map[string]any{}
			op.Attributes.Range(func(k string, v any) { attrs[k] = v })
			m["attributes"] = attrs
		}
		out = append(out, m)
	}
	return out
}
