package crdt

import (
	"bytes"
	"testing"
)

func TestUnobservedTailAppendFastPathMatchesObservedPath(t *testing.T) {
	newText := func(observed bool) (*Doc, *YText) {
		doc := newDoc("tail-append", false, defaultGCFilter, nil, false, WithClientID(17))
		text := doc.GetText("t")
		if observed {
			text.Observe(func(interface{}, interface{}) {})
		}
		return doc, text
	}

	fastDoc, fastText := newText(false)
	referenceDoc, referenceText := newText(true)
	for _, chunk := range []string{"a", "😀", "é", "bc", "世界"} {
		fastText.Insert(fastText.Length(), chunk, Object{})
		referenceText.Insert(referenceText.Length(), chunk, Object{})
	}
	// Cross several append-buffer growth boundaries; every growth must preserve
	// both the old visible bytes and the single-item wire shape.
	for i := 0; i < 512; i++ {
		fastText.Insert(fastText.Length(), "x", Object{})
		referenceText.Insert(referenceText.Length(), "x", Object{})
	}

	if fastText.ToString() != referenceText.ToString() {
		t.Fatalf("text differs: fast %q, observed %q", fastText.ToString(), referenceText.ToString())
	}
	fastV1, err := EncodeStateAsUpdate(fastDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	referenceV1, err := EncodeStateAsUpdate(referenceDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fastV1, referenceV1) {
		t.Fatalf("V1 update differs:\nfast     %x\nobserved %x", fastV1, referenceV1)
	}
	fastV2, err := EncodeStateAsUpdateV2(fastDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	referenceV2, err := EncodeStateAsUpdateV2(referenceDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fastV2, referenceV2) {
		t.Fatalf("V2 update differs:\nfast     %x\nobserved %x", fastV2, referenceV2)
	}
}

func TestTailAppendBufferPreservesOldStringsAndRejectsDirectReplacement(t *testing.T) {
	doc := newDoc("tail-buffer", false, defaultGCFilter, nil, false, WithClientID(23))
	text := doc.GetText("t")
	text.Insert(0, "a", Object{})
	content, ok := text.start.content.(*contentString)
	if !ok {
		t.Fatalf("first content has type %T, want *ContentString", text.start.content)
	}

	text.Insert(text.Length(), "b", Object{}) // creates the append buffer
	retained := content.value
	text.Insert(text.Length(), "c", Object{}) // appends inside its spare capacity
	if retained != "ab" || content.value != "abc" {
		t.Fatalf("append mutated a retained string: old=%q current=%q", retained, content.value)
	}

	// Str is exported. Replacing it with an equal-length value must invalidate
	// the private append buffer rather than resurrecting its old bytes.
	content.value = "XYZ"
	text.Insert(text.Length(), "!", Object{})
	if got := text.ToString(); got != "XYZ!" {
		t.Fatalf("append reused stale backing after direct Str replacement: %q", got)
	}
	if retained != "ab" {
		t.Fatalf("direct replacement or later append changed retained string: %q", retained)
	}
}

func TestTailAppendBufferReleasedWhenTextStopsBeingASoleLiveRun(t *testing.T) {
	text := newDoc("tail-release", false, defaultGCFilter, nil, false, WithClientID(29)).GetText("t")
	text.Insert(0, "a", Object{})
	text.Insert(1, "bc", Object{})
	if text.tailAppend == nil || len(text.tailAppend.buf) == 0 {
		t.Fatal("test setup did not create a tail append buffer")
	}
	text.Delete(0, text.Length())
	if text.tailAppend != nil {
		t.Fatal("deleted text retained its obsolete tail append buffer")
	}
}
