package crdt

import (
	"testing"
	"unsafe"
)

// Every V2 numeric column must still be a view into the caller's buffer once the
// whole decoder is built, not merely at the moment its own column was framed.
//
// WHY THIS IS NOT COVERED BY THE SINGLE-COLUMN VIEW TEST. readV2Column hands back
// bytes.Buffer.Next, whose contract says the returned slice "is only valid until
// the next call to a read or write method". The constructor then calls Next eight
// more times. Every column except the last is therefore held across the exact
// event the documentation names, and the code is correct only because Buffer's
// reads advance an offset rather than compacting — an implementation property, not
// a promised one. TestReadV2ColumnReturnsInputView frames one column and stops, so
// it cannot see this; the allocation ceiling catches a reintroduced copy but says
// nothing about whether a retained slice still points where it did.
//
// Stating it as a test converts the assumption into something that fails loudly if
// it ever stops holding, instead of silently decoding another column's bytes.
//
// The comparison is done in uintptr space on purpose. Computing base+len as an
// unsafe.Pointer to bound-check a slice is exactly the out-of-bounds-pointer
// mistake that cost a checkptr fatal elsewhere in this package: the end of the
// last column is one past the end of the update, and forming that pointer is
// undefined precisely when the column sits at the tail.
func TestUpdateDecoderV2ColumnsStillAliasInputAfterFullConstruction(t *testing.T) {
	doc := newDoc("cols", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "alpha bravo charlie delta echo foxtrot", Object{})
	attrs := newObject()
	attrs.Set("bold", true)
	txt.Format(3, 9, attrs)
	arr := doc.GetArray("a")
	for i := 0; i < 24; i++ {
		arr.Push(ArrayAny{i})
	}
	m := doc.GetMap("m")
	for i := 0; i < 24; i++ {
		m.Set(string(rune('a'+i)), i)
	}

	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	lo := uintptr(unsafe.Pointer(unsafe.SliceData(update)))
	hi := lo + uintptr(len(update))

	d := newUpdateDecoderV2(update)
	columns := map[string][]byte{
		"keyClock":   d.keyClockDecoder.buf.Bytes(),
		"client":     d.clientDecoder.buf.Bytes(),
		"leftClock":  d.leftClockDecoder.buf.Bytes(),
		"rightClock": d.rightClockDecoder.buf.Bytes(),
		"info":       d.infoDecoder.buf.Bytes(),
		"parentInfo": d.parentInfoDecoder.buf.Bytes(),
		"typeRef":    d.typeRefDecoder.buf.Bytes(),
		"length":     d.lenDecoder.buf.Bytes(),
	}

	populated := 0
	for name, col := range columns {
		if len(col) == 0 {
			continue
		}
		populated++
		at := uintptr(unsafe.Pointer(unsafe.SliceData(col)))
		if at < lo || at >= hi {
			t.Errorf("column %q no longer views the input buffer: it starts outside [%#x,%#x). "+
				"Either a copy was reintroduced, or a slice framed early in the constructor was "+
				"invalidated by a later read of the same bytes.Buffer", name, lo, hi)
		}
	}

	// A fixture where most columns came back empty would satisfy the loop above
	// without testing anything, and V2 legitimately leaves some columns empty.
	if populated < 5 {
		t.Fatalf("only %d of %d numeric columns carried data; this fixture cannot demonstrate "+
			"that retained column views survive the constructor's later reads", populated, len(columns))
	}

	// The first column is the one held across the most subsequent reads, so it is
	// the one that would break first. Pin it by name rather than relying on map order.
	if kc := columns["keyClock"]; len(kc) > 0 {
		at := uintptr(unsafe.Pointer(unsafe.SliceData(kc)))
		if at < lo || at >= hi {
			t.Error("the keyClock column, framed before eight further reads, is no longer a view " +
				"into the input")
		}
	}

	// Aliasing must not have cost correctness: the decoder still has to produce the
	// same document as a decode from a private copy of the same bytes.
	fromView, err := decodeUpdateV2(update)
	if err != nil {
		t.Fatal(err)
	}
	fromCopy, err := decodeUpdateV2(append([]byte(nil), update...))
	if err != nil {
		t.Fatal(err)
	}
	if len(fromView.structs) != len(fromCopy.structs) || len(fromView.structs) == 0 {
		t.Fatalf("view decode produced %d structs, copy decode %d", len(fromView.structs), len(fromCopy.structs))
	}
}
