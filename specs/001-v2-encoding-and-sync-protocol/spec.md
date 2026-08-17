# Feature Specification: V2 Encoding/Decoding and Sync Protocol

**Feature Branch**: `001-v2-encoding-and-sync-protocol`
**Created**: 2026-03-31
**Status**: Draft
**Input**: Analysis of collaborative-document-service Yjs usage and y-crdt gap assessment

## Context

The Alkemio collaborative-document-service uses Yjs V2 encoding
exclusively (`encodeStateAsUpdateV2` / `applyUpdateV2`) for document
persistence and sync. The current y-crdt Go implementation has full
V1 support but only a non-functional V2 stub. Additionally, the sync
protocol functions are tightly coupled to V1 encoder types and need
to support V2 for interoperability with Hocuspocus/y-protocols
clients.

This spec covers three areas:
1. Full V2 encoding and decoding implementation
2. Sync protocol generalization to support both V1 and V2
3. A `protocol` subpackage providing consumer-friendly sync and
   awareness helpers with message framing

## Clarifications

### Session 2026-03-31

- Q: Which Yjs npm version should be the authoritative reference for V2 compatibility testing? → A: Yjs v13.6.27 (version used by `collaborative-document-service`)
- Q: Should existing V1-only public function signatures be preserved or replaced with interface-based ones? → A: Breaking change — replace concrete V1 signatures with interface-based ones
- Q: Should the protocol subpackage support custom message type handlers beyond sync and awareness? → A: Yes — support custom message types via a handler registry (match `y-protocols` pattern)
- Q: Does V2 apply to state vectors? → A: No — state vectors are always V1-encoded per Yjs spec. Only update payloads and delete sets have V2 variants.

## User Scenarios & Testing

### User Story 1 — V2 Update Encoding (Priority: P1)

A Go server encodes a Y.Doc state using V2 encoding and sends it to
a JavaScript Yjs client. The client successfully applies the update
via `Y.applyUpdateV2()`. Conversely, a JS client encodes with
`Y.encodeStateAsUpdateV2()` and the Go server decodes and applies it
correctly.

**Why this priority**: The collaborative-document-service stores all
documents in V2 format. Without V2 support, the Go library cannot
read or write production document data.

**Independent Test**: Generate a Y.Doc with mixed content (text,
map, array) in JavaScript, encode as V2, decode in Go, re-encode in
Go, and assert byte equality with the original JS output.

**Acceptance Scenarios**:

1. **Given** a Y.Doc with text insertions and deletions created in
   JS, **When** the V2-encoded update is decoded by Go and
   re-encoded, **Then** the output is byte-identical to the JS
   V2 output.
2. **Given** a Y.Doc with map operations created in Go, **When**
   encoded as V2 update, **Then** a JS client can
   `applyUpdateV2()` without error and the document state matches.
3. **Given** a V2-encoded state vector from JS, **When** decoded by
   Go, **Then** `EncodeStateAsUpdateV2(doc, stateVector)` produces
   the correct differential update.
4. **Given** a large document (10,000+ operations), **When** encoded
   as V2, **Then** the output is smaller than the V1 encoding of
   the same document (V2 compression benefit).

---

### User Story 2 — V2 Delete Set Encoding (Priority: P1)

Delete sets within V2 updates use delta-coded clocks. The V2 delete
set encoder/decoder must produce wire-compatible output.

**Why this priority**: Delete operations are fundamental to CRDT
correctness. V2 delete set encoding is already partially stubbed
(`DSEncoderV2.WriteDsClock` / `WriteDsLen`) but the decoder side
is missing.

**Independent Test**: Create a document with deletions in JS, encode
the delete set portion as V2, decode in Go, and verify the delete
set matches.

**Acceptance Scenarios**:

1. **Given** a document with interleaved inserts and deletes from
   multiple clients, **When** the V2 delete set is decoded in Go,
   **Then** the resulting DeleteSet struct matches the expected
   client-clock ranges.
2. **Given** a DeleteSet encoded by Go using V2, **When** applied
   by JS via `applyUpdateV2()`, **Then** the correct items are
   marked as deleted.

---

### User Story 3 — Encoder/Decoder Interface Abstraction (Priority: P1)

Currently, sync protocol functions (`WriteSyncStep1`, `ReadSyncStep1`,
etc.) are hardcoded to `*UpdateEncoderV1` / `*UpdateDecoderV1`. They
must accept either V1 or V2 via an interface so consumers can choose
the encoding version.

**Why this priority**: Without this, V2 support cannot be used with
the sync protocol, which is the primary consumer of encoding in
real-time collaboration.

**Independent Test**: Call `WriteSyncStep1` with a V2 encoder and
verify the output is decodable by a V2 decoder.

**Acceptance Scenarios**:

1. **Given** the existing V1 encoder/decoder, **When** passed to
   sync functions via the interface, **Then** behavior is identical
   to the current hardcoded V1 implementation (no regression).
2. **Given** a V2 encoder, **When** used with `WriteSyncStep2`,
   **Then** the output contains V2-encoded update data that JS
   can decode.

---

### User Story 4 — Protocol Subpackage (Priority: P2)

A `protocol` subpackage provides consumer-friendly helpers for
building a collaboration server: message framing, sync state
machine, and awareness message handling. This is the Go equivalent
of the `y-protocols` npm package.

**Why this priority**: Consumers should not need to understand the
raw encoder/decoder API to implement a sync server. However, the
core V2 encoding (US1-US3) must work first.

**Independent Test**: Use the `protocol` subpackage to simulate a
full client-server sync handshake (SyncStep1 → SyncStep2 → Update)
between two Y.Docs, verifying both docs converge to the same state.

**Acceptance Scenarios**:

1. **Given** two Y.Docs with divergent state, **When** a full sync
   handshake is performed using the protocol helpers, **Then** both
   docs have identical content.
2. **Given** a client sending awareness updates, **When** the
   server receives and re-broadcasts them, **Then** other clients
   see the correct awareness state.
3. **Given** a message byte stream, **When** passed to the protocol
   message reader, **Then** each message is correctly dispatched
   by type (sync, awareness, custom).

---

### User Story 5 — V1 ↔ V2 Conversion (Priority: P3)

The library can convert updates between V1 and V2 formats, matching
the `convertUpdateFormatV1ToV2` and `convertUpdateFormatV2ToV1`
functions from Yjs.

**Why this priority**: Useful for migration scenarios and mixed
environments, but not required for the primary collaboration flow
where a single format is negotiated.

**Independent Test**: Round-trip a V1 update through V2 conversion
and back, asserting the original document state is preserved.

**Acceptance Scenarios**:

1. **Given** a V1 update, **When** converted to V2 and back to V1,
   **Then** the resulting V1 update produces the same document state.
2. **Given** a V2 update from JS, **When** converted to V1 by Go,
   **Then** JS `Y.applyUpdate()` (V1) succeeds with correct state.

---

### Edge Cases

- Empty document (zero operations) encoded as V2
- Document with a single client (no delta-coding benefit in V2)
- Document with 1000+ distinct clients (stress delta-coding)
- V2 update containing only delete sets (no structs)
- V2 update containing only structs (no deletes)
- Decoding a truncated/corrupted V2 payload (must error cleanly)
- Mixed V1 and V2 updates applied to the same document
- State vector from V1 used to compute V2 differential update

## Requirements

### Functional Requirements

- **FR-001**: Library MUST implement `UpdateEncoderV2` with all
  methods matching the V1 interface: `WriteID`, `WriteLeftID`,
  `WriteRightID`, `WriteClient`, `WriteInfo`, `WriteString`,
  `WriteParentInfo`, `WriteTypeRef`, `WriteLen`, `WriteAny`,
  `WriteBuf`, `WriteJson`, `WriteKey`, `ToUint8Array`
- **FR-002**: Library MUST implement `UpdateDecoderV2` with all
  methods matching the V1 interface: `ReadID`, `ReadLeftID`,
  `ReadRightID`, `ReadClient`, `ReadInfo`, `ReadString`,
  `ReadParentInfo`, `ReadTypeRef`, `ReadLen`, `ReadAny`,
  `ReadBuf`, `ReadJson`, `ReadKey`
- **FR-003**: Library MUST implement `DSDecoderV2` with
  `ResetDsCurVal`, `ReadDsClock`, `ReadDsLen`
- **FR-004**: Library MUST provide `EncodeStateAsUpdateV2(doc, sv)`
  and `ApplyUpdateV2(doc, update, origin)` top-level functions
- **FR-005**: Library MUST define `UpdateEncoder` and
  `UpdateDecoder` interfaces that both V1 and V2 types satisfy
- **FR-006**: Sync protocol functions MUST accept the
  `UpdateEncoder` / `UpdateDecoder` interfaces instead of
  concrete V1 types (breaking API change — existing concrete V1
  signatures are replaced, not preserved as wrappers)
- **FR-007**: Library MUST provide `EncodeStateVectorFromUpdateV2`
  function (note: state vectors themselves are always V1-encoded
  per Yjs spec — only update payloads and delete sets have V2
  variants)
- **FR-008**: Library MUST provide `DiffUpdateV2` function for
  computing differential V2 updates from a state vector
- **FR-009**: A `protocol` subpackage MUST provide message framing:
  `WriteMessage(buf, msgType, payload)` and
  `ReadMessage(buf) (msgType, payload, error)`
- **FR-010**: The `protocol` subpackage MUST provide a `SyncHandler`
  that manages the sync state machine
  (SyncStep1 → SyncStep2 → streaming Updates)
- **FR-011**: The `protocol` subpackage MUST provide awareness
  message encoding/decoding helpers that wrap the existing
  `EncodeAwarenessUpdate` / `ApplyAwarenessUpdate` functions
- **FR-012**: Library MUST provide `ConvertUpdateFormatV1ToV2` and
  `ConvertUpdateFormatV2ToV1` conversion functions
- **FR-013**: All V2 encoding output MUST be byte-identical to
  JavaScript Yjs `encodeStateAsUpdateV2` for the same operations
- **FR-014**: The `protocol` subpackage MUST support registering
  custom message type handlers via a handler registry, matching
  the `y-protocols` dispatch-by-type-byte pattern

### Performance Requirements

- **PR-001**: V2 encoding of a 10,000-operation document MUST
  complete in under 50ms
- **PR-002**: V2-encoded payloads MUST be smaller than V1 for
  documents with repeated client IDs (the common case)

### Security Requirements

- **SEC-001**: Decoding malformed V2 payloads MUST NOT panic —
  must return an error
- **SEC-002**: Decoding must not allocate unbounded memory from
  untrusted length fields — validate lengths against remaining
  buffer

### Key Entities

- **UpdateEncoderV2**: V2 encoder using run-length and delta
  coding for client IDs, clocks, and struct fields. Stores each
  field category in a separate internal buffer, then concatenates
  them in `ToUint8Array()`.
- **UpdateDecoderV2**: V2 decoder that splits the input into
  per-field buffers and reads from each using the corresponding
  decoding strategy.
- **DSEncoderV2** / **DSDecoderV2**: Delta-coded delete set
  encoder/decoder (DSEncoderV2 partially exists).
- **UpdateEncoder / UpdateDecoder interfaces**: Shared interface
  satisfied by both V1 and V2 encoder/decoder types.
- **protocol.SyncHandler**: State machine managing the sync
  handshake between two Y.Doc instances.

## Success Criteria

### Measurable Outcomes

- **SC-001**: All existing V1 compatibility tests continue to pass
  (no regression)
- **SC-002**: New V2 compatibility tests pass against JS Yjs
  reference payloads for: text operations, map operations, array
  operations, XML operations, mixed operations, delete-only
  updates
- **SC-003**: Sync protocol functions work with both V1 and V2
  encoders without code duplication
- **SC-004**: The `protocol` subpackage can execute a full sync
  handshake between two Go Y.Doc instances, converging to
  identical state
- **SC-005**: V2 payloads are measurably smaller than V1 for
  multi-operation single-client documents

## Assumptions

- The JavaScript Yjs npm package v13.6.27 (as used by the Alkemio
  `collaborative-document-service`) is the authoritative reference
  for V2 encoding behavior (pin to that exact version for tests)
- State vectors are always V1-encoded regardless of update format
  version — only update payloads and delete sets have V2 variants
- V2 encoding format follows the Yjs source code in
  `src/utils/UpdateEncoder.js` and `src/utils/UpdateDecoder.js`
- The existing V1 implementation is correct (verified by passing
  compatibility tests)
- Consumers of the `protocol` subpackage provide their own
  WebSocket or transport layer
- The `protocol` subpackage does not manage rooms, authentication,
  or persistence — those are application concerns
