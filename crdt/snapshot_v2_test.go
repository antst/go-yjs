package crdt

import (
	"encoding/base64"
	"testing"
)

// US7 / FR-027..FR-029 (work item 1.4). Snapshots must encode/decode in BOTH V1
// and V2 and round-trip; V2 snapshot bytes must equal yjs and a yjs-produced V2
// snapshot must decode. Regression: DecodeSnapshotV2 hardcoded the V1 decoder, so
// a real V2 snapshot could not round-trip, and there was no V2 encode path.
// Reference generated from yjs@13.6.31 (Y.snapshot + encodeSnapshot{,V2}).

func buildSnapshotDoc() *Snapshot {
	doc := newDoc("test", false, nil, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "abc", Object{})
	txt.Delete(1, 1) // -> "ac", deleted range {clock:1,len:1}
	return NewSnapshotByDoc(doc)
}

func TestSnapshotV1V2BytesAndRoundTrip(t *testing.T) {
	snap := buildSnapshotDoc()

	const wantV2 = "AQEBAQABAQM=" // yjs encodeSnapshotV2
	const wantV1 = "AQEBAQEBAQM=" // yjs encodeSnapshot (V1)

	v2, err := EncodeSnapshotV2(snap)
	if err != nil {
		t.Fatalf("EncodeSnapshotV2: %v", err)
	}
	if got := base64.StdEncoding.EncodeToString(v2); got != wantV2 {
		t.Errorf("EncodeSnapshotV2 = %q, want yjs %q", got, wantV2)
	}

	v1, err := EncodeSnapshotV1(snap)
	if err != nil {
		t.Fatalf("EncodeSnapshotV1: %v", err)
	}
	if got := base64.StdEncoding.EncodeToString(v1); got != wantV1 {
		t.Errorf("EncodeSnapshotV1 = %q, want yjs %q", got, wantV1)
	}

	// EncodeSnapshot/DecodeSnapshot default to V1 (the wire-compatible default).
	if def, err := EncodeSnapshot(snap); err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	} else if base64.StdEncoding.EncodeToString(def) != wantV1 {
		t.Error("EncodeSnapshot default is not V1")
	}

	// round-trips — incl. the default DecodeSnapshot path (must still route to V1)
	if rt, err := DecodeSnapshot(v1); err != nil || !EqualSnapshots(rt, snap) {
		t.Errorf("default DecodeSnapshot round-trip failed: err=%v equal=%v", err, err == nil && EqualSnapshots(rt, snap))
	}
	if rt, err := DecodeSnapshotV1(v1); err != nil || !EqualSnapshots(rt, snap) {
		t.Errorf("V1 round-trip failed: err=%v equal=%v", err, err == nil && EqualSnapshots(rt, snap))
	}
	if rt, err := DecodeSnapshotV2(v2); err != nil || !EqualSnapshots(rt, snap) {
		t.Errorf("V2 round-trip failed: err=%v", err)
	}

	// truncating the trailing state-vector bytes must error (decodeSnapshot SV-read path)
	if _, err := DecodeSnapshotV1(v1[:len(v1)-1]); err == nil {
		t.Error("DecodeSnapshotV1 on SV-truncated blob: expected an error")
	}

	// decode a yjs-produced V2 snapshot
	yjsV2, _ := base64.StdEncoding.DecodeString(wantV2)
	decoded, err := DecodeSnapshotV2(yjsV2)
	if err != nil {
		t.Fatalf("DecodeSnapshotV2(yjs blob): %v", err)
	}
	if !EqualSnapshots(decoded, snap) {
		t.Error("decoded yjs V2 snapshot != local snapshot")
	}
}

func TestSnapshotRestore(t *testing.T) {
	doc := newDoc("test", false, nil, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "abc", Object{})
	txt.Delete(1, 1)
	snap := NewSnapshotByDoc(doc)

	fresh := newDoc("test", false, nil, nil, false, WithClientID(2))
	restored, err := CreateDocFromSnapshot(doc, snap, fresh)
	if err != nil {
		t.Fatalf("CreateDocFromSnapshot: %v", err)
	}
	if got := restored.GetText("t").ToString(); got != "ac" {
		t.Errorf("restored text = %q, want %q", got, "ac")
	}
}
