package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// --- Round-2 finding (Copilot): ReadMessage discarded the underlying varuint
// decode error and always returned ErrShortMessage, erasing the cause. The fix
// wraps the cause (fmt.Errorf("%w: %w", ErrShortMessage, underlying)) so callers
// still match errors.Is(err, ErrShortMessage) AND can retrieve the real cause. ---

func TestRegressionReadMessagePreservesVaruintCause(t *testing.T) {
	// A single 0x80 byte: the type VarUint has its continuation bit set but no
	// following byte, so binary.ReadUvarint fails with io.EOF mid-decode. (buf.Len()
	// is 1, so the len==0 short-circuit does not fire — we exercise the decode-error
	// path specifically.)
	buf := bytes.NewBuffer([]byte{0x80})
	_, _, err := ReadMessage(buf)
	if err == nil {
		t.Fatalf("ReadMessage: expected error on truncated type varuint, got nil")
	}
	// (a) callers keying off the sentinel still work.
	if !errors.Is(err, ErrShortMessage) {
		t.Fatalf("ReadMessage: errors.Is(err, ErrShortMessage) must hold; got %v", err)
	}
	// (b) the underlying cause is no longer discarded — it is retrievable. A varint
	// with its continuation bit set but no following byte is a *partial* read, so
	// binary.ReadUvarint reports io.ErrUnexpectedEOF (not a clean io.EOF).
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadMessage: underlying cause must be retrievable (errors.Is io.ErrUnexpectedEOF); got %v", err)
	}
	// And the two are genuinely distinct errors (the wrap did not collapse them).
	if errors.Is(ErrShortMessage, io.ErrUnexpectedEOF) {
		t.Fatalf("sanity: ErrShortMessage must not itself be io.ErrUnexpectedEOF")
	}
}

// The empty-buffer path returns the bare sentinel (no underlying cause to wrap):
// errors.Is(err, ErrShortMessage) must still hold.
func TestRegressionReadMessageEmptyStillSentinel(t *testing.T) {
	_, _, err := ReadMessage(bytes.NewBuffer(nil))
	if !errors.Is(err, ErrShortMessage) {
		t.Fatalf("ReadMessage(empty): want errors.Is ErrShortMessage, got %v", err)
	}
}

// --- Round-6 finding (Copilot): ReadMessage wrapped ALL varuint decode errors as
// ErrShortMessage, including a complete-but-overflowing type prefix — which is an
// out-of-range type, not a short frame (ErrShortMessage's doc says "no type byte").
// The fix reserves ErrShortMessage for a truncated/empty frame (io.EOF /
// io.ErrUnexpectedEOF) and classifies an overflowing type prefix as
// ErrInvalidMessageType, matching the t > 255 out-of-range treatment. ---
func TestRegressionReadMessageOverflowTypeIsInvalidNotShort(t *testing.T) {
	// Ten 0x80 bytes: a type varuint whose continuation bit is set on every byte, so
	// it is fully present (not truncated) yet overflows uint64. binary.ReadUvarint
	// consumes all ten bytes and reports overflow — NOT an io.EOF-family error.
	buf := bytes.NewBuffer([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	_, _, err := ReadMessage(buf)
	if err == nil {
		t.Fatalf("ReadMessage: expected error on overflowing type varuint, got nil")
	}
	// (a) It is classified as an out-of-range type, the same family as t > 255.
	if !errors.Is(err, ErrInvalidMessageType) {
		t.Fatalf("ReadMessage(overflow type): want errors.Is ErrInvalidMessageType; got %v", err)
	}
	// (b) It must NOT be misclassified as a short frame — that is the bug being fixed.
	if errors.Is(err, ErrShortMessage) {
		t.Fatalf("ReadMessage(overflow type): must NOT be ErrShortMessage; got %v", err)
	}
	// (c) The overflow is a complete-but-invalid prefix, not a truncation, so the cause
	// is not an io.EOF-family error (this is what distinguishes it at runtime).
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		t.Fatalf("ReadMessage(overflow type): overflow cause must not be EOF-family; got %v", err)
	}
}

// --- Finding #11: EncodeAwarenessMessage must derive a monotonic clock so that
// successive broadcasts for the same client are not dropped by the receiver's
// newer-clock rule. ---

func TestRegressionAwarenessMessageMonotonicClock(t *testing.T) {
	clientID := crdt.Number(7777)
	clocks := []uint64{}
	for i := 0; i < 4; i++ {
		payload := EncodeAwarenessMessage(clientID, crdt.MakeObject("n", i))
		// payload = [count=1][clientID][clock][state]; read the clock field.
		dec := bytes.NewBuffer(payload)
		_, _ = lib0.ReadVarUint(dec) // count
		_, _ = lib0.ReadVarUint(dec) // clientID
		clock, err := lib0.ReadVarUint(dec)
		if err != nil {
			t.Fatal(err)
		}
		clocks = append(clocks, clock)
	}
	for i := 1; i < len(clocks); i++ {
		if clocks[i] <= clocks[i-1] {
			t.Fatalf("awareness clock not strictly increasing (finding #11): %v", clocks)
		}
	}

	// And the broadcasts must actually apply in sequence to a receiving Awareness
	// (the second/third are not silently dropped).
	doc := crdt.NewDoc("g")
	aw := crdt.NewAwareness(doc)
	for i := 0; i < 4; i++ {
		payload := EncodeAwarenessMessage(clientID, crdt.MakeObject("n", i))
		// ApplyAwarenessUpdate returns an error; assert it succeeds so a decode/
		// validation failure fails the test loudly instead of silently applying
		// nothing and tripping the convergence check below with a cryptic message.
		if err := crdt.ApplyAwarenessUpdate(aw, payload, "remote"); err != nil {
			t.Fatalf("ApplyAwarenessUpdate broadcast %d failed: %v", i, err)
		}
	}
	got := aw.GetStates()[clientID]
	if got.IsNil() {
		t.Fatalf("awareness state not applied at all")
	}
	// The last update wins; n should be 3 (float64 after JSON round-trip).
	if v, ok := got.GetOr("n").(float64); !ok || v != 3 {
		t.Fatalf("awareness did not converge to last broadcast: got %v", got.GetOr("n"))
	}
}

// --- Finding #12: a malformed/truncated sync message must not panic the
// handler; it must return an error. ---

func TestRegressionMalformedSyncNoPanic(t *testing.T) {
	doc := crdt.NewDoc("g")
	h := NewSyncHandler(doc)

	// Frame a MessageSync whose payload is a truncated SyncStep2 (type byte 1 =
	// SyncStep2, then a bogus length-prefixed update).
	syncPayload := []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 1, 2, 3}
	var framed bytes.Buffer
	WriteMessage(&framed, MessageSync, syncPayload)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HandleMessage panicked on malformed sync (finding #12): %v", r)
		}
	}()
	var reply bytes.Buffer
	// Either a clean (type, nil-err) or (type, err) is acceptable; a panic is not.
	_, _ = h.HandleMessage(framed.Bytes(), &reply)
}

func TestRegressionMalformedSyncFuzz(t *testing.T) {
	doc := crdt.NewDoc("g")
	h := NewSyncHandler(doc)
	garbages := [][]byte{
		{0, 1, 1},
		{0, 0xff, 0x7f},
		{0, 3, 1, 0xff, 0xff},
		{0, 5, 2, 0xff, 0xff, 0xff, 0xff},
	}
	for i, payload := range garbages {
		var framed bytes.Buffer
		WriteMessage(&framed, MessageSync, payload)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: HandleMessage panicked: %v", i, r)
				}
			}()
			var reply bytes.Buffer
			_, _ = h.HandleMessage(framed.Bytes(), &reply)
		}()
	}
}

// --- Finding #13: framing stays CANONICAL y-protocols ([type][payload], the
// transport frames each message — one WS binary frame per message), and rejects
// out-of-range message types instead of truncating them into MessageSync. The
// earlier length-prefixed "self-delimiting" variant was reverted because it broke
// interop with canonical y-protocols/y-websocket clients. ---

func TestRegressionFramingCanonical(t *testing.T) {
	// One message per buffer (as the transport frames it): type then raw payload.
	var buf bytes.Buffer
	WriteMessage(&buf, MessageSync, []byte{1, 2, 3})
	// Canonical wire = VarUint(type) + raw payload, no in-band length prefix.
	if want := []byte{MessageSync, 1, 2, 3}; !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("canonical frame: want %v got %v", want, buf.Bytes())
	}
	typ, payload, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if typ != MessageSync || !bytes.Equal(payload, []byte{1, 2, 3}) {
		t.Fatalf("frame: type %d payload %v", typ, payload)
	}
}

func TestRegressionFramingRejectsOutOfRangeType(t *testing.T) {
	// A type field of 256 must be rejected, not truncated to 0 (MessageSync).
	var buf bytes.Buffer
	lib0.WriteVarUint(&buf, 256) // out-of-range type
	buf.Write([]byte{1, 2})      // payload

	_, _, err := ReadMessage(&buf)
	if !errors.Is(err, ErrInvalidMessageType) {
		t.Fatalf("ReadMessage: want ErrInvalidMessageType, got %v (finding #13)", err)
	}
}

// --- Round-3 finding: EncodeSyncStep2 was threaded to RETURN an error (so a
// failed encode is surfaced rather than framed as a misleadingly-complete empty
// SyncStep2), but it has no in-tree callers and had no test on the new path — a
// regression that swallowed the error would have passed CI silently. Pin both
// arms of the contract:
//
//   - a well-formed doc returns framed bytes + nil error;
//   - a malformed input (a truncated encoded state vector, which fails to decode
//     deep in EncodeStateAsUpdate -> WriteSyncStep2) surfaces a non-nil error
//     and NO bytes — it is not swallowed into an empty/"synced" frame.
//
// SyncHandler.HandleMessageWithOrigin now routes its canonical reply path
// through EncodeSyncStep2, so this guard also pins that a malformed Step1 cannot
// be framed as a misleadingly-complete reply.
func TestRegressionEncodeSyncStep2SurfacesError(t *testing.T) {
	doc := crdt.NewDoc("g")
	doc.GetText("t").Insert(0, "hello", crdt.Object{})

	// Well-formed: a nil (empty) state vector is valid — the remote is missing
	// everything — and must yield a non-empty framed SyncStep2 with no error.
	out, err := EncodeSyncStep2(doc, nil)
	if err != nil {
		t.Fatalf("well-formed EncodeSyncStep2: unexpected error: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("well-formed EncodeSyncStep2: expected framed bytes, got empty")
	}
	if out[0] != MessageSync {
		t.Fatalf("well-formed EncodeSyncStep2: expected MessageSync framing, got type byte %d", out[0])
	}

	// Malformed: this state vector declares one client entry (leading 0x01) but is
	// truncated before that entry's client/clock VarUints, so decodeStateVector
	// fails inside EncodeStateAsUpdate. The error must propagate out, with no
	// bytes — never be swallowed into a 0-length "complete" SyncStep2.
	badSV := []byte{0x01}
	out, err = EncodeSyncStep2(doc, badSV)
	if err == nil {
		t.Fatalf("malformed EncodeSyncStep2: expected non-nil error, got nil (bytes=%v)", out)
	}
	if out != nil {
		t.Fatalf("malformed EncodeSyncStep2: expected nil bytes on error, got %v", out)
	}
}
