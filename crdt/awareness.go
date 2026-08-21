package crdt

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------- from awareness.go

// ErrMalformedAwarenessState is returned by ApplyAwarenessUpdate /
// applyAwarenessUpdateWithoutEvents when a client entry's state payload is valid JSON but
// not a JSON object (a string / array / number / bool). The Awareness state map
// holds Object values, so such a payload cannot be stored — an unchecked type
// assertion would panic and crash the process on hostile input. (The protocol
// package mirrors this with its own ErrMalformedAwarenessState in
// DecodeAwarenessMessage.)
var ErrMalformedAwarenessState = errors.New("awareness state is not a JSON object")

// ErrTruncatedAwarenessFrame is returned by ApplyAwarenessUpdate /
// applyAwarenessUpdateWithoutEvents when a per-client entry cannot be decoded because the
// frame is truncated (e.g. a state-string length prefix claims more bytes than
// remain). It wraps the underlying decode error, so the cause stays retrievable
// while callers can errors.Is it apart from ErrMalformedAwarenessState (which
// signals a well-formed-but-non-object state payload). Because the apply is
// all-or-nothing, receiving this error guarantees NOTHING in States/Meta was
// mutated.
var ErrTruncatedAwarenessFrame = errors.New("awareness frame is truncated")

/*
 * The Awareness class implements a simple shared state protocol that can be used for non-persistent data like awareness information
 * (cursor, username, status, ..). Each client can update its own local state and listen to state changes of
 * remote clients. Every client may set a state of a remote peer to `null` to mark the client as offline.
 *
 * Each client is identified by a unique client id (something we borrow from `doc.clientID`). A client can override
 * its own state by propagating a message with an increasing timestamp (`clock`). If such a message is received, it is
 * applied if the known state of that client is older than the new state (`clock < newClock`). If a client thinks that
 * a remote client is offline, it may propagate a message with
 * `{ clock: currentClientClock, state: null, client: remoteClient }`. If such a
 * message is received,  and the known clock of that client equals the received clock, it will override the state with `null`.
 *
 * Before a client disconnects, it should propagate a `null` state with an updated clock.
 *
 * Awareness states must be updated every 30 seconds. Otherwise the Awareness instance will delete the client state.
 *
 * @extends {Observable<string>}
 */

const OutdatedTimeout = 30 * time.Second

// Awareness is the PLAIN presence type: it never starts a goroutine.
//
// Its maps are private and every supported boundary transfers ownership: setters
// deep-copy caller data and getters return independent deep snapshots. This is
// required even for the plain type because NewManagedAwarenessFrom may attach a
// timer goroutine to the same value after the caller has retained its pointer.
// Protecting only the maps would not be enough — Object values are reference-like
// handles, so mutating a nested value obtained through an accessor would still
// race a managed writer.
//
// PARITY LIMITATION (FR-011): this type performs no local RENEWAL. The reference's timer does two
// things — reaping stale remotes, and re-publishing local state so remote peers do not drop this
// client. Reaping is a read-time judgement and happens on access here; renewal is an outbound
// heartbeat triggered by elapsed time, which nothing read-triggered can reproduce. A client whose
// program stays quiet past the timeout will therefore be dropped by reference peers. Use
// ManagedAwareness where that matters; presence parity claims attach to it.
type Awareness struct {
	*Observable
	Doc      *Doc
	ClientID Number

	states map[Number]Object
	meta   map[Number]Object

	// mu guards mutation and keeps each deep snapshot internally consistent. A
	// plain value normally has no contending library writer; a managed wrapper does.
	mu        sync.Mutex
	destroyed bool
}

func (a *Awareness) Destroy() {
	// Idempotent: claim destroyed under the lock and bail if already destroyed, so a
	// second Destroy (e.g. a consumer call after the doc-'destroy' handler already
	// fired) does not re-emit or re-mutate Meta. Setting destroyed FIRST also makes any
	// later reaper renewal/reap a no-op (setLocalState consults the flag under the lock)
	// so teardown cannot re-add state.
	a.mu.Lock()
	if a.destroyed {
		a.mu.Unlock()
		return
	}
	a.destroyed = true
	a.mu.Unlock()
	// No reaper to signal: the plain type owns no goroutine. Destroy remains available so a
	// consumer can broadcast its removal deliberately, but nothing REQUIRES calling it —
	// discarding a plain value leaks nothing (C-P1.5).
	// Emit the awareness directly (v[0] == a), matching Doc.Destroy's `Emit("destroy",
	// doc)`. Wrapping it in a slice would hand observers the slice as v[0].
	a.Emit("destroy", a)
	// The zero Object (IsNil) marks a cleared/null local state; force past the guard
	// to broadcast the removal.
	_ = a.setLocalState(Object{}, true)
	a.Observable.Destroy()
}

// GetLocalState returns an independent deep snapshot of this client's state.
// Mutating it cannot change awareness state or race a ManagedAwareness writer.
func (a *Awareness) GetLocalState() Object {
	a.mu.Lock()
	defer a.mu.Unlock()
	return mustCloneDataObject(a.states[a.ClientID])
}

// SetLocalState sets (or, when state.IsNil(), clears) this client's awareness
// state. A nil/cleared state is represented by the zero Object value (IsNil).
// The state is deep-copied before it enters the awareness maps. Unsupported
// mutable values are rejected without changing state, clocks, or observers.
func (a *Awareness) SetLocalState(state Object) error {
	return a.setLocalState(state, false)
}

// setLocalState is SetLocalState's guarded core. When force is false and the
// awareness has been destroyed, it is a no-op: the destroyed-check and the mutation
// share ONE critical section, so a late reaper renewal cannot re-add local state
// after teardown (no TOCTOU). Destroy passes force=true to run its own clear +
// removal broadcast despite the flag.
func (a *Awareness) setLocalState(state Object, force bool) error {
	clientID := a.ClientID
	a.mu.Lock()
	if a.destroyed && !force {
		a.mu.Unlock()
		return nil
	}
	ownedState, err := cloneDataObject(state)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("set local awareness state: %w", err)
	}
	currLocalMeta, ok := a.meta[clientID]
	var clock Number
	if !ok {
		clock = 0
	} else {
		clock = currLocalMeta.GetOr("clock").(Number) + 1
	}
	prevState := a.states[clientID]
	if ownedState.IsNil() {
		delete(a.states, clientID)
	} else {
		a.states[clientID] = ownedState
	}

	a.meta[clientID] = MakeObject(
		"clock", clock,
		"lastUpdated", getUnixTime(),
	)

	var added []Number
	var updated []Number
	var filteredUpdated []Number
	var removed []Number
	switch {
	case ownedState.IsNil():
		removed = append(removed, clientID)
	case prevState.IsNil():
		// if state != nil {
		added = append(added, clientID)
		// }
	default:
		updated = append(updated, clientID)
		// Mirror y-protocols setLocalState exactly:
		//   if (!f.equalityDeep(prevState, state)) { filteredUpdated.push(clientID) }
		// i.e. the client is added to filteredUpdated (the 'change' event's updated
		// list) when the state actually CHANGED — not when it stayed the same. The
		// prior `if equalAttrs(prevState, state)` was the INVERSE: a real change fired
		// no 'change' event and a no-op fired a spurious one. equalAttrsDeep is the
		// DEEP comparator (lib0 equalityDeep) so a re-sent, structurally-identical
		// NESTED state (a fresh decoded instance) is correctly seen as unchanged —
		// the shallow equalAttrs/equalFlat would over-report it as changed.
		if !equalAttrsDeep(prevState, ownedState) {
			filteredUpdated = append(filteredUpdated, clientID)
		}
	}

	a.mu.Unlock()

	if len(added) > 0 || len(filteredUpdated) > 0 || len(removed) > 0 {
		a.Emit("change", MakeObject("added", added, "updated", filteredUpdated, "removed", removed), "local")
	}

	a.Emit("update", MakeObject("added", added, "updated", updated, "removed", removed), "local")
	return nil
}

func (a *Awareness) SetLocalStateField(field string, value interface{}) error {
	state := a.GetLocalState()
	if !state.IsNil() {
		state.Set(field, value)
		return a.SetLocalState(state)
	}
	return nil
}

// GetMeta returns a deep snapshot of the client→metadata map (clock /
// lastUpdated), the read-only counterpart to GetStates. Both the map and its
// Object values are independent of the internal metadata.
func (a *Awareness) GetMeta() map[Number]Object {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[Number]Object, len(a.meta))
	for k, v := range a.meta {
		out[k] = mustCloneDataObject(v)
	}
	return out
}

// GetStates returns a deep snapshot of the client→state map. Both the map and
// every mutable value reachable through it are independent of internal state.
func (a *Awareness) GetStates() map[Number]Object {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := getUnixTime()
	fullMs := int64(OutdatedTimeout / time.Millisecond)
	out := make(map[Number]Object, len(a.states))
	for k, v := range a.states {
		// Expiry judged ON ACCESS (FR-010): with no timer, "who is present" is decided when the
		// question is asked. Matches the reference's judgement — a remote client past the timeout
		// is not present — without owning a thread to reach that conclusion on a schedule.
		//
		// The LOCAL client is never expired this way: the reference renews it rather than reaping
		// it, and a plain value has no renewal, so dropping ourselves would be wrong.
		if k != a.ClientID {
			if meta, ok := a.meta[k]; ok {
				if lu, ok := meta.GetOr("lastUpdated").(int64); ok && now-lu >= fullMs {
					continue
				}
			}
		}
		out[k] = mustCloneDataObject(v)
	}
	return out
}

func NewAwareness(doc *Doc) *Awareness {
	aw := &Awareness{
		Observable: NewObservable(),
		Doc:        doc,
		ClientID:   doc.ClientID,
		states:     make(map[Number]Object),
		meta:       make(map[Number]Object),
	}

	// NO goroutine. The reference starts a setInterval in its constructor; doing that here would
	// hand every consumer a thread they did not ask for, plus a lifecycle they must remember —
	// which is what Constitution II forbids without an explicit request. ManagedAwareness provides
	// the timer for consumers that need it (FR-009, FR-011a).

	doc.On("destroy", NewObserverHandler(func(v ...interface{}) {
		aw.Destroy()
	}))
	_ = aw.SetLocalState(newObject())
	return aw
}

// RemoveAwarenessStates marks (remote) clients as inactive and removes them from
// the list of active peers.
// This change will be propagated to remote clients.
func RemoveAwarenessStates(awareness *Awareness, clients []Number, origin interface{}) {
	var added []Number
	var updated []Number
	var removed []Number
	awareness.mu.Lock()
	if awareness.destroyed {
		// No-op after teardown — symmetric with setLocalState's guard, so a late
		// reaper reap (or a post-Destroy consumer call) can't mutate/emit on a
		// destroyed awareness.
		awareness.mu.Unlock()
		return
	}
	for i := 0; i < len(clients); i++ {
		clientID := clients[i]
		if _, exist := awareness.states[clientID]; exist {
			delete(awareness.states, clientID)
			if clientID == awareness.ClientID {
				curMeta := awareness.meta[clientID]
				awareness.meta[clientID] = MakeObject(
					"clock", curMeta.GetOr("clock").(Number)+1,
					"lastUpdated", getUnixTime(),
				)
			}
			removed = append(removed, clientID)
		}
	}

	awareness.mu.Unlock()

	if len(removed) > 0 {
		awareness.Emit("change", MakeObject("added", added, "updated", updated, "removed", removed), origin)
		awareness.Emit("update", MakeObject("added", added, "updated", updated, "removed", removed), origin)
	}
}

// AwarenessStateJSON is the SINGLE serialization boundary for an awareness
// client state: a cleared/removed state (the zero Object, IsNil) serializes as
// the JSON literal "null", and any present state as its JSON.stringify-faithful
// object JSON. Emitting "null" (not "{}") for a cleared state is what makes a
// receiving peer REMOVE the client; "{}" is a PRESENT empty object the peer
// applies and never removes, leaving a ghost cursor for a disconnected/cleared
// client.
//
// This guard was hand-duplicated at EncodeAwarenessUpdate and the protocol
// package's EncodeAwarenessMessage, and was MISSING at ModifyAwarenessUpdate
// (the 3rd ghost-cursor site). Centralizing it here and calling it from all
// three keeps the cleared-state boundary in one place. (Do NOT change
// Object.MarshalJSON globally: "{}" is the correct serialization for an empty
// object on the ContentJson / any paths; only this awareness boundary needs it.)
func AwarenessStateJSON(state Object) string {
	if state.IsNil() {
		return "null"
	}
	return jsonString(state)
}

func EncodeAwarenessUpdate(awareness *Awareness, clients []Number, states map[Number]Object) []byte {
	awareness.mu.Lock()
	defer awareness.mu.Unlock()
	if states == nil {
		states = awareness.states
	}
	encoder := newEncoder()
	// Only encode clients we hold metadata (a clock) for. A caller-supplied client
	// absent from Meta has no clock to write — encoding it would both panic on the
	// nil clock assertion and desync the leading count from the body. Filtering
	// first keeps count == entries; for the normal case (clients drawn from the
	// awareness's own keys) every client is present, so output is unchanged.
	valid := make([]Number, 0, len(clients))
	for _, clientID := range clients {
		if !awareness.meta[clientID].IsNil() {
			valid = append(valid, clientID)
		}
	}
	writeVarUint(encoder, uint64(len(valid)))
	for _, clientID := range valid {
		state := states[clientID]
		clientMeta := awareness.meta[clientID]
		clock := clientMeta.GetOr("clock").(Number)
		writeVarUint(encoder, uint64(clientID))
		writeVarUint(encoder, uint64(clock))
		_ = writeString(encoder, AwarenessStateJSON(state))
	}
	return encoder.Bytes()
}

// ModifyAwarenessUpdate modifies the content of an awareness update before
// re-encoding it to an awareness update.
//
// This might be useful when you have a central server that wants to ensure that clients
// cant hijack somebody elses identity.
func ModifyAwarenessUpdate(update []byte, modify func(interface{}) interface{}) ([]byte, error) {
	decoder := newDecoder(update)
	encoder := newEncoder()
	length, err := readVarUint(decoder)
	if err != nil {
		return nil, fmt.Errorf("%w: entry count: %w", ErrTruncatedAwarenessFrame, err)
	}

	// DoS guard, mirroring decodeAwarenessEntries: the leading count is
	// attacker-controlled (up to math.MaxUint64). Every legitimate entry is at
	// least 3 bytes (VarUint clientID + VarUint clock + VarString state), so a
	// count above remaining/3 is provably truncated. Reject it before invoking the
	// callback or growing output.
	if boundExceeded(length, decoder.Len(), 3) {
		return nil, fmt.Errorf("%w: entry count %d exceeds remaining %d bytes (each entry needs >=3)", ErrTruncatedAwarenessFrame, length, decoder.Len())
	}

	writeVarUint(encoder, length)
	for i := uint64(0); i < length; i++ {
		clientID, err := readVarUint(decoder)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: client id: %w", ErrTruncatedAwarenessFrame, i, err)
		}
		clock, err := readVarUint(decoder)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d (client %d): clock: %w", ErrTruncatedAwarenessFrame, i, clientID, err)
		}
		data, err := readString(decoder)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d (client %d): state: %w", ErrTruncatedAwarenessFrame, i, clientID, err)
		}
		state := jsonObject(data)
		modifiedState := modify(state)

		writeVarUint(encoder, clientID)
		writeVarUint(encoder, clock)
		// Correctness 4: route the (possibly re-typed) state through the SAME
		// cleared-state boundary as the other two encode sites. A modify callback
		// that normalizes a cleared entry to a zero/empty Object{} (or returns a bare
		// nil) must serialize as "null", not "{}" — otherwise the receiver keeps a
		// ghost cursor. A non-Object re-type (the callback returned a scalar/array/map)
		// falls back to its raw JSON, matching the prior behavior for that case.
		_ = writeString(encoder, awarenessModifiedStateJSON(modifiedState))
	}
	return encoder.Bytes(), nil
}

// awarenessModifiedStateJSON serializes the result of a ModifyAwarenessUpdate
// callback. An Object goes through AwarenessStateJSON so a cleared/empty state
// emits "null" (the ghost-cursor guard); anything else (a bare nil, or a
// scalar/array/map the callback substituted) serializes as its raw JSON —
// jsonString(nil) is itself "null", so the cleared-state case is covered either way.
func awarenessModifiedStateJSON(v interface{}) string {
	if s, ok := v.(Object); ok {
		return AwarenessStateJSON(s)
	}
	return jsonString(v)
}

// awarenessEntry is one fully-decoded, validated per-client entry from an
// awareness update frame. The apply is all-or-nothing: ALL entries are decoded
// into a []awarenessEntry first; only if every entry decodes and validates does
// any state mutation happen. state==nil encodes a cleared (null) state.
type awarenessEntry struct {
	clientID Number
	clock    Number
	state    Object
}

// ParseAwarenessStateJSON classifies an awareness entry's state JSON string into
// either a cleared state (the zero Object) or a populated state Object, with
// identical empty/null/object handling for BOTH the core decode path
// (decodeAwarenessEntries) and the protocol package (DecodeAwarenessMessage).
//
// This is the single source of truth that removes a decode DRIFT: the core path
// historically wrapped the parse in `if data != ""` (so an empty state string
// CLEARED the client), while the protocol path parsed unconditionally — so the
// same empty-state frame that cleared a cursor on the core side was rejected as
// ErrMalformedAwarenessState on the websocket side, leaving a GHOST cursor.
//
// Classification (identical on both sides):
//   - ""                       -> cleared state (zero Object, nil error)
//   - JSON null                -> cleared state (zero Object, nil error)
//   - a JSON object            -> that Object, nil error
//   - any other valid JSON     -> zero Object, ErrMalformedAwarenessState
//   - invalid JSON             -> zero Object, the underlying parse error
//
// Note this returns the underlying parse error (not wrapped in
// ErrMalformedAwarenessState) so callers can wrap/classify it as they need;
// decodeAwarenessEntries wraps it in ErrMalformedAwarenessState, the protocol
// path maps it to its own sentinel.
func ParseAwarenessStateJSON(data string) (Object, error) {
	// An empty state string is treated as cleared (the prior core tolerance);
	// it must NOT be fed to the JSON parser (which would EOF and reject it).
	if data == "" {
		return Object{}, nil
	}

	parsed, err := unmarshalJSONOrdered([]byte(data))
	if err != nil {
		return Object{}, err
	}
	switch v := parsed.(type) {
	case nil, NullType:
		// JSON null: a cleared state (the zero Object).
		return Object{}, nil
	case Object:
		return v, nil
	default:
		// A valid JSON value that is not an object (string/array/number/bool)
		// is malformed for an awareness state.
		return Object{}, ErrMalformedAwarenessState
	}
}

// decodeAwarenessEntries decodes and validates EVERY per-client entry of an
// awareness update WITHOUT touching any Awareness state. It is the single
// decode/validate pass that makes ApplyAwarenessUpdate / applyAwarenessUpdateWithoutEvents
// all-or-nothing: if any entry is truncated (ReadString fails) or carries a
// non-object state, it returns an error and the caller mutates nothing.
//
//   - A truncated entry (ReadString error) → ErrTruncatedAwarenessFrame (wrapping
//     the cause). Discarding that error would leave the state string empty,
//     jsonObject("") nil, and the entry misread as a CLEARED state — silently
//     deleting/overwriting existing state on hostile input.
//   - A state payload that is valid JSON but not an object → ErrMalformedAwarenessState
//     (the state map holds Object values; an unchecked assertion would panic).
//
// A truncated leading count varint yields length 0 here (ReadVarUint returns 0 on
// a short read), i.e. an empty entry list and a no-op apply — matching the prior
// behavior for that case.
//
// The leading count varint is attacker-controlled (up to math.MaxUint64). Before
// make()-ing the entries slice with it as capacity, bound it against the bytes
// that actually remain: every entry is VarUint(clientID) + VarUint(clock) +
// VarString(state), i.e. at least 3 bytes, so a count greater than the remaining
// byte budget is provably malformed and would otherwise trigger a `makeslice: cap
// out of range` panic or an unbounded allocation (remote DoS via the public
// ApplyAwarenessUpdate / applyAwarenessUpdateWithoutEvents API). Reject it as a truncated
// frame. Mirrors readArrayDepth / ReadArray (decoding.go) and the V2 ReadArray fix.
func decodeAwarenessEntries(update []byte) ([]awarenessEntry, error) {
	decoder := newDecoder(update)
	length, err := readVarUint(decoder)
	if err != nil {
		return nil, fmt.Errorf("%w: entry count: %w", ErrTruncatedAwarenessFrame, err)
	}

	if boundExceeded(length, decoder.Len(), 3) {
		return nil, fmt.Errorf("%w: entry count %d exceeds remaining %d bytes (each entry needs >=3)", ErrTruncatedAwarenessFrame, length, decoder.Len())
	}

	entries := make([]awarenessEntry, 0, length)
	for i := uint64(0); i < length; i++ {
		// Clamp the client id and clock through readVarUintAsNumber: the swallowing
		// ReadVarUint + unchecked int cast wrapped a value in [2^63, 2^64) to a
		// NEGATIVE Number (the negative-wrap class, H#3); reject it and surface the
		// read/overflow error (a huge-but-POSITIVE clock just under 2^63 is a separate
		// awareness-poisoning concern, out of scope here).
		clientID, err := readVarUintAsNumber(decoder)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: client id: %w", ErrTruncatedAwarenessFrame, i, err)
		}
		clock, err := readVarUintAsNumber(decoder)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d (client %d): clock: %w", ErrTruncatedAwarenessFrame, i, clientID, err)
		}

		data, err := readString(decoder)
		if err != nil {
			// Truncated frame: reject the WHOLE update. Distinguishable from
			// ErrMalformedAwarenessState via errors.Is, with the cause preserved.
			return nil, fmt.Errorf("%w: entry %d (client %d): %w", ErrTruncatedAwarenessFrame, i, clientID, err)
		}

		// Classify the state JSON via the shared ParseAwarenessStateJSON, the single
		// source of truth used by BOTH this path and the protocol package's
		// DecodeAwarenessMessage — so an empty/null/object state is handled
		// identically on both sides (no decode DRIFT / ghost cursor). DoS 2: a
		// deeply-nested state JSON returns a depth-bound error from
		// unmarshalJSONOrdered rather than being swallowed into a (misread) cleared
		// state. An empty state string is treated as cleared (the prior tolerance);
		// a real decode error is rejected as malformed.
		state, perr := ParseAwarenessStateJSON(data)
		if perr != nil {
			// Preserve the existing public contract: a valid-JSON-non-object yields
			// the BARE ErrMalformedAwarenessState (callers assert on it with == and
			// errors.Is); only a genuine JSON parse/decode error is wrapped with the
			// entry/client context (still errors.Is-able as ErrMalformedAwarenessState).
			if errors.Is(perr, ErrMalformedAwarenessState) {
				return nil, ErrMalformedAwarenessState
			}
			return nil, fmt.Errorf("%w: entry %d (client %d): %w", ErrMalformedAwarenessState, i, clientID, perr)
		}

		entries = append(entries, awarenessEntry{clientID: clientID, clock: clock, state: state})
	}

	return entries, nil
}

// applyAwarenessEntries is the SINGLE per-entry merge core shared by
// ApplyAwarenessUpdate (which emits change/update from the returned lists) and
// applyAwarenessUpdateWithoutEvents (which discards them and emits nothing). It mutates
// States/Meta under awareness.mu and returns the added/updated/filteredUpdated/
// removed client lists; events are emitted by the caller AFTER this returns, since
// observers may re-enter the awareness API. Centralizing this removes the
// hand-duplicated loop the two entry points carried, where a future clock/LWW fix
// could land in one and silently miss the other (DRY, Principle VII).
func (a *Awareness) applyAwarenessEntries(entries []awarenessEntry, timestamp int64) (added, updated, filteredUpdated, removed []Number) {
	a.mu.Lock()
	if a.destroyed {
		// No-op after teardown, uniform with setLocalState / RemoveAwarenessStates: an
		// update applied to a destroyed awareness must not re-populate its maps.
		a.mu.Unlock()
		return nil, nil, nil, nil
	}
	defer a.mu.Unlock()
	for _, e := range entries {
		clientID := e.clientID
		clock := e.clock
		state := e.state

		clientMeta := a.meta[clientID]
		prevState := a.states[clientID]

		currClock := 0
		if !clientMeta.IsNil() {
			currClock = clientMeta.GetOr("clock").(Number)
		}

		_, exist := a.states[clientID]
		if currClock < clock || (currClock == clock && state.IsNil() && exist) {
			if state.IsNil() {
				// never let a remote client remove this local state
				if clientID == a.ClientID && !a.states[a.ClientID].IsNil() {
					// remote client removed the local state. Do not remote state. Broadcast a message indicating
					// that this client still exists by increasing the clock
					clock++
				} else {
					delete(a.states, clientID)
				}
			} else {
				a.states[clientID] = state
			}

			a.meta[clientID] = MakeObject(
				"clock", clock,
				"lastUpdated", timestamp,
			)

			switch {
			case clientMeta.IsNil() && !state.IsNil():
				added = append(added, clientID)
			case !clientMeta.IsNil() && state.IsNil():
				removed = append(removed, clientID)
			case !state.IsNil():
				// Mirror y-protocols applyAwarenessUpdate:
				//   if (!f.equalityDeep(state, prevState)) { filteredUpdated.push(clientID) }
				// The direction here was already correct; the fix is the comparator —
				// equalAttrsDeep (lib0 equalityDeep, DEEP) instead of the shallow
				// equalAttrs/equalFlat. A re-sent structurally-identical NESTED state
				// arrives as a freshly-decoded distinct instance, so shallow reference
				// comparison over-reported it as changed; deep comparison treats it as
				// unchanged, matching upstream.
				if !equalAttrsDeep(state, prevState) {
					filteredUpdated = append(filteredUpdated, clientID)
				}
				updated = append(updated, clientID)
			}
		}
	}
	return added, updated, filteredUpdated, removed
}

func ApplyAwarenessUpdate(awareness *Awareness, update []byte, origin interface{}) error {
	// Decode + validate ALL entries before mutating anything: a malformed/truncated
	// frame must mutate NOTHING (no state, no clock) and must not skip the post-loop
	// Emit on a silent partial apply. See decodeAwarenessEntries.
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		return err
	}

	added, updated, filteredUpdated, removed := awareness.applyAwarenessEntries(entries, getUnixTime())

	if len(added) > 0 || len(filteredUpdated) > 0 || len(removed) > 0 {
		awareness.Emit("change", MakeObject("added", added, "updated", filteredUpdated, "removed", removed), origin)
	}

	if len(added) > 0 || len(updated) > 0 || len(removed) > 0 {
		awareness.Emit("update", MakeObject("added", added, "updated", updated, "removed", removed), origin)
	}

	return nil
}

// applyAwarenessUpdateWithoutEvents applies an awareness update without
// emitting the update or change events.
func applyAwarenessUpdateWithoutEvents(awareness *Awareness, update []byte) error {
	// All-or-nothing: decode + validate every entry before mutating any state, so a
	// truncated/malformed frame leaves States/Meta/clock untouched (see
	// decodeAwarenessEntries). This variant emits no events (it discards the change
	// lists); the all-or-nothing guarantee still matters because a partial apply
	// would advance a client's clock and block a legitimate same-clock re-send.
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		return err
	}

	awareness.applyAwarenessEntries(entries, getUnixTime())
	return nil
}

// ---------------------------------------------------------------- from awareness_managed.go

// ManagedAwareness owns the reference's presence timer.
//
// It exists because the reference's interval does TWO things and only one of them can be made
// lazy (research R6, verified in y-protocols/awareness.js):
//
//   - REAPING removes remote clients past the timeout. That is a read-time judgement about who
//     counts as present, so the plain Awareness does it on access.
//   - RENEWAL re-publishes local state once half the timeout has elapsed, so remote peers do not
//     drop this client. It is an OUTBOUND action whose trigger is elapsed time, not a read, so
//     nothing read-triggered can reproduce it. A quiet client with no renewal is dropped by
//     reference peers.
//
// So the timer cannot simply be deleted — only made opt-in, which is exactly the exception
// Constitution II names ("unless explicitly requested by the consumer (e.g. awareness timeout
// cleanup)"). PRESENCE PARITY CLAIMS ATTACH TO THIS TYPE (FR-011a).
//
// It is a SEPARATE type from Awareness rather than a mode flag so opting into a
// timer is explicit. Both types expose state only through deep-copying accessors:
// NewManagedAwarenessFrom can attach this writer to any existing Awareness, so
// the ownership boundary belongs to Awareness itself rather than this wrapper.
type ManagedAwareness struct {
	aw *Awareness

	mu       sync.Mutex
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	running  bool
}

// NewManagedAwareness wraps a fresh Awareness. It does NOT start the timer: a constructor is not
// an explicit request, and Constitution II permits the goroutine only when one is made.
func NewManagedAwareness(doc *Doc) *ManagedAwareness {
	return &ManagedAwareness{aw: NewAwareness(doc)}
}

// NewManagedAwarenessFrom adopts an existing Awareness, for a consumer that built one before
// deciding it needs the timer.
func NewManagedAwarenessFrom(aw *Awareness) *ManagedAwareness {
	return &ManagedAwareness{aw: aw}
}

// Start begins the reference's interval. Idempotent: calling it twice does not start two timers.
func (m *ManagedAwareness) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return
	}
	m.running = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	go m.run(m.stopCh, m.doneCh)
}

// Stop halts the timer and waits for it to exit, so a stopped value provably leaves no goroutine
// behind (C-P2.4). Idempotent.
func (m *ManagedAwareness) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	stop, done := m.stopCh, m.doneCh
	m.running = false
	m.mu.Unlock()

	m.stopOnce.Do(func() { close(stop) })
	<-done
	m.stopOnce = sync.Once{} // allow a later Start/Stop cycle
}

// Running reports whether the timer goroutine is live. Exported so a caller (and this package's
// tests) can assert the US6 invariant that construction starts nothing and Stop joins rather than
// merely signalling — otherwise "no goroutine is left behind" is only observable as a race-detector
// finding much later.
func (m *ManagedAwareness) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Awareness exposes the underlying presence value for encode/apply helpers that
// take one. Its maps remain private and its public state boundaries copy, so
// retaining this pointer cannot bypass the managed writer's lock.
func (m *ManagedAwareness) Awareness() *Awareness { return m.aw }

// GetStates returns a snapshot copy taken under the lock.
func (m *ManagedAwareness) GetStates() map[Number]Object { return m.aw.GetStates() }

// GetMeta returns a snapshot copy taken under the lock.
func (m *ManagedAwareness) GetMeta() map[Number]Object { return m.aw.GetMeta() }

// SetLocalState publishes an owned copy of local presence state.
func (m *ManagedAwareness) SetLocalState(state Object) error { return m.aw.SetLocalState(state) }

// GetLocalState returns this client's published state.
func (m *ManagedAwareness) GetLocalState() Object { return m.aw.GetLocalState() }

// On registers an observer, so a consumer sees the same change/update events the reference emits.
func (m *ManagedAwareness) On(event string, h *ObserverHandler) { m.aw.On(event, h) }

// Destroy stops the timer and tears down the underlying value.
func (m *ManagedAwareness) Destroy() {
	m.Stop()
	m.aw.Destroy()
}

// run is the reference's setInterval body: every OutdatedTimeout/10, renew the local clock if half
// the timeout has elapsed, and reap remote clients past the full timeout.
func (m *ManagedAwareness) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(OutdatedTimeout / 10)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

// tick decides under the lock, then applies AFTER releasing it: SetLocalState and
// RemoveAwarenessStates re-acquire the lock and emit, so calling them while holding it would
// deadlock. Times are milliseconds, matching getUnixTime.
func (m *ManagedAwareness) tick() {
	a := m.aw
	now := getUnixTime()
	halfMs := int64(OutdatedTimeout / 2 / time.Millisecond)
	fullMs := int64(OutdatedTimeout / time.Millisecond)

	a.mu.Lock()
	if a.destroyed {
		a.mu.Unlock()
		return
	}
	renewLocal := false
	if _, ok := a.states[a.ClientID]; ok {
		if meta, ok := a.meta[a.ClientID]; ok {
			if lu, ok := meta.GetOr("lastUpdated").(int64); ok && now-lu >= halfMs {
				renewLocal = true
			}
		}
	}

	// Reap stale remotes ATOMICALLY with the staleness decision: deleting under the same lock hold
	// means a concurrent apply cannot refresh a client between "decided stale" and "removed" and
	// then be spuriously reaped. Ordered by client id so a multi-peer timeout emits a deterministic
	// payload — the reference iterates an insertion-ordered Map, and ranging a Go map here would
	// make the emitted `removed` set order vary run to run (FR-014c).
	var removed []Number
	for clientID, meta := range a.meta {
		if clientID == a.ClientID {
			continue
		}
		lu, ok := meta.GetOr("lastUpdated").(int64)
		if !ok {
			continue
		}
		if _, hasState := a.states[clientID]; hasState && now-lu >= fullMs {
			removed = append(removed, clientID)
		}
	}
	sortNumbers(removed)
	a.mu.Unlock()

	// Renew by RE-READING the current local state (the reference does setLocalState(getLocalState())),
	// so a concurrent update is not clobbered with a stale snapshot.
	if renewLocal {
		if s := a.GetLocalState(); !s.IsNil() {
			_ = a.SetLocalState(s)
		}
	}
	if len(removed) > 0 {
		RemoveAwarenessStates(a, removed, "timeout")
	}
}

// sortNumbers keeps the emitted removal payload deterministic.
func sortNumbers(v []Number) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
