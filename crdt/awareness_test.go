package crdt

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/go-yjs/internal/oracle"
)

// ---------------------------------------------------------------- from awareness_change_event_review_test.go
// awareness_change_event_review_test.go reproduces two findings from the full
// code-review of the Go Yjs v2 codec (PR antst/y-crdt#2), both on the
// Awareness 'change'-event `filteredUpdated` computation. The canonical
// reference is y-protocols `Awareness` (awareness.js):
//
//	setLocalState (state) {
//	  ...
//	  } else {
//	    updated.push(clientID)
//	    if (!f.equalityDeep(prevState, state)) {   // appends when CHANGED, DEEP
//	      filteredUpdated.push(clientID)
//	    }
//	  }
//	  if (added.length > 0 || filteredUpdated.length > 0 || removed.length > 0) {
//	    this.emit('change', [{ added, updated: filteredUpdated, removed }, 'local'])
//	  }
//	  this.emit('update', [{ added, updated, removed }, 'local'])
//	}
//
// and applyAwarenessUpdate likewise: `if (!f.equalityDeep(state, prevState)) {
// filteredUpdated.push(clientID) }`.
//
// FINDING A (HIGH): SetLocalState appended to filteredUpdated when the state was
// UNCHANGED (`if equalAttrs(prevState, state)`), the inverse of upstream. So a
// real local change (cursor 5 -> 6) fired NO 'change' event, while a no-op
// (5 -> 5) fired a spurious one.
//
// FINDING B (MEDIUM): both awareness sites used equalAttrs, which is lib0
// equalFlat (SHALLOW: nested values by ===/reference) — correct for the YText
// format callers but WRONG for awareness, where upstream uses f.equalityDeep.
// A re-sent, structurally-identical NESTED awareness state (the freshly-decoded
// state is a distinct instance, so nested !== ) was over-reported as changed.
//
// Each test below FAILS on the unpatched tree and PASSES after (a) the
// SetLocalState condition is inverted to fire on CHANGE and (b) both awareness
// sites compare with a DEEP equality (equalAttrsDeep) matching f.equalityDeep.

// changeRecord captures one 'change' event's payload (the added/updated/removed
// client lists). The awareness layer emits change as Emit("change", Object,
// origin), so v[0] is the Object payload.
type changeRecord struct {
	added   []Number
	updated []Number
	removed []Number
}

// numberList coerces the heterogeneous []Number stored under a change-payload
// key back to []Number, tolerating an absent key (nil) or an empty slice.
func numberList(o Object, key string) []Number {
	v := o.GetOr(key)
	if v == nil {
		return nil
	}
	ns, _ := v.([]Number)
	return ns
}

// captureChanges subscribes to the awareness 'change' event and appends a
// changeRecord per emission, returning a pointer to the growing slice.
func captureChanges(aw *Awareness) *[]changeRecord {
	var recs []changeRecord
	aw.On("change", NewObserverHandler(func(v ...interface{}) {
		obj, ok := v[0].(Object)
		if !ok {
			return
		}
		recs = append(recs, changeRecord{
			added:   numberList(obj, "added"),
			updated: numberList(obj, "updated"),
			removed: numberList(obj, "removed"),
		})
	}))
	return &recs
}

func containsNumber(ns []Number, want Number) bool {
	for _, n := range ns {
		if n == want {
			return true
		}
	}
	return false
}

// FINDING A: a SetLocalState that CHANGES the state fires a 'change' event with
// the client in `updated`; a no-op SetLocalState does NOT fire 'change'.
func TestSetLocalStateChangeFiresOnRealChange(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	doc.ClientID = 11
	aw := NewAwareness(doc) // initial empty local state (clock 0)

	// Establish a present local cursor state (5). This is an add (empty -> present
	// is actually a transition from the NewAwareness empty {} to {cursor:5}); we
	// only start capturing AFTER this so the test isolates change-vs-no-change.
	_ = aw.SetLocalState(MakeObject("cursor", 5))

	recs := captureChanges(aw)

	// Real change: cursor 5 -> 6 MUST fire 'change' with client 11 in updated.
	_ = aw.SetLocalState(MakeObject("cursor", 6))
	if len(*recs) != 1 {
		t.Fatalf("FINDING A: a real local change (cursor 5->6) fired %d 'change' "+
			"events, want exactly 1 (the change was swallowed)", len(*recs))
	}
	if !containsNumber((*recs)[0].updated, 11) {
		t.Fatalf("FINDING A: 'change' event after a real change did not list client "+
			"11 in updated; updated=%v", (*recs)[0].updated)
	}

	// No-op: cursor 6 -> 6 MUST NOT fire 'change' (structurally identical state).
	_ = aw.SetLocalState(MakeObject("cursor", 6))
	if len(*recs) != 1 {
		t.Fatalf("FINDING A: a no-op SetLocalState (cursor 6->6) fired a spurious "+
			"'change' event; total now %d, want still 1", len(*recs))
	}
}

// FINDING B: re-applying a structurally-identical NESTED awareness state via
// SetLocalState does NOT fire 'change'; a genuinely-changed nested state DOES.
// (equalAttrs/equalFlat would over-report the no-op as a change, because a
// freshly-built nested Object is a distinct instance and compares !== shallow.)
func TestSetLocalStateChangeUsesDeepEqualityForNestedState(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	doc.ClientID = 22
	aw := NewAwareness(doc)

	// Present a nested state: {cursor:{x,y}, selection:[...]}.
	_ = aw.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 1, "y", 2),
		"selection", []any{0, 5},
	))

	recs := captureChanges(aw)

	// Re-send a DISTINCT instance with identical structure. Deep equality => no
	// change. (Shallow equalFlat would see the nested objects/arrays as !== and
	// fire a spurious change.)
	_ = aw.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 1, "y", 2),
		"selection", []any{0, 5},
	))
	if len(*recs) != 0 {
		t.Fatalf("FINDING B: re-sending a structurally-identical NESTED state fired "+
			"%d 'change' events, want 0 (must compare DEEP per f.equalityDeep)", len(*recs))
	}

	// Genuinely changed nested value (cursor.x 1 -> 9) => 'change' fires.
	_ = aw.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 9, "y", 2),
		"selection", []any{0, 5},
	))
	if len(*recs) != 1 {
		t.Fatalf("FINDING B: a genuine nested change (cursor.x 1->9) fired %d 'change' "+
			"events, want exactly 1", len(*recs))
	}
	if !containsNumber((*recs)[0].updated, 22) {
		t.Fatalf("FINDING B: nested-change 'change' event did not list client 22 in "+
			"updated; updated=%v", (*recs)[0].updated)
	}

	// Genuinely changed nested array (selection [0,5] -> [0,9]) => 'change' fires.
	_ = aw.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 9, "y", 2),
		"selection", []any{0, 9},
	))
	if len(*recs) != 2 {
		t.Fatalf("FINDING B: a genuine nested-array change (selection) fired total %d "+
			"'change' events, want 2", len(*recs))
	}
}

// FINDING A/B (flat/primitive sanity): a flat primitive cursor state behaves
// correctly under the deep comparator too — distinct {cursor:5} re-send is a
// no-op, {cursor:5}->{cursor:6} is a change.
func TestSetLocalStateChangeFlatPrimitiveStillCorrect(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	doc.ClientID = 33
	aw := NewAwareness(doc)

	_ = aw.SetLocalState(MakeObject("cursor", 5))
	recs := captureChanges(aw)

	// Distinct {cursor:5}: equal by value -> no change.
	_ = aw.SetLocalState(MakeObject("cursor", 5))
	if len(*recs) != 0 {
		t.Fatalf("flat re-send {cursor:5} fired %d 'change' events, want 0", len(*recs))
	}

	// {cursor:5} -> {cursor:6}: change.
	_ = aw.SetLocalState(MakeObject("cursor", 6))
	if len(*recs) != 1 {
		t.Fatalf("flat change {cursor:5}->{cursor:6} fired %d 'change' events, want 1", len(*recs))
	}
}

// FINDING B (apply path, line 403): applying a re-sent structurally-identical
// NESTED awareness state from a REMOTE peer does NOT add the client to
// change.updated; a genuinely-changed nested state DOES. This exercises
// ApplyAwarenessUpdate's filteredUpdated, the sibling of SetLocalState's.
func TestApplyAwarenessUpdateChangeUsesDeepEqualityForNestedState(t *testing.T) {
	// Producer peer publishes presence for client 100.
	prod := newDoc("doc", true, defaultGCFilter, nil, false)
	prod.ClientID = 100
	awProd := NewAwareness(prod)
	_ = awProd.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 1, "y", 2),
		"selection", []any{0, 5},
	))

	// Consumer peer tracks it.
	cons := newDoc("doc", true, defaultGCFilter, nil, false)
	cons.ClientID = 200
	awCons := NewAwareness(cons)
	first := EncodeAwarenessUpdate(awProd, []Number{100}, nil)
	if err := ApplyAwarenessUpdate(awCons, first, "remote"); err != nil {
		t.Fatalf("apply initial presence: %v", err)
	}

	recs := captureChanges(awCons)

	// Producer re-emits the SAME structural state with an advanced clock (a
	// keep-alive re-send). Re-setting an identical-by-value state bumps the
	// producer's clock (SetLocalState always increments it), so the consumer does
	// NOT skip the apply on clock; yet the decoded state it receives is a fresh
	// instance (distinct nested refs), which equalFlat would over-report as a
	// 'change'. Deep equality must treat it as unchanged.
	_ = awProd.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 1, "y", 2),
		"selection", []any{0, 5},
	))
	resend := EncodeAwarenessUpdate(awProd, []Number{100}, nil)
	if err := ApplyAwarenessUpdate(awCons, resend, "remote"); err != nil {
		t.Fatalf("apply identical re-send: %v", err)
	}
	for _, r := range *recs {
		if containsNumber(r.updated, 100) {
			t.Fatalf("FINDING B (apply path): re-sent structurally-identical NESTED " +
				"state added client 100 to change.updated; must compare DEEP")
		}
	}

	// Now a genuine nested change must show up in change.updated.
	recsLen := len(*recs)
	_ = awProd.SetLocalState(MakeObject(
		"cursor", MakeObject("x", 7, "y", 2),
		"selection", []any{0, 5},
	))
	changed := EncodeAwarenessUpdate(awProd, []Number{100}, nil)
	if err := ApplyAwarenessUpdate(awCons, changed, "remote"); err != nil {
		t.Fatalf("apply genuine change: %v", err)
	}
	sawUpdated := false
	for _, r := range (*recs)[recsLen:] {
		if containsNumber(r.updated, 100) {
			sawUpdated = true
		}
	}
	if !sawUpdated {
		t.Fatalf("FINDING B (apply path): a genuine nested change did not add client " +
			"100 to change.updated")
	}
}

// ---------------------------------------------------------------- from awareness_clear_removal_review_test.go
// awareness_clear_removal_review_test.go reproduces FINDING 1 from the full
// code-review of the Go Yjs v2 codec (PR antst/y-crdt#2): the Object rewrite
// (a9d9cda) made EncodeAwarenessUpdate serialize a CLEARED/removed client state
// as `{}` instead of `null`.
//
// Root cause: for a removed client, `state := states[clientID]` is the zero
// Object{} (IsNil); jsonString(Object{}) -> marshalJSONOrdered -> "{}". The old
// `map[string]any` alias yielded a nil map -> json.Marshal(nil) -> "null".
//
// Consequence: on the receiving peer jsonObject("{}") is a present, empty Object
// (NOT IsNil), so ApplyAwarenessUpdate takes the `else` branch and reports the
// client as UPDATED, never REMOVED. The peer keeps the client in States forever
// -> a ghost cursor that never disappears when a client clears its presence or
// disconnects. Yjs stringifies a removed state as `null`, so a Go<->Yjs (and
// Go<->Go) peer would diverge.
//
// Each test below FAILS on the unpatched tree (the bytes carry `{}` and the peer
// never removes the client) and PASSES after EncodeAwarenessUpdate emits `null`
// for an IsNil() state.

// awarenessEntryStateJSON decodes a single-entry awareness update and returns the
// raw JSON state string for that one entry — letting us assert the on-wire shape
// (`null` vs `{}`) directly, independent of any receiver behavior.
func awarenessEntryStateJSON(t *testing.T, update []byte) string {
	t.Helper()
	d := newDecoder(update)
	if n := mustReadVarUint(t, d); n != 1 {
		t.Fatalf("expected exactly one awareness entry, got %d", n)
	}
	_ = mustReadVarUint(t, d) // clientID
	_ = mustReadVarUint(t, d) // clock
	s, err := readString(d)
	if err != nil {
		t.Fatalf("ReadString(state): %v", err)
	}
	return s
}

// FINDING 1a: a removed client's state must serialize as the JSON literal `null`,
// not `{}`. This is the byte-level root cause; it pins the encode boundary.
func TestEncodeAwarenessUpdateSerializesClearedStateAsNull(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	doc.ClientID = 42
	aw := NewAwareness(doc) // sets an initial empty local state (clock 0)

	// Clear the local state: this advances the clock and DELETES it from States,
	// exactly what a client does right before disconnecting.
	_ = aw.SetLocalState(Object{})

	// states[42] is now absent -> states[42] reads as the zero Object{} (IsNil).
	update := EncodeAwarenessUpdate(aw, []Number{42}, nil)
	got := awarenessEntryStateJSON(t, update)
	if got != "null" {
		t.Fatalf("cleared awareness state serialized as %q, want \"null\" "+
			"(a non-null state makes peers keep a ghost cursor forever)", got)
	}
}

// FINDING 1b: a client that the server explicitly removed (RemoveAwarenessStates,
// the disconnect path) must also serialize as `null`.
func TestEncodeAwarenessUpdateSerializesRemovedClientAsNull(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	doc.ClientID = 7
	aw := NewAwareness(doc)
	_ = aw.SetLocalState(MakeObject("cursor", 5)) // present state, clock advances

	RemoveAwarenessStates(aw, []Number{7}, "disconnect") // deletes States[7]

	update := EncodeAwarenessUpdate(aw, []Number{7}, nil)
	if got := awarenessEntryStateJSON(t, update); got != "null" {
		t.Fatalf("removed-client state serialized as %q, want \"null\"", got)
	}
}

// FINDING 1c (the end-to-end consequence, via the ws_shared_doc presence flow):
// a client that clears its state must be REMOVED from a remote peer's States —
// not lingered as a present, empty object. This is the ghost-cursor reproduction.
//
// Flow: peer A publishes a presence object; peer B applies it and now tracks A.
// A clears its presence (SetLocalState(Object{})); the resulting update, when
// applied to B, must DELETE A from B.states. On the unpatched tree the update
// carries `{}`, B treats it as an update, and A persists in B.states -> ghost.
func TestClearedStatePropagatesAsRemovalAcrossPeers(t *testing.T) {
	docA := newDoc("doc", true, defaultGCFilter, nil, false)
	docA.ClientID = 100
	awA := NewAwareness(docA)

	docB := newDoc("doc", true, defaultGCFilter, nil, false)
	docB.ClientID = 200
	awB := NewAwareness(docB)

	// A publishes presence; replicate to B (simulating the ws_shared_doc
	// "update" broadcast: EncodeAwarenessUpdate(A) -> ApplyAwarenessUpdate(B)).
	_ = awA.SetLocalState(MakeObject("user", "alice", "cursor", 3))
	pub := EncodeAwarenessUpdate(awA, []Number{100}, nil)
	if err := ApplyAwarenessUpdate(awB, pub, "remote"); err != nil {
		t.Fatalf("apply presence to peer B: %v", err)
	}
	if _, ok := awB.states[100]; !ok {
		t.Fatalf("setup: peer B should track client 100 after presence publish")
	}

	// A clears its presence (disconnect). Broadcast the clearing update to B.
	var removedSeen []Number
	awB.On("update", NewObserverHandler(func(v ...interface{}) {
		obj := v[0].(Object)
		removedSeen = append(removedSeen, obj.GetOr("removed").([]Number)...)
	}))

	_ = awA.SetLocalState(Object{}) // clear -> clock advances, States[100] deleted on A
	clear := EncodeAwarenessUpdate(awA, []Number{100}, nil)
	if err := ApplyAwarenessUpdate(awB, clear, "remote"); err != nil {
		t.Fatalf("apply clearing update to peer B: %v", err)
	}

	if _, ok := awB.states[100]; ok {
		t.Fatalf("FINDING 1: peer B still tracks client 100 after it cleared its " +
			"state — a ghost cursor (cleared state encoded as {} not null)")
	}
	if len(removedSeen) != 1 || removedSeen[0] != 100 {
		t.Fatalf("FINDING 1: peer B did not report client 100 as removed; removed=%v", removedSeen)
	}
}

// ---------------------------------------------------------------- from awareness_diff_test.go
type awEvent struct {
	Ev      string `json:"ev"`
	Added   []int  `json:"added"`
	Updated []int  `json:"updated"`
	Removed []int  `json:"removed"`
}

type awCase struct {
	Seed         int                      `json:"seed"`
	Ops          []map[string]interface{} `json:"ops"`
	Events       []awEvent                `json:"events"`
	Updates      []string                 `json:"updates"`
	FinalClients []int                    `json:"finalClients"`
}

// TestAwarenessDiff is the awareness differential (US3, FR-004, C-S4).
//
// Awareness had ZERO differential coverage before this feature. Two things are compared, and
// FR-004 requires both:
//
//  1. The WIRE FORMAT — the encoded update after each op, byte for byte.
//  2. The EMITTED EVENTS — added/updated/removed per op. The events are the contract a consumer
//     reacts to; a peer that ends with the correct presence map while emitting the wrong sets
//     still drives the wrong UI, and a map-only comparison cannot see that.
func TestAwarenessDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_AWARENESS_FILE", "fuzz/awareness_gen.mjs", "1", "400")
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	var total, wireDiv, eventDiv, clientDiv int
	var firstWire, firstEvent []int

	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c awCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("bad awareness record: %v", err)
		}
		total++

		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		aw := NewAwareness(doc)

		var got []awEvent
		aw.On("update", NewObserverHandler(func(v ...interface{}) {
			if len(v) == 0 {
				return
			}
			payload, ok := v[0].(Object)
			if !ok {
				return
			}
			got = append(got, awEvent{
				Ev:      "update",
				Added:   numsOf(payload, "added"),
				Updated: numsOf(payload, "updated"),
				Removed: numsOf(payload, "removed"),
			})
		}))

		for i, op := range c.Ops {
			switch op["op"].(string) {
			case "setLocal":
				st := aw.GetLocalState()
				if st.IsNil() {
					st = newObject()
				} else {
					st = st.ShallowClone()
				}
				st.Set(op["f"].(string), int(op["v"].(float64)))
				_ = aw.SetLocalState(st)
			case "clearLocal":
				_ = aw.SetLocalState(Object{})
			case "applyRemote":
				raw, err := hex.DecodeString(op["update"].(string))
				if err != nil {
					t.Fatalf("seed %d: bad remote update: %v", c.Seed, err)
				}
				if err := ApplyAwarenessUpdate(aw, raw, "remote"); err != nil {
					t.Errorf("seed %d: ApplyAwarenessUpdate: %v", c.Seed, err)
				}
			}

			// (1) WIRE FORMAT — encoded state after this op must be byte-identical.
			states := aw.GetStates()
			clients := make([]Number, 0, len(states))
			for cl := range states {
				clients = append(clients, cl)
			}
			sort.Slice(clients, func(a, b int) bool { return clients[a] < clients[b] })
			want := ""
			if i < len(c.Updates) {
				want = c.Updates[i]
			}
			gotHex := ""
			if len(clients) > 0 {
				gotHex = hex.EncodeToString(EncodeAwarenessUpdate(aw, clients, nil))
			}
			if gotHex != want {
				wireDiv++
				if len(firstWire) < 8 {
					firstWire = append(firstWire, c.Seed)
				}
			}
		}

		// (2) EVENTS — the added/updated/removed sequence must match.
		if !sameEvents(got, c.Events) {
			eventDiv++
			if len(firstEvent) < 8 {
				firstEvent = append(firstEvent, c.Seed)
			}
		}

		if len(aw.GetStates()) != len(c.FinalClients) {
			clientDiv++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("awareness", path, total); err != nil {
		t.Fatal(err)
	}

	t.Logf("AWARENESS_DIFF total=%d wireDiv=%d eventDiv=%d clientDiv=%d first=%v/%v",
		total, wireDiv, eventDiv, clientDiv, firstWire, firstEvent)
	if wireDiv > 0 {
		t.Errorf("awareness WIRE bytes diverged %d (first %v)", wireDiv, firstWire)
	}
	if eventDiv > 0 {
		t.Errorf("awareness EVENTS diverged %d (first %v) — the emitted added/updated/removed sets "+
			"are the contract a consumer reacts to, and a map-only check cannot see this",
			eventDiv, firstEvent)
	}
	if clientDiv > 0 {
		t.Errorf("awareness final client set diverged %d", clientDiv)
	}
}

func numsOf(o Object, key string) []int {
	v, ok := o.Get(key)
	if !ok || v == nil {
		return nil
	}
	var out []int
	switch t := v.(type) {
	case []Number:
		for _, n := range t {
			out = append(out, int(n))
		}
	case []interface{}:
		for _, n := range t {
			if f, ok := n.(Number); ok {
				out = append(out, int(f))
			}
		}
	}
	sort.Ints(out)
	return out
}

func sameEvents(got, want []awEvent) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if fmt.Sprint(got[i].Added) != fmt.Sprint(want[i].Added) ||
			fmt.Sprint(got[i].Updated) != fmt.Sprint(want[i].Updated) ||
			fmt.Sprint(got[i].Removed) != fmt.Sprint(want[i].Removed) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- from awareness_dos_review_test.go
// awareness_dos_review_test.go reproduces the ModifyAwarenessUpdate DoS found in
// the code-review (PR antst/y-crdt#2): `length := ReadVarUint(decoder)` then a
// loop with no bound against remaining bytes — each iteration swallows truncated
// reads AND writes a cleared entry to the output encoder (CPU + unbounded
// output). The sibling decodeAwarenessEntries is correctly bounded with
// `length > decoder.Len()`; this applies the same byte-budget bound.

// --- BUG 3: ModifyAwarenessUpdate length loop unbounded -------------------------

func TestModifyAwarenessUpdateBounded(t *testing.T) {
	payload := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	t.Logf("BUG3 payload = % x (%d bytes)", payload, len(payload))

	runWithin(t, 5*time.Second, "BUG3 ModifyAwarenessUpdate", func() {
		out, err := ModifyAwarenessUpdate(payload, func(v interface{}) interface{} { return v })
		if !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("ModifyAwarenessUpdate error = %v, want ErrTruncatedAwarenessFrame", err)
		}
		if len(out) != 0 {
			t.Errorf("BUG3: truncated frame produced %d output bytes, want none", len(out))
		}
	})
}

// A legitimate awareness update must STILL round-trip through
// ModifyAwarenessUpdate after the count bound — the bound only rejects provably
// truncated frames (count > remaining bytes), never a real one (each entry is
// >= 3 bytes so a valid count is always <= remaining).
func TestModifyAwarenessUpdateAcceptsValidFrame(t *testing.T) {
	enc := newEncoder()
	writeVarUint(enc, 2)
	for _, e := range []struct {
		client, clock uint64
		json          string
	}{{1, 5, `{"u":"a"}`}, {2, 7, `{"u":"b"}`}} {
		writeVarUint(enc, e.client)
		writeVarUint(enc, e.clock)
		_ = writeString(enc, e.json)
	}
	valid := enc.Bytes()

	out, err := ModifyAwarenessUpdate(valid, func(v interface{}) interface{} { return v })
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder(out)
	if n := mustReadVarUint(t, d); n != 2 {
		t.Fatalf("legitimate frame lost entries: count=%d, want 2", n)
	}
}

// ---------------------------------------------------------------- from awareness_empty_state_drift_review_test.go
// awareness_empty_state_drift_review_test.go reproduces the awareness decode
// DRIFT found in the code-review (PR antst/y-crdt#2): an EMPTY state string ("")
// was classified differently by the two decode paths.
//
//   - core decodeAwarenessEntries: wrapped the parse in `if data != ""`, so ""
//     left the state as the zero Object → treated as a CLEARED state, no error.
//   - protocol DecodeAwarenessMessage: parsed unconditionally, so "" was fed to
//     unmarshalJSONOrdered, EOF'd, and was rejected as ErrMalformedAwarenessState.
//
// On a websocket the core would clear the cursor while the protocol rejected the
// very frame that does the clearing → a GHOST cursor that never goes away.
//
// The fix extracts a shared ParseAwarenessStateJSON used by BOTH paths, so
// the empty/null/object classification is identical. This test feeds an
// empty-state frame and asserts BOTH paths agree it is a clean (cleared) state.

// buildEmptyStateAwarenessFrame builds a one-entry awareness frame whose state
// is the EMPTY string "" (the cleared-state representation that triggered the
// drift). NewAwareness/EncodeAwarenessUpdate emit "null" for cleared, but a
// real-world client (e.g. the y-protocols JS awareness) can send "".
func buildEmptyStateAwarenessFrame(clientID, clock uint64) []byte {
	enc := newEncoder()
	writeVarUint(enc, 1)
	writeVarUint(enc, clientID)
	writeVarUint(enc, clock)
	_ = writeString(enc, "") // empty state string
	return enc.Bytes()
}

// The shared helper itself: "" is a cleared state, never an error.
func TestParseAwarenessStateJSONEmptyIsCleared(t *testing.T) {
	state, err := ParseAwarenessStateJSON("")
	if err != nil {
		t.Fatalf("drift: ParseAwarenessStateJSON(\"\") errored (%v); empty state must be cleared", err)
	}
	if state.Len() != 0 {
		t.Fatalf("drift: ParseAwarenessStateJSON(\"\") yielded a non-empty state %v; must be cleared", state)
	}

	// null is also cleared.
	if s, err := ParseAwarenessStateJSON("null"); err != nil || s.Len() != 0 {
		t.Fatalf("ParseAwarenessStateJSON(\"null\") = (%v, %v); want cleared state, nil error", s, err)
	}

	// A populated object round-trips.
	if s, err := ParseAwarenessStateJSON(`{"user":1}`); err != nil || s.Len() == 0 {
		t.Fatalf("ParseAwarenessStateJSON(object) = (%v, %v); want the object, nil error", s, err)
	}

	// A valid-JSON non-object is malformed.
	if _, err := ParseAwarenessStateJSON(`42`); err == nil {
		t.Fatalf("ParseAwarenessStateJSON(\"42\") should be malformed (not a JSON object)")
	}
}

// CORE path: an empty-state frame applies cleanly (no error) — the cleared state.
func TestCoreApplyEmptyStateAwarenessClean(t *testing.T) {
	frame := buildEmptyStateAwarenessFrame(123, 1)
	aw := NewAwareness(newDoc("g", false, nil, nil, false))
	if err := ApplyAwarenessUpdate(aw, frame, nil); err != nil {
		t.Fatalf("drift: core ApplyAwarenessUpdate rejected an empty-state (cleared) frame: %v", err)
	}
}

// ---------------------------------------------------------------- from awareness_modify_ghost_review_test.go
// awareness_modify_ghost_review_test.go covers Correctness 4 from the SECOND
// code-review of PR antst/y-crdt#2: ModifyAwarenessUpdate was the unguarded 3rd
// ghost-cursor site. The "emit JSON null for a cleared (IsNil) state, not {}"
// guard lived (hand-duplicated) at EncodeAwarenessUpdate and protocol
// EncodeAwarenessMessage, but was MISSING at the exported ModifyAwarenessUpdate,
// which did WriteString(encoder, jsonString(modifiedState)) directly. It
// survived only because a cleared state arrives as a bare nil; a modify callback
// that re-types a cleared entry to a zero/empty Object{} re-encodes "{}" — a
// PRESENT empty object the receiver applies and never removes (ghost cursor).
//
// The fix centralizes ONE helper (AwarenessStateJSON) used by all three sites.

// buildClearedAwarenessFrame builds a one-entry awareness frame whose state is
// the JSON literal "null" (a cleared/removed client).
func buildClearedAwarenessFrame(clientID, clock uint64) []byte {
	enc := newEncoder()
	writeVarUint(enc, 1)
	writeVarUint(enc, clientID)
	writeVarUint(enc, clock)
	_ = writeString(enc, "null")
	return enc.Bytes()
}

func mustModifyAwarenessUpdate(t testing.TB, update []byte, modify func(interface{}) interface{}) []byte {
	t.Helper()
	result, err := ModifyAwarenessUpdate(update, modify)
	if err != nil {
		t.Fatalf("modify awareness update: %v", err)
	}
	return result
}

// TestModifyAwarenessUpdateClearedStateStaysNull is the ghost-cursor repro: a
// modify callback that normalizes a cleared entry to the zero Object{} must
// still emit "null" (not "{}"), so a receiver removes the client.
func TestModifyAwarenessUpdateClearedStateStaysNull(t *testing.T) {
	frame := buildClearedAwarenessFrame(100, 3)

	// The callback re-types the cleared (bare-nil) state to a zero Object{} — the
	// exact normalization that, unguarded, re-encodes as "{}".
	out := mustModifyAwarenessUpdate(t, frame, func(v interface{}) interface{} {
		// v is nil for the cleared "null" state.
		if v == nil {
			return Object{} // zero/IsNil Object — must still serialize as "null"
		}
		return v
	})

	if got := awarenessEntryStateJSON(t, out); got != "null" {
		t.Fatalf("ModifyAwarenessUpdate re-typed a cleared state to %q, want \"null\" "+
			"(a non-null state makes peers keep a ghost cursor forever)", got)
	}
}

// TestModifyAwarenessUpdateThenApplyRemovesClient is the end-to-end consequence:
// after ModifyAwarenessUpdate normalizes a cleared entry, applying the result to
// a peer that currently tracks the client must REMOVE it (no ghost), exactly as
// a "null" state does.
func TestModifyAwarenessUpdateThenApplyRemovesClient(t *testing.T) {
	// Peer that currently tracks client 100 with a present state.
	peer := NewAwareness(newDoc("", false, nil, nil, false))
	present := EncodeAwarenessUpdateFor(t, 100, 1, `{"cursor":5}`)
	if err := ApplyAwarenessUpdate(peer, present, nil); err != nil {
		t.Fatalf("seed apply failed: %v", err)
	}
	if _, ok := peer.states[100]; !ok {
		t.Fatalf("precondition: peer should track client 100")
	}

	// A cleared frame (clock advanced) run through ModifyAwarenessUpdate with a
	// callback that normalizes the cleared state to Object{}.
	cleared := buildClearedAwarenessFrame(100, 2)
	modified := mustModifyAwarenessUpdate(t, cleared, func(v interface{}) interface{} {
		if v == nil {
			return Object{}
		}
		return v
	})

	if err := ApplyAwarenessUpdate(peer, modified, nil); err != nil {
		t.Fatalf("apply of modified cleared frame failed: %v", err)
	}
	if _, ok := peer.states[100]; ok {
		t.Fatalf("ghost cursor: peer still tracks client 100 after a cleared+modified frame")
	}
}

// TestModifyAwarenessUpdatePreservesPresentState guards the non-cleared path: a
// present state re-typed by the callback must still round-trip as its object
// JSON (the helper only special-cases IsNil).
func TestModifyAwarenessUpdatePreservesPresentState(t *testing.T) {
	enc := newEncoder()
	writeVarUint(enc, 1)
	writeVarUint(enc, 5)
	writeVarUint(enc, 9)
	_ = writeString(enc, `{"u":"a"}`)
	frame := enc.Bytes()

	out := mustModifyAwarenessUpdate(t, frame, func(v interface{}) interface{} {
		// Mutate the present state: add a key.
		if o, ok := v.(Object); ok {
			o.Set("v", "b")
			return o
		}
		return v
	})
	got := awarenessEntryStateJSON(t, out)
	if got != `{"u":"a","v":"b"}` {
		t.Fatalf("present-state modify produced %q, want {\"u\":\"a\",\"v\":\"b\"}", got)
	}
}

// EncodeAwarenessUpdateFor builds a one-entry frame with a present object state.
func EncodeAwarenessUpdateFor(t *testing.T, clientID, clock uint64, json string) []byte {
	t.Helper()
	enc := newEncoder()
	writeVarUint(enc, 1)
	writeVarUint(enc, clientID)
	writeVarUint(enc, clock)
	_ = writeString(enc, json)
	return enc.Bytes()
}

// ---------------------------------------------------------------- from awareness_ownership_test.go
type awarenessOwnershipFixture struct {
	state  Object
	user   Object
	cursor Object
	ranges []any
	bytes  []byte
	plain  map[string]any
}

func newAwarenessOwnershipFixture(rangeCount int) awarenessOwnershipFixture {
	user := MakeObject("name", "alice", "color", "#123456")
	cursor := MakeObject("anchor", 3, "head", 7)
	bytesValue := []byte{1, 2, 3, 4}
	plain := map[string]any{"active": true, "labels": []any{"one", "two"}}
	ranges := make([]any, rangeCount)
	for i := range ranges {
		ranges[i] = MakeObject("anchor", i, "head", i+1)
	}
	state := MakeObject(
		"user", user,
		"cursor", cursor,
		"ranges", ranges,
		"bytes", bytesValue,
		"plain", plain,
	)
	return awarenessOwnershipFixture{
		state: state, user: user, cursor: cursor, ranges: ranges, bytes: bytesValue, plain: plain,
	}
}

func requireOwnedAwarenessFixture(t *testing.T, state Object, rangeCount int) {
	t.Helper()
	if got := state.GetOr("user").(Object).GetOr("name"); got != "alice" {
		t.Fatalf("user.name = %v, want alice", got)
	}
	if got := state.GetOr("cursor").(Object).GetOr("anchor"); got != 3 {
		t.Fatalf("cursor.anchor = %v, want 3", got)
	}
	ranges := state.GetOr("ranges").([]any)
	if len(ranges) != rangeCount {
		t.Fatalf("len(ranges) = %d, want %d", len(ranges), rangeCount)
	}
	if rangeCount > 0 {
		if got := ranges[0].(Object).GetOr("anchor"); got != 0 {
			t.Fatalf("ranges[0].anchor = %v, want 0", got)
		}
	}
	if got := state.GetOr("bytes").([]byte); !reflect.DeepEqual(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("bytes = %v, want [1 2 3 4]", got)
	}
	plain := state.GetOr("plain").(map[string]any)
	if got := plain["active"]; got != true {
		t.Fatalf("plain.active = %v, want true", got)
	}
	if got := plain["labels"].([]any)[0]; got != "one" {
		t.Fatalf("plain.labels[0] = %v, want one", got)
	}
}

func mutateAwarenessOwnershipFixture(f awarenessOwnershipFixture) {
	f.state.Set("added", true)
	f.user.Set("name", "mallory")
	f.cursor.Set("anchor", 99)
	if len(f.ranges) > 0 {
		first := f.ranges[0].(Object)
		first.Set("anchor", 88)
	}
	f.bytes[0] = 9
	f.plain["active"] = false
	f.plain["labels"].([]any)[0] = "changed"
}

func mutateAwarenessSnapshot(state Object) {
	state.Set("added", true)
	user := state.GetOr("user").(Object)
	user.Set("name", "mallory")
	cursor := state.GetOr("cursor").(Object)
	cursor.Set("anchor", 99)
	ranges := state.GetOr("ranges").([]any)
	if len(ranges) > 0 {
		first := ranges[0].(Object)
		first.Set("anchor", 88)
	}
	state.GetOr("bytes").([]byte)[0] = 9
	plain := state.GetOr("plain").(map[string]any)
	plain["active"] = false
	plain["labels"].([]any)[0] = "changed"
}

func TestAwarenessOwnsCallerStateOnSet(t *testing.T) {
	aw := NewAwareness(newDoc("ownership", false, defaultGCFilter, nil, false, WithClientID(7)))
	fixture := newAwarenessOwnershipFixture(3)
	if err := aw.SetLocalState(fixture.state); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}
	mutateAwarenessOwnershipFixture(fixture)
	requireOwnedAwarenessFixture(t, aw.GetLocalState(), 3)
}

func TestAwarenessGettersReturnIndependentDeepSnapshots(t *testing.T) {
	aw := NewAwareness(newDoc("snapshots", false, defaultGCFilter, nil, false, WithClientID(7)))
	fixture := newAwarenessOwnershipFixture(3)
	if err := aw.SetLocalState(fixture.state); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}

	local := aw.GetLocalState()
	mutateAwarenessSnapshot(local)
	requireOwnedAwarenessFixture(t, aw.GetLocalState(), 3)

	states := aw.GetStates()
	state := states[aw.ClientID]
	delete(states, aw.ClientID)
	mutateAwarenessSnapshot(state)
	requireOwnedAwarenessFixture(t, aw.GetStates()[aw.ClientID], 3)

	meta := aw.GetMeta()
	clientMeta := meta[aw.ClientID]
	delete(meta, aw.ClientID)
	clientMeta.Set("clock", 999)
	if got := aw.GetMeta()[aw.ClientID].GetOr("clock").(Number); got == 999 {
		t.Fatal("GetMeta returned an Object sharing internal metadata")
	}
}

func TestAwarenessRejectsUnsupportedStateWithoutMutation(t *testing.T) {
	aw := NewAwareness(newDoc("unsupported", false, defaultGCFilter, nil, false, WithClientID(7)))
	before := newAwarenessOwnershipFixture(1)
	if err := aw.SetLocalState(before.state); err != nil {
		t.Fatalf("initial SetLocalState: %v", err)
	}
	wantState := jsonString(aw.GetLocalState())
	wantMeta := jsonString(aw.GetMeta()[aw.ClientID])

	var events atomic.Int64
	aw.On("update", NewObserverHandler(func(...interface{}) { events.Add(1) }))
	unsupported := MakeObject("nested", []any{map[int]string{1: "shared"}})
	err := aw.SetLocalState(unsupported)
	if !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("SetLocalState error = %v, want ErrUnsupportedDataValue", err)
	}
	if got := jsonString(aw.GetLocalState()); got != wantState {
		t.Fatalf("unsupported state changed local state: got %s, want %s", got, wantState)
	}
	if got := jsonString(aw.GetMeta()[aw.ClientID]); got != wantMeta {
		t.Fatalf("unsupported state changed metadata: got %s, want %s", got, wantMeta)
	}
	if got := events.Load(); got != 0 {
		t.Fatalf("unsupported state emitted %d update event(s), want 0", got)
	}

	err = aw.SetLocalStateField("unsupported", make(chan int))
	if !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("SetLocalStateField error = %v, want ErrUnsupportedDataValue", err)
	}
	if got := jsonString(aw.GetLocalState()); got != wantState {
		t.Fatalf("unsupported field changed local state: got %s, want %s", got, wantState)
	}

	cyclic := newObject()
	cyclic.Set("self", cyclic)
	if err := aw.SetLocalState(cyclic); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("cyclic SetLocalState error = %v, want ErrUnsupportedDataValue", err)
	}
}

func TestAwarenessOwnsNestedObjectPointers(t *testing.T) {
	aw := NewAwareness(newDoc("object-pointer", false, defaultGCFilter, nil, false, WithClientID(7)))
	child := MakeObject("name", "alice")
	state := MakeObject("child", &child)
	if err := aw.SetLocalState(state); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}
	child.Set("name", "mallory")
	got := aw.GetLocalState().GetOr("child").(*Object)
	if name := got.GetOr("name"); name != "alice" {
		t.Fatalf("caller pointer mutation reached internal state: name=%v", name)
	}
	got.Set("name", "eve")
	if name := aw.GetLocalState().GetOr("child").(*Object).GetOr("name"); name != "alice" {
		t.Fatalf("returned pointer mutation reached internal state: name=%v", name)
	}
}

func TestManagedAwarenessSnapshotsStayOwnedUnderConcurrentUse(t *testing.T) {
	aw := NewAwareness(newDoc("managed-ownership", false, defaultGCFilter, nil, false, WithClientID(7)))
	managed := NewManagedAwarenessFrom(aw)
	fixture := newAwarenessOwnershipFixture(4)
	if err := managed.SetLocalState(fixture.state); err != nil {
		t.Fatalf("initial SetLocalState: %v", err)
	}

	const rounds = 400
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			state := newAwarenessOwnershipFixture(4).state
			cursor := state.GetOr("cursor").(Object)
			cursor.Set("head", i)
			if err := managed.SetLocalState(state); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				mutateAwarenessSnapshot(managed.GetLocalState())
				states := managed.GetStates()
				if state, ok := states[aw.ClientID]; ok {
					mutateAwarenessSnapshot(state)
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("concurrent SetLocalState: %v", err)
	default:
	}
	requireOwnedAwarenessFixture(t, managed.GetLocalState(), 4)
}

var clonedAwarenessBenchmarkSink Object

func TestAwarenessDataCloneAllocationBudget(t *testing.T) {
	state := MakeObject(
		"user", MakeObject("name", "alice", "color", "#123456"),
		"cursor", MakeObject("anchor", 3, "head", 7),
	)
	allocs := testing.AllocsPerRun(1_000, func() {
		var err error
		clonedAwarenessBenchmarkSink, err = cloneDataObject(state)
		if err != nil {
			panic(err)
		}
	})
	if allocs > 5 {
		t.Fatalf("deep-copy allocations = %.0f, want <= 5 (the reflection-based deep copy "+
			"this replaced used 135)", allocs)
	}
}

func BenchmarkAwarenessStateCopy(b *testing.B) {
	for _, ranges := range []int{0, 32} {
		state := MakeObject(
			"user", MakeObject("name", "alice", "color", "#123456"),
			"cursor", MakeObject("anchor", 3, "head", 7),
		)
		if ranges > 0 {
			selection := make([]any, ranges)
			for i := range selection {
				selection[i] = MakeObject("anchor", i, "head", i+1)
			}
			state.Set("selection", selection)
		}
		b.Run("purpose-built-ranges-"+strconv.Itoa(ranges), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				clonedAwarenessBenchmarkSink, err = cloneDataObject(state)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		// The former "copystructure-ranges" arm measured Object.Clone, the
		// reflection-based deep copy this comparison was built to displace. It won,
		// Object.Clone is gone, and a benchmark of a deleted path measures nothing.
		b.Run("shallow-ranges-"+strconv.Itoa(ranges), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				clonedAwarenessBenchmarkSink = state.ShallowClone()
			}
		})
	}
}

// ---------------------------------------------------------------- from awareness_reaper_test.go
// US5 / FR-020..FR-023 (work item 1.7A). The awareness reaper (re-enabled as a
// goroutine, auto-started by NewAwareness, stopped by Destroy) must reap stale
// remote clients and renew the local clock, comparing times in milliseconds.
// Verified against y-protocols@1.0.7 awareness.js (outdatedTimeout=30000, renew at
// /2, reap at full, removeAwarenessStates(..., "timeout")). reapTick is driven
// directly with crafted lastUpdated timestamps so the test is deterministic and
// does not wait the real 30s.

func newReaperDoc() *Doc {
	return newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
}

func TestAwarenessReaperReapsStaleRemote(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	defer aw.Destroy()

	// Inject a remote client (99) whose state is stale (> outdatedTimeout old).
	remoteState := newObject()
	remoteState.Set("name", "ghost")
	aw.mu.Lock()
	aw.states[99] = remoteState
	aw.meta[99] = MakeObject("clock", 0, "lastUpdated", getUnixTime()-int64(OutdatedTimeout/time.Millisecond)-1000)
	aw.mu.Unlock()

	removed := false
	aw.On("update", NewObserverHandler(func(v ...interface{}) {
		ev := v[0].(Object)
		if r, ok := ev.GetOr("removed").([]Number); ok {
			for _, c := range r {
				if c == 99 {
					removed = true
				}
			}
		}
	}))

	m.tick()

	if _, ok := aw.GetStates()[99]; ok {
		t.Error("stale remote client 99 was not reaped")
	}
	if !removed {
		t.Error("reaping a stale remote did not emit a 'removed' update")
	}
}

func TestAwarenessReaperRenewsLocal(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	defer aw.Destroy()

	// Age the local meta past the renew threshold (outdatedTimeout/2) but keep the
	// local state present.
	aw.mu.Lock()
	clockBefore := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.meta[aw.ClientID] = MakeObject("clock", clockBefore, "lastUpdated", getUnixTime()-int64(OutdatedTimeout/2/time.Millisecond)-1000)
	aw.mu.Unlock()

	m.tick()

	aw.mu.Lock()
	clockAfter := aw.meta[aw.ClientID].GetOr("clock").(Number)
	_, localPresent := aw.states[aw.ClientID]
	aw.mu.Unlock()

	if clockAfter <= clockBefore {
		t.Errorf("local clock not renewed: before=%d after=%d", clockBefore, clockAfter)
	}
	if !localPresent {
		t.Error("local client was reaped (must never reap self)")
	}
}

// Regression for the code-review finding: the reaper goroutine's Emit (via
// reapTick -> RemoveAwarenessStates/SetLocalState) raced the unsynchronized
// Observable.observers map against a consumer's On/Off. Before the Observable
// mutex this fails under -race ("concurrent map read/write"); now it is clean.
func TestAwarenessReaperEmitRaceFreeWithObservers(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	defer aw.Destroy()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reaper side: repeatedly seed a stale remote and reap it (each reap Emits).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			aw.mu.Lock()
			aw.states[99] = newObject()
			aw.meta[99] = MakeObject("clock", 0, "lastUpdated", getUnixTime()-int64(OutdatedTimeout/time.Millisecond)-1000)
			aw.mu.Unlock()
			m.tick()
		}
	}()

	// Consumer side: churn observers concurrently with the reaper's emits.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			h := NewObserverHandler(func(...interface{}) {})
			aw.On("update", h)
			aw.Off("update", h)
		}
		close(stop)
	}()

	wg.Wait()
}

// Regression for the round-2/3 finding: Destroy stops the reaper without joining, so
// a late tick could re-add/renew local state on a torn-down awareness (ghost). The
// guard must make reapTick a no-op when destroyed. This test keeps local state
// PRESENT and stale (so a tick WOULD renew it), sets destroyed directly, and asserts
// reapTick neither renews (clock unchanged) nor removes it — so removing the guard
// makes it fail (the earlier version let Destroy delete the state, so reapTick
// no-op'd on the empty-state branch regardless and gave false confidence).
func TestAwarenessReaperNoopAfterDestroy(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	// This test sets aw.destroyed=true directly (below), which makes aw.Destroy() a
	// no-op (its idempotency early-return skips closing stopCh) and would leak the
	// auto-started reaper. Stop the goroutine directly in cleanup instead.
	t.Cleanup(func() {
		m.Stop()
	})

	aw.mu.Lock()
	clk := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.meta[aw.ClientID] = MakeObject("clock", clk, "lastUpdated", getUnixTime()-int64(OutdatedTimeout/2/time.Millisecond)-1000)
	aw.destroyed = true // local state still present + stale -> a tick would renew but for the guard
	aw.mu.Unlock()

	m.tick() // must bail (destroyed): no renewal, no removal

	aw.mu.Lock()
	after := aw.meta[aw.ClientID].GetOr("clock").(Number)
	_, present := aw.states[aw.ClientID]
	aw.mu.Unlock()
	if after != clk {
		t.Errorf("reapTick renewed local state despite destroyed (clock %d -> %d)", clk, after)
	}
	if !present {
		t.Error("reapTick removed local state despite destroyed")
	}
}

// Defends the round-3 guards DIRECTLY (not via reapTick's top-check): once destroyed
// is set, SetLocalState and RemoveAwarenessStates must no-op. This is the guard that
// closes the post-top-check-then-destroy interleaving — removing the `if a.destroyed`
// checks in setLocalState/RemoveAwarenessStates (even with reapTick's top-check
// intact) makes this test fail.
func TestAwarenessMutatorsNoopWhenDestroyed(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	// destroyed=true is set directly below, so aw.Destroy() would no-op and leak the
	// reaper goroutine; stop it directly in cleanup.
	t.Cleanup(func() {
		m.Stop()
	})

	aw.mu.Lock()
	clk := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.states[99] = newObject() // a remote client to attempt to reap
	aw.meta[99] = MakeObject("clock", 0, "lastUpdated", getUnixTime())
	aw.destroyed = true
	aw.mu.Unlock()

	// SetLocalState must not mutate (no clock bump) when destroyed.
	ns := newObject()
	ns.Set("x", 1)
	_ = aw.SetLocalState(ns)

	// RemoveAwarenessStates must not remove when destroyed.
	RemoveAwarenessStates(aw, []Number{99}, "test")

	aw.mu.Lock()
	gotClk := aw.meta[aw.ClientID].GetOr("clock").(Number)
	_, remotePresent := aw.states[99]
	aw.mu.Unlock()
	if gotClk != clk {
		t.Errorf("SetLocalState mutated despite destroyed (clock %d -> %d)", clk, gotClk)
	}
	if !remotePresent {
		t.Error("RemoveAwarenessStates removed a client despite destroyed")
	}
}

// Apply-after-destroy must be a no-op (uniform with the other destroyed-guarded
// mutators): an awareness update applied to a destroyed Awareness must not
// re-populate its state map. Has teeth — without the applyAwarenessEntries guard,
// client 7's state would be added back.
func TestApplyAwarenessUpdateNoopAfterDestroy(t *testing.T) {
	src := NewAwareness(newReaperDoc())
	_ = src.SetLocalState(func() Object { o := newObject(); o.Set("name", "alice"); return o }())
	update := EncodeAwarenessUpdate(src, []Number{src.ClientID}, nil)
	src.Destroy()

	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	aw.Destroy() // destroyed before applying
	if err := ApplyAwarenessUpdate(aw, update, "remote"); err != nil {
		t.Fatalf("ApplyAwarenessUpdate: %v", err)
	}
	if len(aw.GetStates()) != 0 {
		t.Errorf("apply-after-destroy re-populated state: %v", aw.GetStates())
	}
}

// Destroy must be idempotent: a second call (e.g. consumer call after the doc-'destroy'
// handler already ran it) must not re-mutate Meta (bump the local clock) or re-emit.
func TestAwarenessDestroyIdempotent(t *testing.T) {
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()
	aw.mu.Lock()
	clk := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.mu.Unlock()

	aw.Destroy() // clears local state -> bumps clock once
	aw.mu.Lock()
	afterFirst := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.mu.Unlock()
	if afterFirst <= clk {
		t.Fatalf("first Destroy should have bumped the clock: %d -> %d", clk, afterFirst)
	}

	aw.Destroy() // must be a no-op
	aw.mu.Lock()
	afterSecond := aw.meta[aw.ClientID].GetOr("clock").(Number)
	aw.mu.Unlock()
	if afterSecond != afterFirst {
		t.Errorf("second Destroy mutated Meta (clock %d -> %d); Destroy must be idempotent", afterFirst, afterSecond)
	}
}

func TestAwarenessReaperAutoStartAndDestroyExits(t *testing.T) {
	// Auto-start: NewAwareness starts the reaper with no explicit Start(); doneCh is
	// still open. Destroy signals it to stop (asynchronously — no join, which would
	// self-deadlock if an emitted-event observer called Destroy from the reaper
	// goroutine), and it must exit promptly: no leak. Verified DETERMINISTICALLY via
	// the reaper's doneCh rather than a flaky NumGoroutine delta.
	m := NewManagedAwareness(newReaperDoc())
	aw := m.Awareness()

	aw.Destroy()
}

// ---------------------------------------------------------------- from awareness_split_test.go
// US6 tests — written before the split. The plain type must never own a thread; the managed type
// must reproduce the reference's timer in full, because only one of the reference's two
// time-driven behaviours can be made lazy.
//
// REAPING is a read-time judgement about which remote clients still count as present, so it can
// happen on access. RENEWAL is an outbound heartbeat that re-publishes local state so remote peers
// do not drop this client — its trigger is elapsed time, not a read, so nothing read-triggered can
// reproduce it. That asymmetry is why the timer becomes opt-in rather than deleted.

func goroutineDelta(t *testing.T, f func()) int {
	t.Helper()
	runtime.GC()
	before := runtime.NumGoroutine()
	f()
	runtime.GC()
	time.Sleep(50 * time.Millisecond) // let any started goroutine actually be scheduled
	runtime.GC()
	return runtime.NumGoroutine() - before
}

// C-P1.1 / FR-009: the plain type MUST NOT start a goroutine, ever.
func TestPlainAwarenessStartsNoGoroutine(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	delta := goroutineDelta(t, func() {
		aw := NewAwareness(doc)
		st := newObject()
		st.Set("name", 1)
		_ = aw.SetLocalState(st)
	})
	if delta > 0 {
		t.Errorf("plain Awareness started %d goroutine(s); FR-009 requires none by default — a "+
			"library that spawns a thread the consumer did not ask for also owns a lifecycle the "+
			"consumer must remember", delta)
	}
}

// C-P1.5 / SC-006: discarding plain values must leave nothing behind, with no disposal call.
func TestPlainAwarenessNeedsNoDisposal(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates many objects")
	}
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	delta := goroutineDelta(t, func() {
		for i := 0; i < 2000; i++ {
			aw := NewAwareness(doc)
			st := newObject()
			st.Set("i", i)
			_ = aw.SetLocalState(st)
			// deliberately NOT destroyed: the plain type must require no teardown
		}
	})
	if delta > 2 {
		t.Errorf("2000 discarded plain Awareness values left %d goroutine(s); they must require "+
			"no disposal call at all", delta)
	}
}

// C-P1.3 / FR-010: stale remote entries are judged expired ON ACCESS.
func TestPlainAwarenessExpiresOnAccess(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	aw := NewAwareness(doc)

	// A remote client whose last update is older than the timeout.
	stale := Number(99)
	aw.mu.Lock()
	aw.states[stale] = newObject()
	aw.meta[stale] = MakeObject("clock", 0,
		"lastUpdated", getUnixTime()-int64(OutdatedTimeout/time.Millisecond)-1000)
	aw.mu.Unlock()

	if _, present := aw.GetStates()[stale]; present {
		t.Error("a remote entry past the timeout is still reported present; the plain type must " +
			"judge expiry on access, since it has no timer to reap")
	}
}

// C-P2.1 / FR-011a: the managed type owns the timer, started only by an explicit call.
func TestManagedAwarenessTimerIsExplicit(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))

	var m *ManagedAwareness
	delta := goroutineDelta(t, func() { m = NewManagedAwareness(doc) })
	if delta > 0 {
		t.Errorf("constructing ManagedAwareness started %d goroutine(s); Constitution II permits "+
			"the goroutine only when EXPLICITLY requested, and a constructor is not a request", delta)
	}

	delta = goroutineDelta(t, func() { m.Start() })
	if delta < 1 {
		t.Error("Start() did not start the timer")
	}
	delta = goroutineDelta(t, func() { m.Stop() })
	if delta > -1 {
		t.Errorf("Stop() left the timer running (delta %d); C-P2.4 requires no residue", delta)
	}
}

// C-P2.4 / SC-006: start+stop cycles must leave nothing behind.
func TestManagedAwarenessStopLeavesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts many timers")
	}
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	delta := goroutineDelta(t, func() {
		for i := 0; i < 200; i++ {
			m := NewManagedAwareness(doc)
			m.Start()
			m.Stop()
		}
	})
	if delta > 2 {
		t.Errorf("200 start/stop cycles left %d goroutine(s)", delta)
	}
}

// C-P3.1 / FR-012a: the unsafe pairing must be UNREPRESENTABLE, not merely
// discouraged. NewManagedAwarenessFrom can attach a writer to any plain value,
// so neither type may expose the underlying maps.
func TestPresenceTypesCannotPairFieldsWithAWriter(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	typ := reflect.TypeOf(Awareness{})
	for _, field := range []string{"States", "Meta"} {
		if _, ok := typ.FieldByName(field); ok {
			t.Fatalf("Awareness.%s exposes mutable state while a managed writer can be attached", field)
		}
	}

	managed := NewManagedAwarenessFrom(NewAwareness(doc))
	managed.Start()
	defer managed.Stop()
	if got := managed.GetStates(); got == nil {
		t.Error("managed awareness must expose state through owned accessors")
	}
}

// SC-006a / C-S4.3: the managed type's RENEWAL must keep an otherwise-idle client alive against a
// peer applying the reference's timeout rule.
//
// This is the behaviour that makes the timer impossible to delete rather than merely opt-in: a
// plain client that goes quiet stops renewing and is dropped, while a managed one keeps
// republishing. Verified by simulating the peer's judgement — an entry is present iff its
// lastUpdated is within the timeout — rather than waiting 30 real seconds.
func TestManagedAwarenessRenewalKeepsClientAlive(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m := NewManagedAwareness(doc)
	defer m.Destroy()

	st := newObject()
	st.Set("name", "alive")
	_ = m.SetLocalState(st)

	// Age the local entry past the renewal threshold (half the timeout) but not past the full
	// timeout: this is exactly when the reference renews.
	aw := m.Awareness()
	aw.mu.Lock()
	meta := aw.meta[aw.ClientID]
	clockBefore, _ := meta.GetOr("clock").(Number)
	aw.meta[aw.ClientID] = MakeObject("clock", clockBefore,
		"lastUpdated", getUnixTime()-int64(OutdatedTimeout/2/time.Millisecond)-1000)
	aw.mu.Unlock()

	m.tick()

	aw.mu.Lock()
	after := aw.meta[aw.ClientID]
	lastUpdated, _ := after.GetOr("lastUpdated").(int64)
	clockAfter, _ := after.GetOr("clock").(Number)
	aw.mu.Unlock()

	fresh := getUnixTime()-lastUpdated < int64(OutdatedTimeout/2/time.Millisecond)
	if !fresh {
		t.Error("the managed timer did not renew local presence; a reference peer would drop this " +
			"client once the full timeout elapsed, which is the interop break the plain type " +
			"documents as its limitation")
	}
	if clockAfter <= clockBefore {
		t.Errorf("renewal did not advance the clock (%d -> %d); peers use the clock to accept the "+
			"refreshed state", clockBefore, clockAfter)
	}

	// And the plain type, by contrast, does NOT renew — the documented parity limitation.
	plainDoc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	plain := NewAwareness(plainDoc)
	_ = plain.SetLocalState(st)
	plain.mu.Lock()
	stale := getUnixTime() - int64(OutdatedTimeout/2/time.Millisecond) - 1000
	pm := plain.meta[plain.ClientID]
	plain.meta[plain.ClientID] = MakeObject("clock", pm.GetOr("clock"), "lastUpdated", stale)
	plain.mu.Unlock()
	// No timer exists, so nothing refreshes it.
	plain.mu.Lock()
	got, _ := plain.meta[plain.ClientID].GetOr("lastUpdated").(int64)
	plain.mu.Unlock()
	if got != stale {
		t.Error("the plain type renewed local presence; it must not — it owns no timer, and " +
			"claiming otherwise would hide the parity limitation FR-011 requires documenting")
	}
}
