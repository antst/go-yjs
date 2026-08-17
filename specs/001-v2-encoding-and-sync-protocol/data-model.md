# Data Model: V2 Encoding/Decoding and Sync Protocol

## New Types

### Helper Encoder Types

#### RleEncoder

| Field | Type | Description |
|-------|------|-------------|
| w | func(*bytes.Buffer, uint8) | Writer function |
| state | uint8 | Current value |
| count | uint | Consecutive repeat count |
| buf | *bytes.Buffer | Output buffer |

**State transitions**: Idle → Writing (first write) → Flushing
(value changes or ToUint8Array called)

#### RleDecoder

| Field | Type | Description |
|-------|------|-------------|
| r | func(*bytes.Buffer) (uint8, error) | Reader function |
| state | uint8 | Current value |
| count | int | Remaining repeats (-1 = infinite) |
| buf | *bytes.Buffer | Input buffer |

#### UintOptRleEncoder

| Field | Type | Description |
|-------|------|-------------|
| s | uint64 | Current value |
| count | uint | Repeat count |
| buf | *bytes.Buffer | Output buffer |

#### UintOptRleDecoder

| Field | Type | Description |
|-------|------|-------------|
| s | uint64 | Current value |
| count | uint | Remaining repeats |
| buf | *bytes.Buffer | Input buffer |

#### IntDiffOptRleEncoder

| Field | Type | Description |
|-------|------|-------------|
| s | int64 | Previous value |
| diff | int64 | Current diff |
| count | uint | Repeat count for current diff |
| buf | *bytes.Buffer | Output buffer |

#### IntDiffOptRleDecoder

| Field | Type | Description |
|-------|------|-------------|
| s | int64 | Running accumulated value |
| diff | int64 | Current diff |
| count | uint | Remaining repeats |
| buf | *bytes.Buffer | Input buffer |

#### StringEncoder

| Field | Type | Description |
|-------|------|-------------|
| parts | []string | Accumulated string fragments |
| lensEncoder | *UintOptRleEncoder | Length tracker |

#### StringDecoder

| Field | Type | Description |
|-------|------|-------------|
| str | string | Full concatenated string |
| spos | int | Current slice position |
| lensDecoder | *UintOptRleDecoder | Length reader |

### V2 Encoder/Decoder Types

#### UpdateEncoderV2

| Field | Type | Description |
|-------|------|-------------|
| DSEncoderV2 | embedded | DS encoding (delta clock/len) |
| keyClockEncoder | *IntDiffOptRleEncoder | Key cache indices |
| clientEncoder | *UintOptRleEncoder | Client IDs |
| leftClockEncoder | *IntDiffOptRleEncoder | Left-origin clocks |
| rightClockEncoder | *IntDiffOptRleEncoder | Right-origin clocks |
| infoEncoder | *RleEncoder | Info bytes |
| stringEncoder | *StringEncoder | Strings |
| parentInfoEncoder | *RleEncoder | Parent info flags |
| typeRefEncoder | *UintOptRleEncoder | Type references |
| lenEncoder | *UintOptRleEncoder | Struct lengths |
| keyMap | map[string]int | Key string → cache index |
| keyClock | int | Next key cache index |

#### UpdateDecoderV2

| Field | Type | Description |
|-------|------|-------------|
| DSDecoderV2 | embedded | DS decoding (delta) |
| keyClockDecoder | *IntDiffOptRleDecoder | Key cache indices |
| clientDecoder | *UintOptRleDecoder | Client IDs |
| leftClockDecoder | *IntDiffOptRleDecoder | Left-origin clocks |
| rightClockDecoder | *IntDiffOptRleDecoder | Right-origin clocks |
| infoDecoder | *RleDecoder | Info bytes |
| stringDecoder | *StringDecoder | Strings |
| parentInfoDecoder | *RleDecoder | Parent info flags |
| typeRefDecoder | *UintOptRleDecoder | Type references |
| lenDecoder | *UintOptRleDecoder | Struct lengths |
| keys | []string | Key cache (populated on read) |

#### DSDecoderV2

| Field | Type | Description |
|-------|------|-------------|
| RestDecoder | *bytes.Buffer | Input buffer |
| DsCurrVal | Number | Running delta accumulator |

### Interfaces

#### UpdateEncoder

Methods: `WriteID`, `WriteLeftID`, `WriteRightID`, `WriteClient`,
`WriteInfo`, `WriteString`, `WriteParentInfo`, `WriteTypeRef`,
`WriteLen`, `WriteAny`, `WriteBuf`, `WriteJson`, `WriteKey`,
`ToUint8Array`

Satisfied by: `*UpdateEncoderV1`, `*UpdateEncoderV2`

#### UpdateDecoder

Methods: `ReadID`, `ReadLeftID`, `ReadRightID`, `ReadClient`,
`ReadInfo`, `ReadString`, `ReadParentInfo`, `ReadTypeRef`,
`ReadLen`, `ReadAny`, `ReadBuf`, `ReadJson`, `ReadKey`

Satisfied by: `*UpdateDecoderV1`, `*UpdateDecoderV2`

#### DSEncoder

Methods: `ToUint8Array`, `ResetDsCurVal`, `WriteDsClock`,
`WriteDsLen`

Satisfied by: `*DSEncoderV1` (via `*UpdateEncoderV1`),
`*DSEncoderV2` (via `*UpdateEncoderV2`)

#### DSDecoder

Methods: `ResetDsCurVal`, `ReadDsClock`, `ReadDsLen`

Satisfied by: `*UpdateDecoderV1`, `*UpdateDecoderV2`

### Protocol Subpackage Types

#### protocol.SyncHandler

| Field | Type | Description |
|-------|------|-------------|
| doc | *y_crdt.Doc | Document being synced |
| handlers | map[uint8]MessageHandler | Custom message type registry |

**MessageHandler**: `func(decoder *bytes.Buffer) error`

#### Message Type Constants

| Constant | Value | Description |
|----------|-------|-------------|
| MessageSync | 0 | Sync protocol messages |
| MessageAwareness | 1 | Awareness protocol messages |
| (custom) | 2+ | User-registered handlers |

## Relationships

```
UpdateEncoder (interface)
  ├── *UpdateEncoderV1 (embeds DSEncoderV1)
  └── *UpdateEncoderV2 (embeds DSEncoderV2)
         ├── uses *IntDiffOptRleEncoder (×3)
         ├── uses *UintOptRleEncoder (×3)
         ├── uses *RleEncoder (×2)
         └── uses *StringEncoder (×1)

UpdateDecoder (interface)
  ├── *UpdateDecoderV1
  └── *UpdateDecoderV2
         ├── uses *IntDiffOptRleDecoder (×3)
         ├── uses *UintOptRleDecoder (×3)
         ├── uses *RleDecoder (×2)
         └── uses *StringDecoder (×1)

protocol.SyncHandler
  ├── owns *y_crdt.Doc
  ├── uses UpdateEncoder/UpdateDecoder (via y_crdt sync functions)
  └── dispatches to map[uint8]MessageHandler
```
