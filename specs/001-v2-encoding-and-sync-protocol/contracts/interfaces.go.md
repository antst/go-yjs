# Contract: Public Interfaces

These interfaces define the public API contract for V1/V2 encoder
and decoder interchangeability.

## UpdateEncoder Interface

```go
type UpdateEncoder interface {
    DSEncoder
    WriteID(id *ID)
    WriteLeftID(id *ID)
    WriteRightID(id *ID)
    WriteClient(client Number)
    WriteInfo(info uint8)
    WriteString(str string) error
    WriteParentInfo(isYKey bool)
    WriteTypeRef(info uint8)
    WriteLen(length Number)
    WriteAny(any any) error
    WriteBuf(buf []uint8) error
    WriteJson(embed interface{}) error
    WriteKey(key string) error
}
```

`WriteAny` and `WriteBuf` return `error` (the V2 Any/Buf column codecs can fail
on a hostile/over-range value); V1 always returns nil. The struct/content
writers thread these errors up so an encode failure surfaces as an error rather
than emitting a truncated update.

## UpdateDecoder Interface

```go
type UpdateDecoder interface {
    DSDecoder
    ReadID() (*ID, error)
    ReadLeftID() (*ID, error)
    ReadRightID() (*ID, error)
    ReadClient() (Number, error)
    ReadInfo() (uint8, error)
    ReadString() (string, error)
    ReadParentInfo() (bool, error)
    ReadTypeRef() (uint8, error)
    ReadLen() (Number, error)
    ReadAny() (any, error)
    ReadBuf() ([]uint8, error)
    ReadJson() (interface{}, error)
    ReadKey() (string, error)
}
```

## DSEncoder Interface

```go
type DSEncoder interface {
    ToUint8Array() []uint8
    // Error reports a deferred encode error captured while writing (e.g. a V2
    // clock-column diff overflow), or nil. The struct/content write methods that
    // cannot return an error are checked via Error() after ToUint8Array so a
    // failed encode surfaces instead of emitting a truncated update. V1 always
    // returns nil.
    Error() error
    ResetDsCurVal()
    WriteDsClock(clock Number)
    WriteDsLen(length Number) error
    GetRestEncoder() *bytes.Buffer
    // RestartRestEncoder returns the current rest-buffer bytes and installs a
    // fresh empty rest buffer, leaving any V2 column sub-encoders intact (used by
    // the lazy struct writer to capture one client fragment at a time).
    RestartRestEncoder() []uint8
}
```

## DSDecoder Interface

```go
type DSDecoder interface {
    ResetDsCurVal()
    ReadDsClock() (Number, error)
    ReadDsLen() (Number, error)
    GetRestDecoder() *bytes.Buffer
}
```

## Satisfaction

- `*UpdateEncoderV1` satisfies `UpdateEncoder` (no changes needed
  to method signatures — methods already match)
- `*UpdateEncoderV2` satisfies `UpdateEncoder` (new implementation)
- `*UpdateDecoderV1` satisfies `UpdateDecoder` (no changes needed)
- `*UpdateDecoderV2` satisfies `UpdateDecoder` (new implementation)

## Breaking Changes (per clarification decision)

Existing functions that accepted `*UpdateEncoderV1` /
`*UpdateDecoderV1` will change to accept `UpdateEncoder` /
`UpdateDecoder`. This is a breaking API change. Callers must
update but the fix is mechanical (types already implement the
interface).
