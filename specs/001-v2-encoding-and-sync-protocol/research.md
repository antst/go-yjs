# Research: V2 Encoding/Decoding and Sync Protocol

**Date**: 2026-03-31
**Reference**: Yjs v13.6.27 source (`yjs/yjs` GitHub) + `dmonad/lib0`

## V2 Encoding Architecture

### Decision: V2 uses 10 specialized sub-encoders + 1 rest encoder

**Rationale**: Each struct field category gets its own encoder
optimized for that field's value distribution. Fields like client
IDs (many repeats) and clocks (sequential) compress dramatically
with RLE and delta-coding. The final output concatenates all
sub-buffers with length prefixes.

**Alternatives considered**: Single-buffer encoding (V1 approach) —
rejected because it cannot exploit per-field patterns.

### Sub-Encoder Assignments

| Field | Encoder Type | Write Method |
|-------|-------------|--------------|
| `restEncoder` | Plain buffer | `WriteAny`, `WriteBuf`, `WriteJson` |
| `keyClockEncoder` | IntDiffOptRleEncoder | `WriteKey` (key cache index) |
| `clientEncoder` | UintOptRleEncoder | `WriteClient`, `WriteLeftID`, `WriteRightID` |
| `leftClockEncoder` | IntDiffOptRleEncoder | `WriteLeftID` |
| `rightClockEncoder` | IntDiffOptRleEncoder | `WriteRightID` |
| `infoEncoder` | RleEncoder(uint8) | `WriteInfo` |
| `stringEncoder` | StringEncoder | `WriteString`, `WriteKey` (new keys) |
| `parentInfoEncoder` | RleEncoder(uint8) | `WriteParentInfo` |
| `typeRefEncoder` | UintOptRleEncoder | `WriteTypeRef` |
| `lenEncoder` | UintOptRleEncoder | `WriteLen` |

### ToUint8Array() Output Format

```
[0] writeVarUint(0)                            — feature flag (always 0)
[1] writeVarUint8Array(keyClock bytes)          — length-prefixed
[2] writeVarUint8Array(client bytes)            — length-prefixed
[3] writeVarUint8Array(leftClock bytes)         — length-prefixed
[4] writeVarUint8Array(rightClock bytes)        — length-prefixed
[5] writeVarUint8Array(info bytes)              — length-prefixed
[6] writeVarUint8Array(string bytes)            — length-prefixed
[7] writeVarUint8Array(parentInfo bytes)        — length-prefixed
[8] writeVarUint8Array(typeRef bytes)           — length-prefixed
[9] writeVarUint8Array(len bytes)               — length-prefixed
[rest] writeUint8Array(rest bytes)              — NOT length-prefixed
```

**Critical**: Positions 1-9 use `writeVarUint8Array` (length prefix).
Position `rest` uses `writeUint8Array` (raw append, no prefix).
The decoder reads "everything remaining" as rest.

## Helper Encoder/Decoder Types (from lib0)

### RleEncoder / RleDecoder

Basic run-length encoding for consecutive identical values.

**Encode**: Write value, count consecutive repeats. When value
changes, write `count - 1` as VarUint, then write new value.

**Decode**: Read value, read `VarUint + 1` as count. Return that
value for `count` reads. If no more data after value read, set
count to max (return forever).

**Used for**: `infoEncoder`, `parentInfoEncoder` (both uint8).

### UintOptRleEncoder / UintOptRleDecoder

Optimized RLE for unsigned integers — avoids overhead when values
don't repeat.

**Encode**: On flush:
- count == 1: write `s` as positive VarInt (single value)
- count > 1: write `-s` as negative VarInt, then `count - 2`
  as VarUint

**Decode**: Read VarInt. If negative: value = `-s`, read
`VarUint + 2` as count. If positive: value = `s`, count = 1.

**Key insight**: Sign bit of VarInt acts as "has repeat count" flag.
Non-repeating values cost zero overhead.

**Used for**: `clientEncoder`, `typeRefEncoder`, `lenEncoder`.

### IntDiffOptRleEncoder / IntDiffOptRleDecoder

Combines delta-encoding with optimized RLE. Values are diff'd from
previous, then consecutive identical diffs are RLE'd.

**Encode**: On flush:
- `encodedDiff = diff * 2 + (count == 1 ? 0 : 1)` — LSB = hasCount
- Write `encodedDiff` as VarInt
- If count > 1: write `count - 2` as VarUint

**Decode**: Read VarInt as `diff`.
- `hasCount = diff & 1`
- `actualDiff = diff >> 1` (arithmetic shift)
- If hasCount: read `VarUint + 2` as count; else count = 1
- Accumulate: `s += actualDiff`, return `s`

**Note**: Only supports 31-bit integers due to LSB reservation.

**Used for**: `keyClockEncoder`, `leftClockEncoder`,
`rightClockEncoder`.

### StringEncoder / StringDecoder

All strings concatenated into one buffer; lengths tracked via
UintOptRleEncoder.

**Encode** (`toUint8Array`):
1. Join all strings into one
2. Write concatenated string as VarString
3. Append UintOptRle length data

**Decode**: Read VarString (full concat), then slice per-string
using UintOptRle-decoded lengths.

## DS Encoder/Decoder V2 (Delete Sets)

**DSEncoderV2**: Delta-encoded clocks within each client's ranges.
- `ResetDsCurVal()`: reset running value to 0
- `WriteDsClock(clock)`: write `clock - dsCurrVal` as VarUint,
  set `dsCurrVal = clock`
- `WriteDsLen(len)`: write `len - 1` as VarUint, add `len` to
  `dsCurrVal`

**DSDecoderV2**: Reverse of above.
- `ResetDsCurVal()`: reset to 0
- `ReadDsClock()`: read VarUint, add to `dsCurrVal`, return it
- `ReadDsLen()`: read `VarUint + 1`, add to `dsCurrVal`, return it

**Contrast with V1**: V1 writes raw values, no delta encoding.
`ResetDsCurVal` is a no-op in V1.

## V2 Key Caching

Keys use a cache: first occurrence writes the string, subsequent
occurrences write the cache index. **However**, in Yjs v13.6.27
the `keyMap.set()` call is commented out with a TODO, so key
caching is effectively disabled. Every `writeKey` increments
`keyClock` and writes the string. The decoder handles both paths,
so we must implement the same behavior (no caching) to match
byte output.

## V2 JSON Encoding Difference

In V1, `WriteJson` uses `JSON.stringify` + `writeVarString`.
In V2, `WriteJson` uses `writeAny` (lib0 any-encoding with type
prefix byte). This must be matched exactly.

## Format Conversion (V1 ↔ V2)

Both `convertUpdateFormatV1ToV2` and `convertUpdateFormatV2ToV1`
use the same generic `convertUpdateFormat(update, blockTransformer,
Decoder, Encoder)`:

1. Decode all structs from source format via LazyStructReader
2. Write all structs to target format via LazyStructWriter
3. Read delete set from source decoder, write to target encoder
4. Return `encoder.toUint8Array()`

The block transformer is `identity` — full decode-reencode cycle,
no shortcut.

## Current Go Codebase State

| Component | Status |
|-----------|--------|
| DSEncoderV2 | Partially implemented (delta clock/len works) |
| UpdateEncoderV2 | Stub — `ToUint8Array` writes empty sub-buffers |
| UpdateDecoderV2 | Does not exist |
| DSDecoderV2 | Does not exist |
| Helper types (RLE, etc.) | Do not exist |
| Interfaces | Do not exist |
| Content Write/Read | All hardcoded to `*UpdateEncoderV1` / `*UpdateDecoderV1` |
| Sync functions | All hardcoded to V1 concrete types |
| Protocol subpackage | Does not exist |

## Impact on Existing Code

Introducing `UpdateEncoder`/`UpdateDecoder` interfaces requires
changing signatures in ~34 locations:
- 22 `Write(encoder *UpdateEncoderV1, ...)` methods
- 12 `ReadContent*(decoder *UpdateDecoderV1)` functions
- 8 sync functions in `sync.go`
- `WriteDeleteSet` / `ReadDeleteSet` in `delete_set.go`
- Various callers in `updates.go`, `merge.go`, `transaction.go`,
  `snapshot.go`, `permanent_user_data.go`

All changes are mechanical: replace concrete pointer with interface
type. No logic changes needed — both V1 and V2 types will
implement the same method set.
