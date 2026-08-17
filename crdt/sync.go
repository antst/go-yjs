package crdt

import (
	"fmt"
)

/*
 * Core Yjs defines two message types:
 * • YjsSyncStep1: Includes the State Set of the sending client. When received, the client should reply with YjsSyncStep2.
 * • YjsSyncStep2: Includes all missing structs and the complete delete set. When received, the client is assured that it
 *   received all information from the remote client.
 *
 * In a peer-to-peer network, you may want to introduce a SyncDone message type. Both parties should initiate the connection
 * with SyncStep1. When a client received SyncStep2, it should reply with SyncDone. When the local client received both
 * SyncStep2 and SyncDone, it is assured that it is synced to the remote client.
 *
 * In a client-server model, you want to handle this differently: The client should initiate the connection with SyncStep1.
 * When the server receives SyncStep1, it should reply with SyncStep2 immediately followed by SyncStep1. The client replies
 * with SyncStep2 when it receives SyncStep1. Optionally the server may send a SyncDone after it received SyncStep2, so the
 *  client knows that the sync is finished.  There are two reasons for this more elaborated sync model: 1. This protocol can
 * easily be implemented on top of http and websockets. 2. The server should only reply to requests, and not initiate them.
 * Therefore it is necesarry that the client initiates the sync.
 *
 * Construction of a message:
 * [messageType : varUint, message definition..]
 *
 * Note: A message does not include information about the room name. This must to be handled by the upper layer protocol!
 *
 * stringify[messageType] stringifies a message definition (messageType is already read from the bufffer)
 */

const (
	messageYjsSyncStep1 = 0
	messageYjsSyncStep2 = 1
	messageYjsUpdate    = 2
)

// Create a sync step 1 message based on the state of the current shared document.
func writeSyncStep1(encoder updateEncoder, doc *Doc) {
	writeVarUint(encoder.restEncoder(), messageYjsSyncStep1)
	sv := encodeStateVectorWith(doc, nil, newUpdateEncoderV1())
	writeVarUint8Array(encoder.restEncoder(), sv)
}

// writeSyncStep1FromUpdate writes a SyncStep1 message carrying the state vector
// extracted from a pre-encoded update. It returns an error if that extraction
// fails: a cap-breached / malformed update must NOT be written as a
// silently-truncated state vector, which would make the peer believe it holds
// state it does not — mirroring WriteSyncStep2FromUpdate's all-or-nothing
// contract. The frame header is written only after the SV is successfully
// extracted, so a failure leaves no partial SyncStep1 on the wire.
func writeSyncStep1FromUpdate(encoder updateEncoder, update []uint8) error {
	sv, err := encodeStateVectorFromUpdate(update)
	if err != nil {
		return fmt.Errorf("write sync step1 from update: %w", err)
	}
	writeVarUint(encoder.restEncoder(), messageYjsSyncStep1)
	writeVarUint8Array(encoder.restEncoder(), sv)
	return nil
}

// writeSyncStep2 writes a SyncStep2 reply carrying the structs the remote (given
// its state vector) is missing. It returns an error if the document update
// cannot be encoded, so a failed encode is not written as a (misleadingly
// "complete") empty SyncStep2 frame.
func writeSyncStep2(encoder updateEncoder, doc *Doc, encodedStateVector []byte) error {
	update, err := EncodeStateAsUpdate(doc, encodedStateVector)
	if err != nil {
		return fmt.Errorf("write sync step2: %w", err)
	}
	writeVarUint(encoder.restEncoder(), messageYjsSyncStep2)
	writeVarUint8Array(encoder.restEncoder(), update)
	return nil
}

// writeSyncStep2FromUpdate writes a SyncStep2 reply by diffing a pre-encoded
// update against the remote state vector. It returns an error if the diff cannot
// be encoded: an unconvertible/malformed update must NOT be written as a
// 0-length SyncStep2, which would make the peer believe it is fully synced.
func writeSyncStep2FromUpdate(encoder updateEncoder, update []byte, encodedStateVector []byte) error {
	diff, err := DiffUpdate(update, encodedStateVector)
	if err != nil {
		return fmt.Errorf("write sync step2 from update: %w", err)
	}
	writeVarUint(encoder.restEncoder(), messageYjsSyncStep2)
	writeVarUint8Array(encoder.restEncoder(), diff)
	return nil
}

func writeUpdate(encoder updateEncoder, update []byte) {
	writeVarUint(encoder.restEncoder(), messageYjsUpdate)
	writeVarUint8Array(encoder.restEncoder(), update)
}
