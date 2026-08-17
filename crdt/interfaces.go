package crdt

import "bytes"

type restVarUintEncoder interface {
	writeRestVarUint(value uint64)
}

// writeEncoderRestVarUint keeps raw framing on the rest stream for both codec versions while
// allowing the internal V1 full-update encoder to append without exposing a bytes.Buffer.
func writeEncoderRestVarUint(encoder dsEncoder, value uint64) {
	if fast, ok := encoder.(restVarUintEncoder); ok {
		fast.writeRestVarUint(value)
		return
	}
	writeVarUint(encoder.restEncoder(), value)
}

// interfaces.go defines the shared encoder/decoder interfaces that let the sync
// protocol, struct/content writers and update merge logic operate over either
// the V1 or the V2 wire format. Both *UpdateEncoderV1/*UpdateEncoderV2 and
// *UpdateDecoderV1/*UpdateDecoderV2 satisfy these.
//
// Per the spec clarification this is a breaking API change: functions that used
// to accept *UpdateEncoderV1 / *UpdateDecoderV1 now accept the interfaces.
//
// GetRestEncoder / GetRestDecoder expose the underlying "rest" buffer. Yjs
// addresses this buffer directly (encoder.restEncoder) for raw VarUint framing
// that is identical across V1 and V2; Go cannot reach an embedded field through
// an interface, so these accessors stand in for that access.

// dsEncoder is the delete-set encoding surface shared by V1 and V2.
type dsEncoder interface {
	toBytes() []uint8
	// encodeError reports a deferred encode error captured while writing (e.g. a V2
	// clock-column diff overflow), or nil. The struct/content write methods
	// cannot return an error, so callers check encodeError() after ToUint8Array to
	// surface a failed encode instead of emitting a truncated update. V1 always
	// returns nil.
	encodeError() error
	resetDS()
	writeDSClock(clock Number)
	writeDSLength(length Number) error
	restEncoder() *bytes.Buffer
	// restartRestEncoder returns the current rest-buffer bytes and installs a
	// fresh empty rest buffer, leaving any V2 column sub-encoders intact. The
	// lazy struct writer uses it to capture one client fragment at a time
	// (matching Yjs flushLazyStructWriter, which only resets restEncoder).
	restartRestEncoder() []uint8
}

// dsDecoder is the delete-set decoding surface shared by V1 and V2.
type dsDecoder interface {
	resetDS()
	readDSClock() (Number, error)
	readDSLength() (Number, error)
	restDecoder() *bytes.Buffer
}

// updateEncoder is the struct/content encoding surface shared by V1 and V2.
type updateEncoder interface {
	dsEncoder
	writeID(id *ID)
	writeLeftID(id *ID)
	writeRightID(id *ID)
	writeClient(client Number)
	writeInfo(info uint8)
	writeStringValue(str string) error
	writeParentInfo(isYKey bool)
	writeTypeRef(info uint8)
	writeLength(length Number)
	writeAnyValue(any any) error
	writeBuffer(buf []uint8) error
	writeJSONValue(embed interface{}) error
	writeKey(key string) error
}

// updateDecoder is the struct/content decoding surface shared by V1 and V2.
type updateDecoder interface {
	dsDecoder
	readID() (*ID, error)
	readLeftID() (*ID, error)
	readRightID() (*ID, error)
	readClient() (Number, error)
	readInfo() (uint8, error)
	readStringValue() (string, error)
	readParentInfo() (bool, error)
	readTypeRef() (uint8, error)
	readLength() (Number, error)
	readAnyValue() (any, error)
	readBuffer() ([]uint8, error)
	readJSONValue() (interface{}, error)
	readKey() (string, error)
}

// Compile-time assertions that the V1 types satisfy the interfaces. The V2
// assertions live in update_encoder_v2.go / update_decoder_v2.go.
var (
	_ updateEncoder = (*updateEncoderV1)(nil)
	_ updateDecoder = (*updateDecoderV1)(nil)
)

// Interface-typed factory adapters. Go function values are invariant in their
// return type, so `NewUpdateEncoderV1` (returning *UpdateEncoderV1) cannot be
// passed where a `func() UpdateEncoder` is expected. These adapters bridge that
// for the YEncoder/YDecoder factory parameters of the merge/diff helpers.

// newEncoderV1 returns a fresh V1 update encoder as the UpdateEncoder interface.
func newEncoderV1() updateEncoder { return newUpdateEncoderV1() }

// newDecoderV1 wraps buf in a V1 update decoder as the UpdateDecoder interface.
func newDecoderV1(buf []byte) updateDecoder { return newUpdateDecoderV1(buf) }

// newEncoderV2 returns a fresh V2 update encoder as the UpdateEncoder interface.
func newEncoderV2() updateEncoder { return newDefaultUpdateEncoderV2() }

// newDecoderV2 wraps buf in a V2 update decoder as the UpdateDecoder interface.
func newDecoderV2(buf []byte) updateDecoder { return newUpdateDecoderV2(buf) }
