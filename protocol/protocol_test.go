package protocol

import (
	"bytes"
	"errors"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// fullSync drives a complete handshake from a (server) to b (client) and back
// until no more sync responses are produced, then returns.
func syncDocs(t *testing.T, server, client *crdt.Doc) {
	t.Helper()
	sh := NewSyncHandler(server)
	ch := NewSyncHandler(client)

	// client starts: SyncStep1 (its state vector) -> server
	msg := EncodeSyncStep1(client)

	// Bounce messages between the two handlers until both go quiet.
	queue := [][2]any{{sh, msg}} // {targetHandler, message}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		h := item[0].(*SyncHandler)
		m := item[1].([]byte)

		var reply bytes.Buffer
		if _, err := h.HandleMessage(m, &reply); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		if reply.Len() > 0 {
			// route the reply to the other handler
			other := ch
			if h == ch {
				other = sh
			}
			queue = append(queue, [2]any{other, reply.Bytes()})
		}
	}
}

func TestSyncHandshakeConverges(t *testing.T) {
	// Two divergent docs; after a full handshake both must have identical content.
	server := crdt.NewDoc("guid")
	client := crdt.NewDoc("guid")

	server.GetText("t").Insert(0, "server-side ", crdt.Object{})
	server.GetArray("a").Push(crdt.ArrayAny{"s1", "s2"})
	client.GetText("t").Insert(0, "client-side ", crdt.Object{})
	client.GetMap("m").Set("k", "v")

	// Sync both directions (client->server then server->client) so each learns
	// the other's structs.
	syncDocs(t, server, client)
	syncDocs(t, client, server)

	st := server.GetText("t").ToString()
	ct := client.GetText("t").ToString()
	if st != ct {
		t.Errorf("text diverged after handshake:\n  server=%q\n  client=%q", st, ct)
	}
	if server.GetArray("a").GetLength() != client.GetArray("a").GetLength() {
		t.Errorf("array length diverged: server=%d client=%d",
			server.GetArray("a").GetLength(), client.GetArray("a").GetLength())
	}
	if server.GetMap("m").Get("k") != client.GetMap("m").Get("k") {
		t.Errorf("map diverged after handshake")
	}
}

func TestMessageFramingRoundTrip(t *testing.T) {
	cases := []struct {
		typ     uint8
		payload []byte
	}{
		{MessageSync, []byte{1, 2, 3, 4}},
		{42, []byte("custom payload")},
		{200, nil},
	}
	// MessageAwareness is deliberately absent: unlike sync and custom payloads,
	// its complete update body is VarUint8Array-framed. That built-in type must
	// use EncodeAwarenessUpdateMessage and is covered against real y-protocols by
	// TestAwarenessMessageFrameMatchesYProtocols.
	for _, c := range cases {
		var buf bytes.Buffer
		WriteMessage(&buf, c.typ, c.payload)
		gotType, gotPayload, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if gotType != c.typ {
			t.Errorf("type: want %d got %d", c.typ, gotType)
		}
		bothEmpty := len(gotPayload) == 0 && len(c.payload) == 0
		if !bytes.Equal(gotPayload, c.payload) && !bothEmpty {
			t.Errorf("payload: want %v got %v", c.payload, gotPayload)
		}
	}
}

func TestCustomMessageHandler(t *testing.T) {
	doc := crdt.NewDoc("guid")
	h := NewSyncHandler(doc)

	called := false
	var gotPayload []byte
	h.RegisterHandler(42, func(payload []byte) error {
		called = true
		gotPayload = payload
		return nil
	})

	var framed bytes.Buffer
	WriteMessage(&framed, 42, []byte("hello-42"))

	var reply bytes.Buffer
	typ, err := h.HandleMessage(framed.Bytes(), &reply)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if typ != 42 {
		t.Errorf("type: want 42 got %d", typ)
	}
	if !called {
		t.Errorf("custom handler for type 42 was not called")
	}
	if string(gotPayload) != "hello-42" {
		t.Errorf("custom payload: want hello-42 got %q", gotPayload)
	}
}

func TestAwarenessMessageRoundTrip(t *testing.T) {
	state := crdt.MakeObject("name", "alice", "color", "#fff")
	payload := EncodeAwarenessMessage(12345, state)

	clientID, gotState, err := DecodeAwarenessMessage(payload)
	if err != nil {
		t.Fatalf("DecodeAwarenessMessage: %v", err)
	}
	if clientID != 12345 {
		t.Errorf("clientID: want 12345 got %d", clientID)
	}
	if gotState.GetOr("name") != "alice" || gotState.GetOr("color") != "#fff" {
		t.Errorf("state mismatch: got %v", gotState)
	}
}

// TestAwarenessFirstBroadcastAppliedByFreshReceiver guards the off-by-one fix in
// nextAwarenessClock. A fresh receiver defaults currClock to 0 and applies an
// entry only when currClock < clock, so the very first EncodeAwarenessMessage
// broadcast must carry clock >= 1 or it is silently dropped. We decode the first
// broadcast and feed it through the root ApplyAwarenessUpdate on a never-seen
// awareness, asserting the state actually lands.
func TestAwarenessFirstBroadcastAppliedByFreshReceiver(t *testing.T) {
	// Reset the package-level clock map so this test starts from "unseen".
	ResetAllAwarenessClocks()

	const clientID crdt.Number = 987654
	payload := EncodeAwarenessMessage(clientID, crdt.MakeObject("name", "carol"))

	// Fresh receiver: a brand-new Awareness that has never seen clientID.
	doc := crdt.NewDoc("guid")
	aw := crdt.NewAwareness(doc)
	// ApplyAwarenessUpdate returns an error; assert it succeeds so an unexpected
	// decode/validation failure fails loudly here rather than surfacing as the
	// less obvious "first broadcast dropped" assertion below.
	if err := crdt.ApplyAwarenessUpdate(aw, payload, "remote"); err != nil {
		t.Fatalf("ApplyAwarenessUpdate (first broadcast) failed: %v", err)
	}

	got := aw.GetStates()[clientID]
	if got.IsNil() || got.GetOr("name") != "carol" {
		t.Fatalf("first awareness broadcast dropped by fresh receiver: states=%v", aw.GetStates())
	}
}

// TestDecodeAwarenessMessageTruncated guards that a truncated payload is
// reported as an error rather than silently misread (e.g. an empty count or a
// zero clientID).
func TestDecodeAwarenessMessageTruncated(t *testing.T) {
	// A valid one-client payload, then chopped before the clientID/clock/state.
	full := EncodeAwarenessMessage(42, crdt.MakeObject("k", "v"))
	if len(full) < 2 {
		t.Fatalf("unexpected payload length %d", len(full))
	}
	// Keep only the count byte (1 client) — clientID/clock/state are missing.
	truncated := full[:1]
	if _, _, err := DecodeAwarenessMessage(truncated); err == nil {
		t.Fatalf("expected error decoding truncated awareness payload, got nil")
	}

	// Truncated multi-byte count varint: a lone continuation byte (0x80) has the
	// continuation bit set but no following byte. The error-discarding path
	// misreads this as count=0 and returns ErrEmptyAwareness — masking a malformed
	// frame as a well-formed empty one. The error-surfacing path must instead
	// report a real decode error (anything other than ErrEmptyAwareness).
	if _, _, err := DecodeAwarenessMessage([]byte{0x80}); err == nil {
		t.Fatalf("expected error decoding truncated-count payload, got nil")
	} else if errors.Is(err, ErrEmptyAwareness) {
		t.Fatalf("truncated count must not be misclassified as ErrEmptyAwareness")
	}

	// Count says 1 client, but the clientID varint is a truncated continuation
	// byte. The discarding path would silently yield clientID=0; the surfacing
	// path errors.
	if _, _, err := DecodeAwarenessMessage([]byte{0x01, 0x80}); err == nil {
		t.Fatalf("expected error decoding payload truncated mid-clientID, got nil")
	}

	// Empty buffer: the count read itself must error.
	if _, _, err := DecodeAwarenessMessage(nil); err == nil {
		t.Fatalf("expected error decoding empty awareness payload, got nil")
	}
}

// TestAwarenessClockResetAPI verifies the reset helpers bound the package-level
// clock map: after advancing a client's clock, ResetAwarenessClock removes its
// entry (so the next broadcast restarts at 1), and ResetAllAwarenessClocks
// clears everything.
func TestAwarenessClockResetAPI(t *testing.T) {
	ResetAllAwarenessClocks()

	const a crdt.Number = 111
	const b crdt.Number = 222

	// First broadcast for each client -> clock 1; second for a -> clock 2.
	_ = EncodeAwarenessMessage(a, crdt.MakeObject("n", "1"))
	_ = EncodeAwarenessMessage(a, crdt.MakeObject("n", "2"))
	_ = EncodeAwarenessMessage(b, crdt.MakeObject("n", "1"))

	awarenessClockMu.Lock()
	clockA, okA := awarenessClocks[a]
	awarenessClockMu.Unlock()
	if !okA || clockA != 2 {
		t.Fatalf("client a clock: want 2 got %d (present=%v)", clockA, okA)
	}

	// Reset just client a -> its entry is gone; client b remains.
	ResetAwarenessClock(a)
	awarenessClockMu.Lock()
	_, okA = awarenessClocks[a]
	_, okB := awarenessClocks[b]
	awarenessClockMu.Unlock()
	if okA {
		t.Fatalf("ResetAwarenessClock did not remove client a")
	}
	if !okB {
		t.Fatalf("ResetAwarenessClock removed the wrong client (b gone)")
	}

	// a's next broadcast restarts at 1 (still applied by fresh receivers).
	doc := crdt.NewDoc("guid")
	aw := crdt.NewAwareness(doc)
	if err := crdt.ApplyAwarenessUpdate(aw, EncodeAwarenessMessage(a, crdt.MakeObject("n", "fresh")), "remote"); err != nil {
		t.Fatalf("post-reset ApplyAwarenessUpdate failed: %v", err)
	}
	if got := aw.GetStates()[a]; got.IsNil() || got.GetOr("n") != "fresh" {
		t.Fatalf("post-reset broadcast not applied: states=%v", aw.GetStates())
	}

	// Clear everything.
	ResetAllAwarenessClocks()
	awarenessClockMu.Lock()
	n := len(awarenessClocks)
	awarenessClockMu.Unlock()
	if n != 0 {
		t.Fatalf("ResetAllAwarenessClocks left %d entries", n)
	}
}

// TestDecodeAwarenessMessageMalformedJSON guards that a malformed state JSON is
// reported as an error rather than silently decoding to a nil state (the prior
// behavior, where crdt.jsonObject swallowed the json.Unmarshal error).
func TestDecodeAwarenessMessageMalformedJSON(t *testing.T) {
	// Hand-build a one-client awareness payload whose state string is invalid JSON.
	buf := new(bytes.Buffer)
	lib0.WriteVarUint(buf, 1)                    // one client
	lib0.WriteVarUint(buf, 7)                    // clientID
	lib0.WriteVarUint(buf, 1)                    // clock
	_ = lib0.WriteString(buf, "{not valid json") // malformed state

	if _, _, err := DecodeAwarenessMessage(buf.Bytes()); err == nil {
		t.Fatalf("expected error decoding malformed state JSON, got nil")
	}
}

// TestDecodeAwarenessMessageNonObjectState guards that a state payload which is
// valid JSON but not an object (string/array/number/bool) is rejected with
// ErrMalformedAwarenessState, while null (cleared state) and a real object are
// accepted. Otherwise a non-object would silently decode to a nil state that a
// caller could not distinguish from a legitimate empty/cleared one.
func TestDecodeAwarenessMessageNonObjectState(t *testing.T) {
	encode := func(stateJSON string) []byte {
		buf := new(bytes.Buffer)
		lib0.WriteVarUint(buf, 1)
		lib0.WriteVarUint(buf, 7)
		lib0.WriteVarUint(buf, 1)
		_ = lib0.WriteString(buf, stateJSON)
		return buf.Bytes()
	}

	// Non-object JSON values must be rejected.
	for _, bad := range []string{`"a string"`, `[1,2,3]`, `42`, `true`} {
		if _, _, err := DecodeAwarenessMessage(encode(bad)); !errors.Is(err, ErrMalformedAwarenessState) {
			t.Errorf("state %s: want ErrMalformedAwarenessState, got %v", bad, err)
		}
	}

	// null = cleared state: nil (zero) state, no error.
	if cid, st, err := DecodeAwarenessMessage(encode(`null`)); err != nil || !st.IsNil() || cid != 7 {
		t.Errorf("null state: want (7, nil, nil), got (%d, %v, %v)", cid, st, err)
	}

	// A real object decodes fine.
	if _, st, err := DecodeAwarenessMessage(encode(`{"name":"x"}`)); err != nil || st.GetOr("name") != "x" {
		t.Errorf("object state: want ok, got st=%v err=%v", st, err)
	}
}

func TestAwarenessApplyViaHandler(t *testing.T) {
	doc := crdt.NewDoc("guid")
	aw := crdt.NewAwareness(doc)
	h := NewSyncHandler(doc)
	h.SetAwareness(aw)

	// Build a full awareness update for the local client via the root API.
	_ = aw.SetLocalState(crdt.MakeObject("name", "bob"))
	clients := []crdt.Number{aw.ClientID}
	update := crdt.EncodeAwarenessUpdate(aw, clients, nil)

	// Apply it to a second awareness via the handler.
	doc2 := crdt.NewDoc("guid")
	aw2 := crdt.NewAwareness(doc2)
	h2 := NewSyncHandler(doc2)
	h2.SetAwareness(aw2)

	framed := EncodeAwarenessUpdateMessage(update)
	var reply bytes.Buffer
	typ, err := h2.HandleMessage(framed, &reply)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if typ != MessageAwareness {
		t.Errorf("type: want %d got %d", MessageAwareness, typ)
	}
	if got := aw2.GetStates()[aw.ClientID]; got.IsNil() || got.GetOr("name") != "bob" {
		t.Errorf("awareness not applied: states=%v", aw2.GetStates())
	}
}
