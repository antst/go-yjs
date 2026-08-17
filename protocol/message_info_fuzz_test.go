package protocol

import (
	"bytes"
	"math/rand"
	"testing"
	"unsafe"
)

// Differential fuzz of InspectMessage's hand-rolled zero-allocation cursor
// against the established bytes.Buffer parser in ReadMessage.
//
// InspectMessage parses hostile input with a raw binary.Uvarint slice cursor
// rather than a Buffer, because the inspect path must not allocate — a relay
// runs it on every frame before deciding whether to authorize, gate or forward.
// Hand-rolled bounds arithmetic over attacker-controlled lengths is exactly
// where a parser goes wrong, and the failure mode is a panic or a Body view
// pointing outside the caller's frame.
//
// The targeted tests alongside this one pin specific error classes and
// boundaries. This adds two properties they cannot express case by case:
// agreement with the OTHER parser on the outer prefix across random input, and
// the memory invariant that a zero-copy view must always lie inside the frame it
// was handed.
//
// The round-trip arm at the end exists because the fuzz invariants are
// SELF-consistent: a parser that consumed to end-of-frame instead of the
// declared length satisfies every one of them. Verified by injection — dropping
// the declared-length bound is caught by the pointer-bracket check, and
// consuming past the declared length is caught only by the round-trip arm.
func TestZZInspectMessageDifferentialFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	corpus := [][]byte{
		{}, {0x00}, {0x01}, {0x02}, {0xff}, {0x80}, {0x80, 0x80},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, // uvarint overflow
		{0x00, 0x00},             // sync, subtype 0, no body
		{0x00, 0x00, 0x00},       // sync step1, declared len 0
		{0x00, 0x00, 0x05, 0x01}, // declared 5, only 1 present
		{0x01, 0x00},             // awareness, declared len 0
		{0x01, 0x02, 0xaa, 0xbb}, // awareness, declared 2, exact
		{0x01, 0x01, 0xaa, 0xbb}, // awareness, declared 1, trailing byte
		{0x01, 0xff, 0xaa},       // awareness, declared 255, 1 present
	}
	for i := 0; i < 60000; i++ {
		n := rng.Intn(12)
		b := make([]byte, n)
		for j := range b {
			if rng.Intn(3) == 0 {
				b[j] = byte(rng.Intn(4)) // bias toward small/valid varints
			} else {
				b[j] = byte(rng.Intn(256))
			}
		}
		corpus = append(corpus, b)
	}

	for _, frame := range corpus {
		info, ierr := InspectMessage(frame) // must never panic

		rt, _, rerr := ReadMessage(bytes.NewBuffer(append([]byte(nil), frame...)))

		// INVARIANT 1: when both accept the outer prefix, the type must agree.
		if ierr == nil && rerr == nil && info.Type != rt {
			t.Fatalf("type disagreement on %x: Inspect=%d ReadMessage=%d", frame, info.Type, rt)
		}
		// INVARIANT 2: ReadMessage rejecting the outer prefix means Inspect must too.
		if rerr != nil && ierr == nil {
			t.Fatalf("Inspect ACCEPTED %x that ReadMessage rejected (%v)", frame, rerr)
		}
		if ierr != nil {
			continue
		}
		// INVARIANT 3: Body must be a view INSIDE frame — never past the end,
		// never a fresh allocation. This is what a zero-copy view can get wrong.
		if len(info.Body) > 0 {
			if len(frame) == 0 {
				t.Fatalf("non-empty Body from empty frame %x", frame)
			}
			base := uintptr(unsafe.Pointer(&frame[0]))
			bodyStart := uintptr(unsafe.Pointer(&info.Body[0]))
			bodyEnd := bodyStart + uintptr(len(info.Body))
			if bodyStart < base || bodyEnd > base+uintptr(len(frame)) {
				t.Fatalf("Body escapes frame on %x: body=[%d,%d) frame=[%d,%d)",
					frame, bodyStart, bodyEnd, base, base+uintptr(len(frame)))
			}
		}
		// INVARIANT 4: reported lengths must be self-consistent.
		if info.FrameLength != len(frame) {
			t.Fatalf("FrameLength=%d want %d on %x", info.FrameLength, len(frame), frame)
		}
		if info.BodyLength != len(info.Body) {
			t.Fatalf("BodyLength=%d but len(Body)=%d on %x", info.BodyLength, len(info.Body), frame)
		}
	}

	// ROUND-TRIP ARM. The invariants above are self-consistent, so a parser that
	// consumed to end-of-frame instead of the declared length would satisfy them
	// all. Build frames whose body is KNOWN and require it back exactly, with any
	// trailing bytes excluded.
	for _, tc := range []struct {
		name    string
		msgType uint8
		body    []byte
		trailer []byte
	}{
		{"awareness exact", MessageAwareness, []byte{0xaa, 0xbb}, nil},
		{"awareness trailing", MessageAwareness, []byte{0xaa}, []byte{0xbb, 0xcc}},
		{"awareness empty", MessageAwareness, []byte{}, nil},
		{"awareness empty+trailing", MessageAwareness, []byte{}, []byte{0x99}},
	} {
		frame := append(EncodeAwarenessUpdateMessage(tc.body), tc.trailer...)
		info, err := InspectMessage(frame)
		if len(tc.trailer) > 0 {
			// A trailing tail is a malformed frame OR must be excluded from Body;
			// either is acceptable, silently swallowing it into Body is not.
			if err == nil && !bytes.Equal(info.Body, tc.body) {
				t.Fatalf("%s: Body=%x swallowed the trailer, want %x", tc.name, info.Body, tc.body)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: InspectMessage rejected a self-encoded frame: %v", tc.name, err)
		}
		if !bytes.Equal(info.Body, tc.body) {
			t.Fatalf("%s: Body=%x want exactly %x", tc.name, info.Body, tc.body)
		}
	}
}
