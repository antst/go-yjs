package crdt

import "math"

// readSyncMessageForTest preserves the retired root sync parser only for the
// legacy core tests. Production protocol dispatch lives in protocol.SyncHandler,
// which surfaces malformed input instead of logging and swallowing it.
func readSyncMessageForTest(decoder updateDecoder, encoder updateEncoder, doc *Doc, transactionOrigin any) int {
	messageType, err := readVarUint(decoder.restDecoder())
	if err != nil || messageType > math.MaxInt32 {
		return -1
	}

	switch messageType {
	case messageYjsSyncStep1:
		data, err := readVarUint8Array(decoder.restDecoder())
		if err == nil {
			_ = writeSyncStep2(encoder, doc, data.([]byte))
		}
	case messageYjsSyncStep2, messageYjsUpdate:
		data, err := readVarUint8Array(decoder.restDecoder())
		if err == nil {
			_ = ApplyUpdate(doc, data.([]byte), transactionOrigin)
		}
	}

	return int(messageType)
}
