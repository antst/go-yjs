package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Sync sub-message types used inside a MessageSync frame. SyncMessageNone is
// reported for non-sync outer messages. Unknown sync sub-message types are
// preserved as their actual non-negative value so a relay can authorize and
// forward extensions it does not itself understand.
const (
	SyncMessageNone   = -1
	SyncMessageStep1  = 0
	SyncMessageStep2  = 1
	SyncMessageUpdate = 2
)

// ErrInvalidSyncMessageType is returned when a sync sub-message type is outside
// the portable supported range [0, math.MaxInt32]. Such a value must not wrap
// and alias a known type or the SyncMessageNone sentinel.
var ErrInvalidSyncMessageType = errors.New("protocol: sync sub-message type out of range")

var errVarUintOverflow = errors.New("protocol: varuint overflows uint64")

// MessageInfo describes one framed protocol message without applying it.
//
// Body is a zero-copy view into the frame passed to InspectMessage. For known
// sync messages it is the decoded state-vector/update body; for awareness it is
// the decoded awareness-update body; and for custom or unknown sync messages it
// is the remaining raw payload after the known type prefixes. The caller owns
// frame, so Body remains valid for as long as the caller keeps frame unchanged.
// InspectMessage does not retain either slice.
type MessageInfo struct {
	Type        uint8
	SyncType    int
	FrameLength int
	BodyLength  int
	Body        []byte
}

type inspectedMessage struct {
	info MessageInfo
}

// InspectMessage classifies and validates one protocol frame without applying
// it. Relays can inspect the returned type and body, run authorization and
// post-apply size gates, and only then pass the original frame to
// SyncHandler.HandleMessageWithOrigin. No document or awareness state is
// mutated, and the body view is returned without allocating or copying.
func InspectMessage(frame []byte) (MessageInfo, error) {
	inspected, err := inspectMessage(frame)
	if err != nil {
		return MessageInfo{}, err
	}
	return inspected.info, nil
}

func inspectMessage(frame []byte) (inspectedMessage, error) {
	msgType, payload, err := readMessageView(frame)
	if err != nil {
		return inspectedMessage{}, err
	}
	return inspectPayload(msgType, payload, len(frame))
}

func inspectPayload(msgType uint8, payload []byte, frameLength int) (inspectedMessage, error) {
	result := inspectedMessage{
		info: MessageInfo{
			Type:        msgType,
			SyncType:    SyncMessageNone,
			FrameLength: frameLength,
		},
	}

	switch msgType {
	case MessageSync:
		offset := 0
		subType, err := readVarUintView(payload, &offset)
		if err != nil {
			if errors.Is(err, errVarUintOverflow) {
				return inspectedMessage{}, fmt.Errorf("%w: %w", ErrInvalidSyncMessageType, err)
			}
			return inspectedMessage{}, fmt.Errorf("protocol: read sync sub-message type: %w", err)
		}
		if subType > math.MaxInt32 {
			return inspectedMessage{}, fmt.Errorf("%w: %d", ErrInvalidSyncMessageType, subType)
		}
		result.info.SyncType = int(subType)

		switch result.info.SyncType {
		case SyncMessageStep1, SyncMessageStep2, SyncMessageUpdate:
			result.info.Body, err = readVarUint8ArrayView(payload, &offset)
			if err != nil {
				return inspectedMessage{}, fmt.Errorf("protocol: read sync sub-message body: %w", err)
			}
		default:
			// y-protocols treats unknown in-range sync subtypes as no-ops. Preserve
			// the actual subtype and remaining bytes so a relay can classify and
			// forward an extension without understanding its private framing.
			result.info.Body = payload[offset:]
		}

	case MessageAwareness:
		offset := 0
		body, err := readVarUint8ArrayView(payload, &offset)
		if err != nil {
			return inspectedMessage{}, fmt.Errorf("protocol: read awareness message body: %w", err)
		}
		result.info.Body = body

	default:
		result.info.Body = payload
	}

	result.info.BodyLength = len(result.info.Body)
	return result, nil
}

// readMessageView parses the generic outer type while retaining a view into the
// caller-owned frame. It mirrors ReadMessage's type bounds and error sentinels
// without constructing a bytes.Buffer on the allocation-sensitive inspect path.
func readMessageView(frame []byte) (uint8, []byte, error) {
	if len(frame) == 0 {
		return 0, nil, ErrShortMessage
	}
	offset := 0
	msgType, err := readVarUintView(frame, &offset)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("%w: %w", ErrShortMessage, err)
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrInvalidMessageType, err)
	}
	if msgType > math.MaxUint8 {
		return 0, nil, ErrInvalidMessageType
	}
	return uint8(msgType), frame[offset:], nil
}

// readVarUint8ArrayView parses the canonical length prefix and returns a view
// into data. It checks the declared length before slicing so a hostile frame is
// reported as truncated instead of panicking.
func readVarUint8ArrayView(data []byte, offset *int) ([]byte, error) {
	size, err := readVarUintView(data, offset)
	if err != nil {
		return nil, err
	}
	remaining := len(data) - *offset
	if size > uint64(remaining) {
		return nil, io.ErrUnexpectedEOF
	}
	start := *offset
	*offset += int(size)
	return data[start:*offset], nil
}

func readVarUintView(data []byte, offset *int) (uint64, error) {
	value, n := binary.Uvarint(data[*offset:])
	switch {
	case n == 0:
		return 0, io.ErrUnexpectedEOF
	case n < 0:
		return 0, errVarUintOverflow
	default:
		*offset += n
		return value, nil
	}
}
