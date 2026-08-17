package crdt

import (
	"errors"
	"testing"
)

func TestModifyAwarenessUpdateSurfacesEveryTruncatedVarUint(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "entry count", frame: []byte{0x80}},
		{name: "client id", frame: []byte{1, 0x80, 0x80, 0x80}},
		{name: "clock", frame: []byte{1, 0, 0x80, 0x80}},
		{name: "state", frame: []byte{1, 0, 0, 2, '{'}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callbackCalls := 0
			out, err := ModifyAwarenessUpdate(test.frame, func(value interface{}) interface{} {
				callbackCalls++
				return value
			})
			if !errors.Is(err, ErrTruncatedAwarenessFrame) {
				t.Fatalf("error = %v, want ErrTruncatedAwarenessFrame", err)
			}
			if out != nil {
				t.Fatalf("output = %x, want nil on a decode error", out)
			}
			if callbackCalls != 0 {
				t.Fatalf("modify callback ran %d times for a malformed single-entry frame", callbackCalls)
			}
		})
	}
}
