package crdt

import (
	"bytes"
	"testing"
)

// genericUpdateEncoderV2 deliberately changes the encoder's dynamic type so
// WriteStructs takes the generic IAbstractStruct.Write path. It lets the focused
// test compare the full-state specialization against the established encoder,
// independently of the Node differential gate.
type genericUpdateEncoderV2 struct{ *updateEncoderV2 }

func encodeStateGenericV2(doc *Doc) ([]byte, error) {
	encoder := &genericUpdateEncoderV2{newDefaultUpdateEncoderV2()}
	if err := writeStateAsUpdate(encoder, doc, map[Number]Number{}); err != nil {
		return nil, err
	}
	out := encoder.toBytes()
	return out, encoder.encodeError()
}

func TestFullStateV2FastPathMatchesGenericEncoder(t *testing.T) {
	build := func(client Number) *Doc {
		doc := newDoc("v2-fast", false, defaultGCFilter, nil, false, WithClientID(client))
		attrs := newObject()
		attrs.Set("bold", true)
		attrs.Set("lang", "nl")
		text := doc.GetText("t")
		text.Insert(0, "a😀βc", attrs)
		text.Delete(1, 2)
		doc.GetArray("a").Insert(0, ArrayAny{"x", int64(2), true})
		m := doc.GetMap("m")
		obj := newObject()
		obj.Set("first", 1)
		obj.Set("second", "two")
		m.Set("object", obj)
		m.Set("deleted", "value")
		m.Delete("deleted")
		return doc
	}

	docs := make([]*Doc, 0, 3)
	docs = append(docs, build(1))
	left := build(11)
	right := build(99)
	right.GetText("remote").Insert(0, "remote", newObject())
	merged := newDoc("v2-fast", false, defaultGCFilter, nil, false, WithClientID(7))
	leftUpdate, err := EncodeStateAsUpdateV2(left, nil)
	if err != nil {
		t.Fatal(err)
	}
	rightUpdate, err := EncodeStateAsUpdateV2(right, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(merged, leftUpdate, nil)
	_ = ApplyUpdateV2(merged, rightUpdate, nil)
	docs = append(docs, merged)

	for i, doc := range docs {
		got, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("case %d fast encode: %v", i, err)
		}
		want, err := encodeStateGenericV2(doc)
		if err != nil {
			t.Fatalf("case %d generic encode: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("case %d fast V2 bytes differ from generic encoder", i)
		}
	}
}

func TestFullStateV2PoolDoesNotAliasReturnedUpdates(t *testing.T) {
	first := newDoc("pool", false, defaultGCFilter, nil, false, WithClientID(1))
	first.GetText("t").Insert(0, "first", newObject())
	firstUpdate, err := EncodeStateAsUpdateV2(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(firstUpdate)

	for i := 0; i < 20; i++ {
		next := newDoc("pool", false, defaultGCFilter, nil, false, WithClientID(i+2))
		next.GetText("t").Insert(0, "a different document with a longer payload", newObject())
		if _, err := EncodeStateAsUpdateV2(next, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(firstUpdate, want) {
		t.Fatal("a later pooled encode mutated an update already returned to the caller")
	}
}

func TestFullStateV2TrustedClockRejectsOverflow(t *testing.T) {
	encoder := newDefaultUpdateEncoderV2()
	encoder.trustedFullState = true
	clockEncoder := encoder.leftClockEncoder
	clockEncoder.s = 0
	clockEncoder.count = 1
	clockEncoder.diff = 0

	writeTrustedClock(encoder, clockEncoder, maxIntDiffOptRleDiff+1)
	if encoder.encodeError() == nil {
		t.Fatal("trusted live-store clock silently accepted a diff that overflows V2 framing")
	}
}
