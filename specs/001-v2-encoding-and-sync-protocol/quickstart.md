# Quickstart: V2 Encoding/Decoding and Sync Protocol

## V2 Encoding

```go
import ycrdt "github.com/antst/go-yjs"

// Encode document state as V2 update
// NewDoc(guid, gc, gcFilter, meta, autoLoad)
doc := ycrdt.NewDoc("guid", true, ycrdt.DefaultGCFilter, nil, false)
text := doc.GetText("content")
text.Insert(0, "Hello, world!", nil)
update, err := ycrdt.EncodeStateAsUpdateV2(doc, nil)
if err != nil {
    // handle encode error
}

// Apply a V2 update from a JS client
ycrdt.ApplyUpdateV2(doc, jsUpdate, "remote")

// Compute a differential V2 update
// EncodeStateVector(doc, m, encoder) — pass nil for the default (full) vector.
sv := ycrdt.EncodeStateVector(doc, nil, ycrdt.NewUpdateEncoderV1())
diff, err := ycrdt.DiffUpdateV2(fullUpdate, sv)
if err != nil {
    // handle diff error
}

// Convert between formats
v2Update, err := ycrdt.ConvertUpdateFormatV1ToV2(v1Update)
if err != nil {
    // handle conversion error
}
v1Update, err := ycrdt.ConvertUpdateFormatV2ToV1(v2Update)
if err != nil {
    // handle conversion error
}
```

## Using Encoder/Decoder Interfaces

The `WriteSyncStep*` / `ReadSyncMessage` helpers accept the `UpdateEncoder` /
`UpdateDecoder` interfaces so the same code works for either format. **For sync
protocol messages on the wire, use the V1 encoder/decoder** — that is the
canonical y-protocols framing (`[msgType varUint][payload]`), and it is exactly
what `protocol.SyncHandler` emits, so it stays interoperable with
y-protocols / y-websocket clients.

```go
// Sync messages use the V1 encoder: ToUint8Array() returns the raw
// [msgType][payload] sync frame.
encoder := ycrdt.NewUpdateEncoderV1()
ycrdt.WriteSyncStep1(encoder, doc)
output := encoder.ToUint8Array() // canonical y-protocols sync message bytes

// Read it back with the matching V1 decoder.
decoder := ycrdt.NewUpdateDecoderV1(output)
reply := ycrdt.NewUpdateEncoderV1()
ycrdt.ReadSyncMessage(decoder, reply, doc, nil)
```

> Do NOT serialize a sync message with `UpdateEncoderV2.ToUint8Array()`: that
> wraps the payload in the V2 update column framing (feature flag + 9
> length-prefixed columns + rest buffer), which is an *update* container, not a
> sync frame, and would break interop. The V2 encoder/decoder are for **update
> payloads** (`EncodeStateAsUpdateV2` / `ApplyUpdateV2`), shown above.

## Protocol Subpackage

```go
import "github.com/antst/go-yjs/protocol"

// Server-side sync handler
handler := protocol.NewSyncHandler(doc)

// Handle incoming WebSocket message
responseType, err := handler.HandleMessage(msg, responseBuf)

// Generate initial sync message
syncStep1 := protocol.EncodeSyncStep1(doc)
conn.Write(syncStep1)

// Register custom message handler
handler.RegisterHandler(42, func(payload []byte) error {
    // handle custom message type 42
    return nil
})
```
