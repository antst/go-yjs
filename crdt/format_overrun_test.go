package crdt

import (
	"strings"
	"testing"
)

func TestFormatOverrunAppendsRequiredNewlines(t *testing.T) {
	doc := newDoc("format-overrun", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "x", Object{})
	attrs := newObject()
	attrs.Set("bold", true)

	text.Format(0, 10, attrs)
	if got, want := text.ToString(), "x"+strings.Repeat("\n", 9); got != want {
		t.Fatalf("formatted overrun text = %q, want %q", got, want)
	}

	delta := text.ToDelta(nil, nil, nil)
	if len(delta) != 1 || delta[0].InsertText != "x"+strings.Repeat("\n", 9) || delta[0].Attributes.GetOr("bold") != true {
		t.Fatalf("formatted overrun delta = %#v", delta)
	}
}
