package crdt

import "testing"

// TestQuickstartExamples exercises the public API shapes shown in the spec's
// quickstart.md (T064), confirming they compile and run as documented.
func TestQuickstartExamples(t *testing.T) {
	// --- V2 Encoding ---
	doc := newDoc("guid", true, defaultGCFilter, nil, false)
	text := doc.GetText("content")
	text.Insert(0, "Hello, world!", Object{})
	update := mustBytes(EncodeStateAsUpdateV2(doc, nil))
	if len(update) == 0 {
		t.Fatal("EncodeStateAsUpdateV2 produced empty update")
	}

	// Apply a V2 update from a "JS client" (here, our own V2 update).
	doc2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc2, update, "remote")
	if doc2.GetText("content").ToString() != "Hello, world!" {
		t.Errorf("ApplyUpdateV2: want 'Hello, world!' got %q", doc2.GetText("content").ToString())
	}

	// Compute a differential V2 update.
	sv := encodeStateVectorWith(doc, nil, newUpdateEncoderV1())
	diff := mustBytes(DiffUpdateV2(update, sv))
	if diff == nil {
		t.Error("DiffUpdateV2 returned nil")
	}

	// Convert between formats.
	v1Update := mustBytes(EncodeStateAsUpdate(doc, nil))
	v2FromV1 := mustBytes(ConvertUpdateFormatV1ToV2(v1Update))
	backToV1 := mustBytes(ConvertUpdateFormatV2ToV1(v2FromV1))
	if len(v2FromV1) == 0 || len(backToV1) == 0 {
		t.Error("format conversion produced empty output")
	}

	// --- Encoder/Decoder interfaces ---
	// Sync messages use the V1 encoder: ToUint8Array() returns the canonical
	// y-protocols [msgType][payload] frame, matching protocol.SyncHandler. (A V2
	// encoder's ToUint8Array() would wrap this in V2 update column framing, which
	// is an update container, not a sync frame — see quickstart.md.)
	encoder := newUpdateEncoderV1()
	writeSyncStep1(encoder, doc)
	out := encoder.toBytes()
	if len(out) == 0 {
		t.Error("WriteSyncStep1 via V1 encoder produced empty output")
	}

	// The matching V1 decoder reads it back through the generic ReadSyncMessage.
	dec := newUpdateDecoderV1(out)
	respEnc := newUpdateEncoderV1()
	_ = readSyncMessageForTest(dec, respEnc, doc2, nil)
}
