package crdt

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// ---------------------------------------------------------------- from native_arr_diff_test.go
func TestNativeArrayDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_ARR_FILE", "fuzz/native_diff_arr.mjs", "1", "1000")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	total, div := 0, 0
	readDiv := 0
	var firstRead []int
	var first []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed  int              `json:"seed"`
			Ops   []map[string]any `json:"ops"`
			State string           `json:"state"`
			Reads string           `json:"reads"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC: %v", c.Seed, r)
				}
			}()
			for _, op := range c.Ops {
				switch op["op"].(string) {
				case "insert", "push", "unshift":
					raw := op["vals"].([]any)
					vals := make(ArrayAny, len(raw))
					for i, v := range raw {
						switch vv := v.(type) {
						case nil:
							vals[i] = Null // JS null -> Null sentinel
						case map[string]any:
							// Embedded shared-type element: build it NATIVELY, mirroring the
							// generator's construction so clock assignment matches yjs.
							switch vv["ytype"].(string) {
							case "text":
								vals[i] = NewYText(vv["s"].(string))
							case "map":
								m := NewYMap(nil)
								// Multi-key now: the single-key form left the prelim flush order
								// unexercised, which is where the ordering defect lived.
								for _, e := range vv["entries"].([]any) {
									kv := e.([]any)
									mv := kv[1]
									if mv == nil {
										mv = Null
									}
									if f, isNum := mv.(float64); isNum {
										mv = int(f)
									}
									m.Set(kv[0].(string), mv)
								}
								vals[i] = m
							case "arr":
								items := vv["items"].([]any)
								gi := make(ArrayAny, len(items))
								for j, it := range items {
									if it == nil {
										gi[j] = Null
									} else {
										gi[j] = it
									}
								}
								child := NewYArray()
								child.Insert(0, gi)
								vals[i] = child
							default:
								t.Fatalf("seed %d unknown embedded ytype %v", c.Seed, vv["ytype"])
							}
						default:
							vals[i] = v
						}
					}
					// push and unshift are distinct public mutators; the value decoding above is
					// shared, but the placement must be theirs or the document diverges.
					switch op["op"].(string) {
					case "push":
						arr.Push(vals)
					case "unshift":
						arr.Unshift(vals)
					default:
						arr.Insert(int(op["idx"].(float64)), vals)
					}
				case "delete":
					arr.Delete(int(op["idx"].(float64)), int(op["len"].(float64)))
				default:
					// An op the replay does not know must FAIL, never be skipped. Silently
					// ignoring it applies a different op stream than the reference ran and then
					// compares the results — the corpus and the replay would drift apart while
					// the gate stayed green. This is how the new mutators were caught.
					t.Fatalf("seed %d: replay does not handle op %q (generator/replay drift)",
						c.Seed, op["op"].(string))
				}
			}
		}()
		s, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		if hex.EncodeToString(s) != c.State {
			div++
			if len(first) < 8 {
				first = append(first, c.Seed)
			}
		}
		// Read-path sweep. The op stream drives only the MUTATING half of the API; every read
		// and query operation was reachable by unit tests alone until now. Compared as an
		// observation per case, which is the only way a read CAN be checked differentially.
		if c.Reads != "" {
			got, e := fuzzCanon(readsArray(arr))
			if e != nil {
				t.Fatalf("seed %d reads canon: %v", c.Seed, e)
			}
			if got != c.Reads {
				readDiv++
				if len(firstRead) < 8 {
					firstRead = append(firstRead, c.Seed)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("array", path, total); err != nil {
		t.Fatal(err)
	}
	t.Logf("ARR_DIFF total=%d div=%d first=%v", total, div, first)
	t.Logf("READS_DIFF surface-reads total=%d div=%d first=%v", total, readDiv, firstRead)
	if div > 0 {
		t.Errorf("array native diverged %d/%d", div, total)
	}
	if readDiv > 0 {
		t.Errorf("read-path sweep diverged %d/%d (first %v)", readDiv, total, firstRead)
	}
}

// ---------------------------------------------------------------- from native_delta_diff_test.go
// ApplyDelta differential: replay a base text + a Quill delta exactly as yjs did
// (fuzz/native_diff_delta.mjs) and compare encoded state byte-exact. ApplyDelta is a
// native surface the existing fuzz gate (which only replays yjs UPDATES) never
// exercises — and it shares one currPos across ops, so a delete op's cleanup
// mutations to currAttributes must persist for later ops. Run:
//   node fuzz/native_diff_delta.mjs 1 2000 > /tmp/ndd.ndjson
//   FUZZ_DELTA_FILE=/tmp/ndd.ndjson go test -run TestNativeDeltaDiff -v .

func ndDelta(raw []interface{}) []EventOperator {
	ops := make([]EventOperator, 0, len(raw))
	for _, r := range raw {
		m := r.(map[string]interface{})
		var op EventOperator
		if ins, ok := m["insert"]; ok {
			op = NewTextDeltaOp(ins.(string), Object{})
		} else if ret, ok := m["retain"]; ok {
			op = NewRetainDeltaOp(int(ret.(float64)), Object{})
		} else if del, ok := m["delete"]; ok {
			op = NewDeleteDeltaOp(int(del.(float64)))
		}
		if a, ok := m["attributes"].([]interface{}); ok {
			op.Attributes = ndAttr(a)
		}
		ops = append(ops, op)
	}
	return ops
}

func TestNativeDeltaDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_DELTA_FILE", "fuzz/native_diff_delta.mjs", "1", "2000")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	total, div := 0, 0
	var first []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed  int                      `json:"seed"`
			Base  []map[string]interface{} `json:"base"`
			Delta []interface{}            `json:"delta"`
			State string                   `json:"state"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		tx := doc.GetText("t")
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC: %v", c.Seed, r)
				}
			}()
			for _, op := range c.Base {
				switch op["op"].(string) {
				case "insert":
					tx.Insert(int(op["idx"].(float64)), op["s"].(string), Object{})
				case "format":
					tx.Format(int(op["idx"].(float64)), int(op["len"].(float64)), ndAttr(op["attr"].([]interface{})))
				}
			}
			tx.ApplyDelta(ndDelta(c.Delta), true)
		}()
		s, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		if hex.EncodeToString(s) != c.State {
			div++
			if len(first) < 8 {
				first = append(first, c.Seed)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("applyDelta", path, total); err != nil {
		t.Fatal(err)
	}
	t.Logf("DELTA_DIFF total=%d div=%d first=%v", total, div, first)
	if div > 0 {
		t.Errorf("ApplyDelta native diverged %d/%d", div, total)
	}
}

// ---------------------------------------------------------------- from native_diff_test.go
// Native-op differential: replay the SAME insert/format/delete op streams that
// fuzz/native_diff_gen.mjs applied with yjs, and compare Go's encoded state
// (byte-exact item chain) against yjs. The existing fuzz gate only replays
// yjs-produced UPDATES through Go's decode/apply path; it never exercises Go's
// native formatText/negation/cleanup. Run with:
//   node fuzz/native_diff_gen.mjs 1 500 12 > /tmp/nd.ndjson
//   FUZZ_NATIVE_FILE=/tmp/nd.ndjson go test -run TestNativeOpDiff -v .

type ndCase struct {
	Seed    int                      `json:"seed"`
	Ops     []map[string]interface{} `json:"ops"`
	State   string                   `json:"state"`
	Reads   string                   `json:"reads"`
	StateV2 string                   `json:"stateV2"`
	Delta   json.RawMessage          `json:"delta"`
}

// ndAttr builds an Object from ORDERED [key,value] pairs (yjs Object.entries), so
// multi-key attributes are inserted in the same order yjs uses — a Go map would
// randomize the order and produce a different (but not wrong) item chain.
func ndAttr(pairs []interface{}) Object {
	o := newObject()
	for _, p := range pairs {
		kv := p.([]interface{})
		k := kv[0].(string)
		v := kv[1]
		if v == nil {
			o.Set(k, Null)
		} else {
			o.Set(k, v)
		}
	}
	return o
}

func TestNativeOpDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_NATIVE_FILE", "fuzz/native_diff_gen.mjs", "1", "500", "12")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	total, stateDiv := 0, 0
	readDiv := 0
	var firstRead []int
	stateDivV2, rtDivV2 := 0, 0
	var firstDivsV2 []int
	var firstDivs []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c ndCase
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("seed parse: %v", err)
		}
		total++

		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		tx := doc.GetText("t")
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC replaying ops: %v", c.Seed, r)
				}
			}()
			for _, op := range c.Ops {
				switch op["op"].(string) {
				case "insert":
					idx := int(op["idx"].(float64))
					s := op["s"].(string)
					if a, ok := op["attr"].([]interface{}); ok {
						tx.Insert(idx, s, ndAttr(a))
					} else {
						tx.Insert(idx, s, Object{})
					}
				case "format":
					tx.Format(int(op["idx"].(float64)), int(op["len"].(float64)), ndAttr(op["attr"].([]interface{})))
				case "setAttribute":
					tx.SetAttribute(op["k"].(string), op["v"])
				case "removeAttribute":
					tx.RemoveAttribute(op["k"].(string))
				case "insertEmbed":
					emb := op["embed"].(map[string]interface{})
					obj := newObject()
					for _, k := range []string{"type", "w"} {
						if v, ok := emb[k]; ok {
							if f, isNum := v.(float64); isNum {
								obj.Set(k, int(f))
							} else {
								obj.Set(k, v)
							}
						}
					}
					tx.InsertEmbed(int(op["idx"].(float64)), obj, Object{})
				case "delete":
					tx.Delete(int(op["idx"].(float64)), int(op["len"].(float64)))
				default:
					// An unknown op must FAIL, never be skipped: a silently ignored op means the
					// replay ran a different stream than the reference and then compared results,
					// so the corpus and the replay drift while the gate stays green.
					t.Fatalf("seed %d: replay does not handle op %q (generator/replay drift)",
						c.Seed, op["op"].(string))
				}
			}
		}()

		goState, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		// V2 must be byte-exact on the SAME document, and must round-trip back through
		// ApplyUpdateV2 to the same V1 bytes — encoding parity alone would not prove the V2
		// decoder agrees with the V2 encoder.
		if c.StateV2 != "" {
			goV2, e2 := EncodeStateAsUpdateV2(doc, nil)
			if e2 != nil {
				t.Errorf("seed %d V2 encode: %v", c.Seed, e2)
			} else {
				if hex.EncodeToString(goV2) != c.StateV2 {
					stateDivV2++
					if len(firstDivsV2) < 8 {
						firstDivsV2 = append(firstDivsV2, c.Seed)
					}
				}
				rt := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
				_ = ApplyUpdateV2(rt, goV2, nil)
				if rtState, e3 := EncodeStateAsUpdate(rt, nil); e3 != nil ||
					hex.EncodeToString(rtState) != c.State {
					rtDivV2++
				}
			}
		}

		// Read-path sweep: toString / toJSON / toDelta / getAttributes / getAttribute. These are
		// the operations no op stream can drive, and two of this feature's defects lived in them.
		if c.Reads != "" {
			gotReads, e := fuzzCanon(readsText(tx))
			if e != nil {
				t.Fatalf("seed %d reads canon: %v", c.Seed, e)
			}
			if gotReads != c.Reads {
				readDiv++
				if len(firstRead) < 8 {
					firstRead = append(firstRead, c.Seed)
				}
			}
		}

		if hex.EncodeToString(goState) != c.State {
			stateDiv++
			if len(firstDivs) < 8 {
				firstDivs = append(firstDivs, c.Seed)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("text", path, total); err != nil {
		t.Fatal(err)
	}

	t.Logf("NATIVE_DIFF total=%d stateDivergences=%d firstDivergentSeeds=%v", total, stateDiv, firstDivs)
	t.Logf("NATIVE_DIFF_V2 total=%d stateDivergencesV2=%d roundTripDivergencesV2=%d firstDivergentSeedsV2=%v",
		total, stateDivV2, rtDivV2, firstDivsV2)
	t.Logf("NATIVE_DIFF_READS total=%d div=%d first=%v", total, readDiv, firstRead)
	if readDiv > 0 {
		t.Errorf("text read-path sweep diverged %d/%d (first %v)", readDiv, total, firstRead)
	}
	if stateDivV2 > 0 {
		t.Errorf("Go V2 encoding diverged from yjs on %d/%d streams", stateDivV2, total)
	}
	if rtDivV2 > 0 {
		t.Errorf("Go V2 encode->ApplyUpdateV2 round-trip diverged on %d/%d streams", rtDivV2, total)
	}
	if stateDiv > 0 {
		t.Errorf("Go native op application diverged from yjs encoded state on %d/%d streams", stateDiv, total)
	}
}

// ---------------------------------------------------------------- from native_events_diff_test.go
// Differential coverage for what a deep observer is HANDED.
//
// The gate's other surfaces compare document state and read projections. The event
// payload — the path to the changed type and the per-key action map — was compared
// against nothing, and two defects lived there through 1.1M seeds: getPathTo
// counted Items rather than summing Item.Length, and the key projection was dead
// by construction and always returned empty. Both are reference-defined, so both
// are checkable against yjs; neither was checked.
//
// The generator produces the two shapes those defects need in order to be visible
// at all: coalesced predecessors (adjacent primitives merge into one ContentAny
// Item, so an index counted per Item differs from one summing lengths) and nesting
// deeper than one level (a one-level path is flat under either implementation).
func TestNativeEventsDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_EVENTS_FILE", "fuzz/native_diff_events.mjs", "1", "600")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	total, div := 0, 0
	var firstDiv []int
	withKeys, deepPaths := 0, 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed   int              `json:"seed"`
			Ops    []map[string]any `json:"ops"`
			Events string           `json:"events"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++

		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		root := doc.GetArray("a")

		var batches [][]eventRecord
		root.ObserveDeep(func(value interface{}, _ interface{}) {
			events, ok := value.([]IEventType)
			if !ok {
				return
			}
			batch := make([]eventRecord, 0, len(events))
			for _, ev := range events {
				batch = append(batch, newEventRecord(ev))
			}
			batches = append(batches, batch)
		})

		for _, op := range c.Ops {
			replayEventOp(root, op)
		}

		for _, b := range batches {
			for _, e := range b {
				if len(e.Keys) > 0 {
					withKeys++
				}
				if len(e.Path) >= 2 {
					deepPaths++
				}
			}
		}

		got, err := json.Marshal(batches)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.Events {
			div++
			if len(firstDiv) < 5 {
				firstDiv = append(firstDiv, c.Seed)
			}
			if div == 1 {
				t.Errorf("seed %d event projection differs:\n go: %s\nyjs: %s", c.Seed, got, c.Events)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	if total == 0 {
		t.Fatal("empty corpus — the surface compared NOTHING")
	}
	// The two defect shapes must actually occur, or a green result means only that
	// the generator never built the documents that expose them.
	if withKeys == 0 {
		t.Fatalf("no event carried a key projection across %d cases; this surface cannot see the "+
			"GetKeys defect it exists for", total)
	}
	if deepPaths == 0 {
		t.Fatalf("no event carried a path deeper than one level across %d cases; this surface "+
			"cannot see the nested-path defect it exists for", total)
	}
	if div > 0 {
		t.Fatalf("EVENTS_DIFF total=%d divergent=%d first=%v", total, div, firstDiv)
	}
	t.Logf("EVENTS_DIFF total=%d divergent=0 keyedEvents=%d deepPaths=%d", total, withKeys, deepPaths)
}

// eventRecord mirrors the reference generator's per-event projection exactly, so
// the two sides can be compared as bytes rather than field by field.
type eventRecord struct {
	Path []interface{}   `json:"path"`
	Keys [][]interface{} `json:"keys"`
}

// IEventType exposes Path but not GetKeys, so the key projection is reachable
// only by asserting to the concrete event type — worth knowing, since it means a
// deep-observer consumer cannot read keys through the published interface alone.
type eventKeyProjector interface {
	GetKeys() map[string]EventAction
}

func newEventRecord(ev IEventType) eventRecord {
	rec := eventRecord{Path: ev.Path(), Keys: [][]interface{}{}}
	if rec.Path == nil {
		rec.Path = []interface{}{}
	}
	kp, ok := ev.(eventKeyProjector)
	if !ok {
		return rec
	}
	for k, action := range kp.GetKeys() {
		rec.Keys = append(rec.Keys, []interface{}{k, action.Action, action.OldValue})
	}
	sort.Slice(rec.Keys, func(i, j int) bool {
		return rec.Keys[i][0].(string) < rec.Keys[j][0].(string)
	})
	return rec
}

// The descent must be identical on both sides, so it is a deterministic scan
// rather than anything that depends on iteration order.
func firstNestedArray(root abstractType) *YArray {
	arr, ok := root.(*YArray)
	if !ok {
		return nil
	}
	for i := Number(0); i < arr.GetLength(); i++ {
		if inner, ok := arr.Get(i).(*YArray); ok {
			return inner
		}
	}
	return nil
}

func firstNestedMap(arr *YArray) *YMap {
	for i := Number(0); i < arr.GetLength(); i++ {
		if m, ok := arr.Get(i).(*YMap); ok {
			return m
		}
	}
	return nil
}

func replayEventOp(root abstractType, op map[string]any) {
	arr, _ := root.(*YArray)
	if arr == nil {
		return
	}
	switch op["op"] {
	case "pushnum":
		arr.Push(ArrayAny{Number(op["n"].(float64))})
	case "pusharr":
		arr.Push(ArrayAny{NewYArray()})
	case "nestmap":
		if inner := firstNestedArray(root); inner != nil {
			inner.Push(ArrayAny{NewYMap(nil)})
		}
	case "mapset":
		if inner := firstNestedArray(root); inner != nil {
			if m := firstNestedMap(inner); m != nil {
				m.Set(op["key"].(string), Number(op["v"].(float64)))
			}
		}
	case "mapdel":
		if inner := firstNestedArray(root); inner != nil {
			if m := firstNestedMap(inner); m != nil {
				m.Delete(op["key"].(string))
			}
		}
	}
}

// ---------------------------------------------------------------- from native_map_diff_test.go
func TestNativeMapDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_MAP_FILE", "fuzz/native_diff_map.mjs", "1", "1000")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	total, div := 0, 0
	readDiv := 0
	var firstRead []int
	var first []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed  int              `json:"seed"`
			Ops   []map[string]any `json:"ops"`
			State string           `json:"state"`
			Reads string           `json:"reads"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		m := doc.GetMap("m")
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC: %v", c.Seed, r)
				}
			}()
			for _, op := range c.Ops {
				switch op["op"].(string) {
				case "set":
					v := op["v"]
					if v == nil {
						v = Null
					}
					m.Set(op["key"].(string), v)
				case "clear":
					m.Clear()
				case "delete":
					m.Delete(op["key"].(string))
				default:
					// An unknown op must FAIL, never be skipped: a silently ignored op means the
					// replay ran a different stream than the reference and then compared results,
					// so the corpus and the replay drift while the gate stays green.
					t.Fatalf("seed %d: replay does not handle op %q (generator/replay drift)",
						c.Seed, op["op"].(string))
				}
			}
		}()
		s, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		if hex.EncodeToString(s) != c.State {
			div++
			if len(first) < 8 {
				first = append(first, c.Seed)
			}
		}
		// Read-path sweep. The op stream drives only the MUTATING half of the API; every read
		// and query operation was reachable by unit tests alone until now. Compared as an
		// observation per case, which is the only way a read CAN be checked differentially.
		if c.Reads != "" {
			got, e := fuzzCanon(readsMap(m))
			if e != nil {
				t.Fatalf("seed %d reads canon: %v", c.Seed, e)
			}
			if got != c.Reads {
				readDiv++
				if len(firstRead) < 8 {
					firstRead = append(firstRead, c.Seed)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("map", path, total); err != nil {
		t.Fatal(err)
	}
	t.Logf("MAP_DIFF total=%d div=%d first=%v", total, div, first)
	t.Logf("READS_DIFF surface-reads total=%d div=%d first=%v", total, readDiv, firstRead)
	if div > 0 {
		t.Errorf("map native diverged %d/%d", div, total)
	}
	if readDiv > 0 {
		t.Errorf("read-path sweep diverged %d/%d (first %v)", readDiv, total, firstRead)
	}
}

// ---------------------------------------------------------------- from native_merge_diff_test.go
// Differential coverage for merged-update BYTES.
//
// MergeUpdates is a public API whose output is wire bytes other peers consume,
// and its layout is decided by a scheduler: which reader drains next, how ties
// break, where Skips land. Nothing compared those bytes to the reference.
// Direction B catches a non-canonical DOCUMENT encoding because the bytes
// originate here; a non-canonical MERGE was invisible to every surface.
//
// The gap is measured rather than argued. Inverting the scheduler's client
// ordering — yjs writes higher clients first — leaves a 20,000-case
// both-direction gate run entirely green, while these guards fail immediately.
// A property that determines wire output deserves a differential, not only unit
// assertions written by whoever last touched the scheduler.
//
// Inputs are replayed verbatim from the reference's own hex rather than
// regenerated, so both implementations merge byte-identical updates and any
// difference in the output belongs to the merge itself.
func TestNativeMergeDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_MERGE_FILE", "fuzz/native_diff_merge.mjs", "1", "300")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	total, divV1, divV2 := 0, 0, 0
	var firstV1, firstV2 []int
	multiReader := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed     int      `json:"seed"`
			InputsV1 []string `json:"inputsV1"`
			InputsV2 []string `json:"inputsV2"`
			MergedV1 string   `json:"mergedV1"`
			MergedV2 string   `json:"mergedV2"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++
		if len(c.InputsV1) >= 2 {
			multiReader++
		}

		decodeAll := func(hexes []string) [][]uint8 {
			out := make([][]uint8, 0, len(hexes))
			for _, h := range hexes {
				raw, err := hex.DecodeString(h)
				if err != nil {
					t.Fatalf("seed %d: bad input hex: %v", c.Seed, err)
				}
				out = append(out, raw)
			}
			return out
		}

		gotV1, err := mergeUpdatesWith(decodeAll(c.InputsV1),
			func(b []byte) updateDecoder { return newUpdateDecoderV1(b) },
			func() updateEncoder { return newUpdateEncoderV1() })
		if err != nil {
			t.Fatalf("seed %d: MergeUpdates V1: %v", c.Seed, err)
		}
		if hex.EncodeToString(gotV1) != c.MergedV1 {
			divV1++
			if len(firstV1) < 5 {
				firstV1 = append(firstV1, c.Seed)
			}
			if divV1 == 1 {
				t.Errorf("seed %d merged V1 bytes differ:\n go: %s\nyjs: %s",
					c.Seed, hex.EncodeToString(gotV1), c.MergedV1)
			}
		}

		gotV2, err := mergeUpdatesWith(decodeAll(c.InputsV2),
			func(b []byte) updateDecoder { return newUpdateDecoderV2(b) },
			func() updateEncoder { return newDefaultUpdateEncoderV2() })
		if err != nil {
			t.Fatalf("seed %d: MergeUpdates V2: %v", c.Seed, err)
		}
		if hex.EncodeToString(gotV2) != c.MergedV2 {
			divV2++
			if len(firstV2) < 5 {
				firstV2 = append(firstV2, c.Seed)
			}
			if divV2 == 1 {
				t.Errorf("seed %d merged V2 bytes differ:\n go: %s\nyjs: %s",
					c.Seed, hex.EncodeToString(gotV2), c.MergedV2)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	if total == 0 {
		t.Fatal("empty corpus — this surface compared NOTHING")
	}
	// Merging a single update exercises no scheduling at all, so a corpus of
	// one-input cases would pass while testing none of what this exists for.
	if multiReader < total {
		t.Fatalf("only %d of %d cases merged 2+ updates; the scheduler is only "+
			"exercised when readers compete", multiReader, total)
	}
	if divV1 > 0 || divV2 > 0 {
		t.Fatalf("MERGE_DIFF total=%d divergentV1=%d %v divergentV2=%d %v",
			total, divV1, firstV1, divV2, firstV2)
	}
	t.Logf("MERGE_DIFF total=%d divergent=0 (V1 and V2), all cases merged 2+ updates", total)
}

// ---------------------------------------------------------------- from native_xml_diff_test.go
func TestNativeXmlDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_XML_FILE", "fuzz/native_diff_xml.mjs", "1", "1000")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	total, div, strDiv := 0, 0, 0
	readDiv := 0
	var firstRead []int
	var first []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c struct {
			Seed  int              `json:"seed"`
			Ops   []map[string]any `json:"ops"`
			State string           `json:"state"`
			Str   string           `json:"str"`
			Reads string           `json:"reads"`
		}
		if e := json.Unmarshal(line, &c); e != nil {
			t.Fatal(e)
		}
		total++
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		frag := doc.GetXmlFragment("f")
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC: %v", c.Seed, r)
				}
			}()
			for _, op := range c.Ops {
				switch op["op"].(string) {
				case "insElem":
					frag.Insert(int(op["idx"].(float64)), ArrayAny{NewYXmlElement(op["tag"].(string))})
				case "pushElem":
					frag.Push(ArrayAny{NewYXmlElement(op["tag"].(string))})
				case "unshiftElem":
					frag.Unshift(ArrayAny{NewYXmlElement(op["tag"].(string))})
				case "insertAfterElem":
					rawRef := frag.Get(int(op["refIdx"].(float64)))
					ref, ok := rawRef.(SharedType)
					if !ok {
						t.Fatalf("seed %d invalid insertAfter target: %T", c.Seed, rawRef)
					}
					frag.InsertAfter(ref, ArrayAny{NewYXmlElement(op["tag"].(string))})
				case "rmAttr":
					idx := int(op["idx"].(float64))
					node := frag.Get(idx)
					el, ok := node.(*YXmlElement)
					if !ok {
						t.Fatalf("seed %d invalid rmAttr target at idx %d: %T (harness/generator bug)", c.Seed, idx, node)
					}
					el.RemoveAttribute(op["k"].(string))
				case "setAttr":
					idx := int(op["idx"].(float64))
					node := frag.Get(idx)
					el, ok := node.(*YXmlElement)
					if !ok {
						t.Fatalf("seed %d invalid setAttr target at idx %d: %T (oracle harness/generator bug)", c.Seed, idx, node)
					}
					v := op["v"]
					// A binary attribute value is tagged {"__bin":[...]} by the generator, because a
					// Uint8Array does not survive JSON.stringify as an array. This is the only path
					// that reaches the []uint8 arm of xmlAttrValueString (FR-016 bar (b)).
					if m, isObj := v.(map[string]any); isObj {
						raw, isBin := m["__bin"].([]any)
						if !isBin {
							t.Fatalf("seed %d: object attribute value %v is not a tagged binary (generator/harness disagreement)", c.Seed, m)
						}
						b := make([]uint8, len(raw))
						for i, n := range raw {
							b[i] = uint8(n.(float64))
						}
						v = b
					}
					if v == nil {
						v = Null
					}
					el.SetAttribute(op["k"].(string), v)
				case "del":
					frag.Delete(int(op["idx"].(float64)), 1)
				default:
					// An op the replay does not know must FAIL, never be skipped. Silently
					// ignoring it applies a different op stream than the reference ran and then
					// compares the results — the corpus and the replay would drift apart while
					// the gate stayed green. This is how the new mutators were caught.
					t.Fatalf("seed %d: replay does not handle op %q (generator/replay drift)",
						c.Seed, op["op"].(string))
				}
			}
		}()
		s, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		if hex.EncodeToString(s) != c.State {
			div++
			if len(first) < 8 {
				first = append(first, c.Seed)
			}
		}
		if frag.ToString() != c.Str {
			strDiv++
		}
		// Read-path sweep — see the array/map differentials for why reads are compared as
		// observations rather than driven as ops.
		if c.Reads != "" {
			got, e := fuzzCanon(readsXML(frag))
			if e != nil {
				t.Fatalf("seed %d reads canon: %v", c.Seed, e)
			}
			if got != c.Reads {
				readDiv++
				if len(firstRead) < 8 {
					firstRead = append(firstRead, c.Seed)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("xml", path, total); err != nil {
		t.Fatal(err)
	}
	t.Logf("XML_DIFF total=%d stateDiv=%d strDiv=%d first=%v", total, div, strDiv, first)
	t.Logf("READS_DIFF surface-reads total=%d div=%d first=%v", total, readDiv, firstRead)
	if div > 0 {
		t.Errorf("xml native state diverged %d/%d", div, total)
	}
	if strDiv > 0 {
		t.Errorf("xml ToString diverged %d/%d", strDiv, total)
	}
	if readDiv > 0 {
		t.Errorf("read-path sweep diverged %d/%d (first %v)", readDiv, total, firstRead)
	}
}
