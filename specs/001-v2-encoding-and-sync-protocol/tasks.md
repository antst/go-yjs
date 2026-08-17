# Tasks: V2 Encoding/Decoding and Sync Protocol

**Input**: Design documents from `/specs/001-v2-encoding-and-sync-protocol/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Included — constitution mandates test-first development (Principle IV) and cross-language compatibility tests (Principle V).

**Organization**: Tasks grouped by user story. US3 (interfaces) is foundational — must complete before US1/US2 can integrate with sync layer.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Setup

**Purpose**: JS test harness for generating V2 reference payloads

- [X] T001 Create JS test harness project for generating V2 reference payloads in v2_test_fixtures/generate.js (pin yjs@13.6.27)
- [X] T002 Generate V2 reference payloads from JS: text ops, map ops, array ops, XML ops, mixed ops, delete-only updates, empty doc, single-client, multi-client (1000+) in v2_test_fixtures/

---

## Phase 2: Foundational (Helper Encoder/Decoder Types)

**Purpose**: Build the RLE, UintOptRle, IntDiffOptRle, and String encoder/decoder types that V2 depends on. These are pure building blocks with no external dependencies.

**CRITICAL**: No V2 encoder/decoder work can begin until this phase is complete.

### Tests

- [X] T003 [P] Write unit tests for RleEncoder/RleDecoder in rle_codec_test.go — verify round-trip for: single value, repeated values, alternating values, empty input
- [X] T004 [P] Write unit tests for UintOptRleEncoder/UintOptRleDecoder in uint_opt_rle_codec_test.go — verify round-trip for: single values (positive VarInt path), repeated values (negative VarInt path), mixed, large values
- [X] T005 [P] Write unit tests for IntDiffOptRleEncoder/IntDiffOptRleDecoder in int_diff_opt_rle_codec_test.go — verify round-trip for: sequential (diff=1), constant (diff=0), random diffs, negative values
- [X] T006 [P] Write unit tests for StringEncoder/StringDecoder in string_codec_test.go — verify round-trip for: single string, multiple strings, empty strings, unicode strings

### Implementation

- [X] T007 [P] Implement RleEncoder and RleDecoder in rle_codec.go
- [X] T008 [P] Implement UintOptRleEncoder and UintOptRleDecoder in uint_opt_rle_codec.go
- [X] T009 [P] Implement IntDiffOptRleEncoder and IntDiffOptRleDecoder in int_diff_opt_rle_codec.go
- [X] T010 [P] Implement StringEncoder and StringDecoder in string_codec.go

**Checkpoint**: All helper codec tests pass. These are self-contained types with no dependencies on V1/V2 encoder.

---

## Phase 3: User Story 3 — Encoder/Decoder Interface Abstraction (Priority: P1) 🎯 MVP-Enabling

**Goal**: Define UpdateEncoder/UpdateDecoder/DSEncoder/DSDecoder interfaces and refactor all existing code to use them instead of concrete V1 types. This is a breaking API change per clarification decision.

**Independent Test**: Pass all existing V1 compatibility tests with interface-based signatures (no regression).

### Implementation

- [X] T011 Define UpdateEncoder, UpdateDecoder, DSEncoder, and DSDecoder interfaces in interfaces.go per contracts/interfaces.go.md
- [X] T012 Define IAbstractContent.Write and ReadContent function type signatures using UpdateEncoder/UpdateDecoder interfaces in interfaces.go
- [X] T013 [US3] Refactor all content type Write methods to accept UpdateEncoder interface: content_string.go, content_binary.go, content_json.go, content_any.go, content_embed.go, content_format.go, content_deleted.go, content_doc.go, content_type.go
- [X] T014 [US3] Refactor all ReadContent functions to accept UpdateDecoder interface: content_string.go, content_binary.go, content_json.go, content_any.go, content_embed.go, content_format.go, content_deleted.go, content_doc.go, content_type.go
- [X] T015 [US3] Refactor struct Write methods to accept UpdateEncoder interface: item.go, gc.go, skip.go, abstract_struct.go
- [X] T016 [US3] Refactor type Write methods to accept UpdateEncoder interface: abstract_type.go, y_text.go, y_array.go, y_map.go, y_string.go, y_xml_fragment.go, y_xml_element.go, y_xml_text.go, y_xml_hook.go
- [X] T017 [US3] Refactor WriteDeleteSet and ReadDeleteSet to accept DSEncoder/DSDecoder interfaces in delete_set.go — unify WriteDeleteSet and WriteDeleteSetV2 into single function
- [X] T018 [US3] Refactor sync functions to accept UpdateEncoder/UpdateDecoder interfaces in sync.go: WriteSyncStep1, WriteSyncStep1FromUpdate, WriteSyncStep2, WriteSyncStep2FromUpdate, ReadSyncStep1, ReadSyncStep2, WriteUpdate, ReadSyncMessage
- [X] T019 [US3] Update callers in updates.go: MergeUpdatesV2, DiffUpdateV2, and related functions to use interfaces
- [X] T020 [US3] Update callers in merge.go: ApplyUpdateV2, EncodeStateAsUpdateV2, ReadStateVector, and related functions to use interfaces
- [X] T021 [US3] Update callers in transaction.go, snapshot.go, permanent_user_data.go to use interfaces
- [X] T022 [US3] Run all existing tests and golangci-lint to verify zero regressions

**Checkpoint**: All existing V1 compatibility tests pass. All function signatures use interfaces. No concrete V1 types in public API signatures (except constructors).

---

## Phase 4: User Story 1 — V2 Update Encoding (Priority: P1) 🎯 MVP

**Goal**: Full UpdateEncoderV2/UpdateDecoderV2 implementation that produces byte-identical output to Yjs v13.6.27.

**Independent Test**: Encode a Y.Doc with mixed content in Go as V2, compare byte-for-byte with JS-generated V2 payload from v2_test_fixtures/.

### Tests

- [X] T023 [US1] Write V2 compatibility tests in compatibility_v2_test.go — text operations: encode in Go, compare with JS V2 reference payload from v2_test_fixtures/
- [X] T024 [P] [US1] Write V2 compatibility tests in compatibility_v2_test.go — map operations, array operations, XML operations
- [X] T025 [P] [US1] Write V2 compatibility tests in compatibility_v2_test.go — mixed operations (text+map+array in same doc)
- [X] T026 [P] [US1] Write V2 compatibility tests in compatibility_v2_test.go — edge cases: empty doc, single client, 1000+ clients, structs-only (no deletes)
- [X] T027 [P] [US1] Write V2 size comparison test in compatibility_v2_test.go — verify V2 payload is smaller than V1 for 10K-op single-client document

### Implementation

- [X] T028 [US1] Remove existing V2 stub from update_encoder_decoder_v2.go
- [X] T029 [US1] Implement UpdateEncoderV2 with all 10 sub-encoders in update_encoder_v2.go — WriteID, WriteLeftID, WriteRightID, WriteClient, WriteInfo, WriteString, WriteParentInfo, WriteTypeRef, WriteLen, WriteAny, WriteBuf, WriteJson, WriteKey, ToUint8Array
- [X] T030 [US1] Implement UpdateDecoderV2 with all 10 sub-decoders in update_decoder_v2.go — ReadID, ReadLeftID, ReadRightID, ReadClient, ReadInfo, ReadString, ReadParentInfo, ReadTypeRef, ReadLen, ReadAny, ReadBuf, ReadJson, ReadKey (constructor splits input into per-field buffers)
- [X] T031 [US1] Implement key caching in UpdateEncoderV2.WriteKey and UpdateDecoderV2.ReadKey (match Yjs v13.6.27 behavior — caching disabled via commented keyMap.set)
- [X] T032 [US1] Implement WriteJson using WriteAny (not JSON.stringify) in UpdateEncoderV2 — V2-specific difference from V1
- [X] T033 [US1] Implement EncodeStateAsUpdateV2 and ApplyUpdateV2 convenience functions using V2 encoder/decoder in merge.go
- [X] T034 [US1] Implement EncodeStateVectorFromUpdateV2 in merge.go (state vector is always V1-encoded, extract from V2 update)
- [X] T035 [US1] Implement DiffUpdateV2 convenience function using V2 encoder/decoder in updates.go
- [X] T036 [US1] Add SEC-001 tests: decode malformed/truncated V2 payloads in compatibility_v2_test.go — must return error, not panic
- [X] T037 [US1] Add SEC-002 tests: decode V2 payloads with oversized length fields in compatibility_v2_test.go — must validate against remaining buffer
- [X] T038 [US1] Add performance benchmark for V2 encoding 10K-op document in compatibility_v2_test.go — verify < 50ms (PR-001)
- [X] T039 [US1] Run golangci-lint and all tests (V1 + V2)

**Checkpoint**: V2 encode/decode round-trips correctly. Byte-identical to JS Yjs v13.6.27 reference payloads. All V1 tests still pass.

---

## Phase 5: User Story 2 — V2 Delete Set Encoding (Priority: P1)

**Goal**: Complete DSDecoderV2 implementation with delta-coded clocks. DSEncoderV2 partially exists — verify and complete.

**Independent Test**: Decode a V2 delete set from JS reference payload, verify DeleteSet struct matches expected client-clock ranges.

### Tests

- [X] T040 [P] [US2] Write V2 delete set compatibility test in compatibility_v2_test.go — interleaved inserts and deletes from multiple clients, decode V2 delete set, verify client-clock ranges
- [X] T041 [P] [US2] Write V2 delete set round-trip test in compatibility_v2_test.go — encode DeleteSet in Go as V2, compare with JS reference payload
- [X] T042 [P] [US2] Write V2 delete-only update test in compatibility_v2_test.go — V2 update containing only delete sets (no structs)

### Implementation

- [X] T043 [US2] Implement DSDecoderV2 with ResetDsCurVal, ReadDsClock, ReadDsLen in update_decoder_v2.go
- [X] T044 [US2] Verify existing DSEncoderV2 (WriteDsClock, WriteDsLen, ResetDsCurVal) correctness against JS reference — fix if needed in update_encoder_v2.go
- [X] T045 [US2] Run all delete set tests (V1 + V2) and golangci-lint

**Checkpoint**: V2 delete sets encode/decode correctly. Byte-identical to JS reference. Mixed V1/V2 updates applied to same document work correctly.

---

## Phase 6: User Story 4 — Protocol Subpackage (Priority: P2)

**Goal**: Consumer-friendly `protocol/` subpackage with message framing, sync state machine, awareness helpers, and custom message type registry.

**Independent Test**: Full sync handshake (SyncStep1 → SyncStep2 → Update) between two Y.Docs via protocol helpers, verify both docs converge to identical state.

### Tests

- [X] T046 [US4] Write sync handshake integration test in protocol/protocol_test.go — two divergent Y.Docs, full handshake, verify identical state
- [X] T047 [P] [US4] Write awareness round-trip test in protocol/protocol_test.go — encode awareness update, decode, verify state matches
- [X] T048 [P] [US4] Write custom message handler test in protocol/protocol_test.go — register handler for type 42, dispatch message, verify handler called
- [X] T049 [P] [US4] Write message framing test in protocol/protocol_test.go — WriteMessage/ReadMessage round-trip for sync, awareness, and custom types

### Implementation

- [X] T050 [US4] Create protocol/ directory and protocol/protocol.go with message type constants and WriteMessage/ReadMessage functions per contracts/protocol-subpackage.go.md
- [X] T051 [US4] Implement SyncHandler with handler registry and HandleMessage dispatcher in protocol/sync.go
- [X] T052 [US4] Implement EncodeSyncStep1 and EncodeSyncStep2 convenience functions in protocol/sync.go
- [X] T053 [US4] Implement awareness helpers (EncodeAwarenessMessage, DecodeAwarenessMessage) wrapping root package functions in protocol/awareness.go
- [X] T054 [US4] Run all protocol tests and golangci-lint

**Checkpoint**: Protocol subpackage is fully functional. Sync handshake converges two docs. Custom message types dispatch correctly.

---

## Phase 7: User Story 5 — V1 ↔ V2 Conversion (Priority: P3)

**Goal**: Convert updates between V1 and V2 formats via full decode-reencode cycle.

**Independent Test**: Round-trip a V1 update through V2 and back, verify original document state preserved.

### Tests

- [X] T055 [P] [US5] Write V1→V2→V1 round-trip test in compatibility_v2_test.go — convert V1 update to V2 and back, verify document state matches
- [X] T056 [P] [US5] Write V2→V1 cross-language test in compatibility_v2_test.go — convert JS V2 update to V1 in Go, verify JS can apply V1 result

### Implementation

- [X] T057 [US5] Implement ConvertUpdateFormatV1ToV2 in updates.go — decode with V1 decoder, reencode with V2 encoder via LazyStructReader/LazyStructWriter
- [X] T058 [US5] Implement ConvertUpdateFormatV2ToV1 in updates.go — decode with V2 decoder, reencode with V1 encoder
- [X] T059 [US5] Run all conversion tests and golangci-lint

**Checkpoint**: Format conversion works both directions. Round-trip preserves document state.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Final validation across all stories

- [X] T060 Run full test suite (V1 compatibility + V2 compatibility + protocol + conversion) and verify zero failures
- [X] T061 Run golangci-lint with zero violations
- [X] T062 Verify state vector handling: confirm state vectors remain V1-encoded when used with V2 functions (per clarification)
- [X] T063 Verify edge case: mixed V1 and V2 updates applied to same document produce correct state
- [X] T064 Run quickstart.md code examples to verify they work as documented

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately
- **Foundational (Phase 2)**: No dependencies — can run in parallel with Phase 1
- **US3 Interfaces (Phase 3)**: Depends on Phase 2 (helper types needed for interface validation)
- **US1 V2 Encoding (Phase 4)**: Depends on Phase 2 (helper types) + Phase 3 (interfaces)
- **US2 V2 Delete Sets (Phase 5)**: Depends on Phase 3 (interfaces). Can run in parallel with Phase 4
- **US4 Protocol (Phase 6)**: Depends on Phase 3 (interfaces). Can run in parallel with Phase 4/5
- **US5 Conversion (Phase 7)**: Depends on Phase 4 (V2 encoder/decoder must exist)
- **Polish (Phase 8)**: Depends on all previous phases

### User Story Dependencies

- **US3 (Interfaces)**: FOUNDATIONAL — must complete before US1, US2, US4, US5
- **US1 (V2 Encoding)**: Depends on US3. No dependency on US2/US4/US5
- **US2 (V2 Delete Sets)**: Depends on US3. Can parallel with US1
- **US4 (Protocol)**: Depends on US3. Can parallel with US1/US2
- **US5 (Conversion)**: Depends on US1 (needs working V2 encoder/decoder)

### Parallel Opportunities

- Phase 1 and Phase 2 can run in parallel
- Within Phase 2: T003-T006 (tests) in parallel, then T007-T010 (impl) in parallel
- Within Phase 3: T013-T016 (signature changes) can partially parallel (different files)
- Phase 4, Phase 5, and Phase 6 can run in parallel after Phase 3
- Within Phase 4: T024-T027 (tests) in parallel
- Within Phase 5: T040-T042 (tests) in parallel

---

## Parallel Example: Phase 2 (Foundational)

```
# Launch all helper type tests together:
Task T003: "RleEncoder/RleDecoder tests in rle_codec_test.go"
Task T004: "UintOptRleEncoder/UintOptRleDecoder tests in uint_opt_rle_codec_test.go"
Task T005: "IntDiffOptRleEncoder/IntDiffOptRleDecoder tests in int_diff_opt_rle_codec_test.go"
Task T006: "StringEncoder/StringDecoder tests in string_codec_test.go"

# Then launch all helper type implementations together:
Task T007: "RleEncoder/RleDecoder in rle_codec.go"
Task T008: "UintOptRleEncoder/UintOptRleDecoder in uint_opt_rle_codec.go"
Task T009: "IntDiffOptRleEncoder/IntDiffOptRleDecoder in int_diff_opt_rle_codec.go"
Task T010: "StringEncoder/StringDecoder in string_codec.go"
```

## Parallel Example: After Phase 3 (three stories in parallel)

```
# US1, US2, and US4 can all proceed simultaneously:
Phase 4 (US1): V2 Update Encoder/Decoder implementation
Phase 5 (US2): V2 Delete Set Decoder implementation
Phase 6 (US4): Protocol subpackage implementation
```

---

## Implementation Strategy

### MVP First (US3 + US1 + US2)

1. Complete Phase 1: Setup (JS reference payloads)
2. Complete Phase 2: Foundational (helper types)
3. Complete Phase 3: US3 — Interface abstraction
4. Complete Phase 4: US1 — V2 Update Encoding
5. Complete Phase 5: US2 — V2 Delete Set Encoding
6. **STOP and VALIDATE**: V2 encode/decode is byte-identical to JS Yjs v13.6.27
7. The library can now read/write production V2 documents

### Incremental Delivery

1. Setup + Foundational + US3 → Interfaces ready, V1 still works
2. Add US1 + US2 → V2 encoding works → **Core MVP complete**
3. Add US4 → Protocol subpackage → Consumer-friendly API
4. Add US5 → Format conversion → Migration support
5. Each story adds capability without breaking previous stories

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story
- Constitution requires test-first: write tests, verify they fail, then implement
- All V2 output must be verified byte-for-byte against JS Yjs v13.6.27
- Key caching in V2 is disabled in Yjs v13.6.27 — match that behavior exactly
- V2 WriteJson uses WriteAny (not JSON.stringify) — critical difference from V1
- State vectors are always V1-encoded regardless of update format version
