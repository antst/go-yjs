# Implementation Plan: V2 Encoding/Decoding and Sync Protocol

**Branch**: `001-v2-encoding-and-sync-protocol` | **Date**: 2026-03-31 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-v2-encoding-and-sync-protocol/spec.md`

## Summary

Implement full V2 encoding/decoding for Yjs update payloads and
delete sets in the y-crdt Go library, generalize the sync protocol
to accept both V1 and V2 via interfaces, and create a `protocol`
subpackage with consumer-friendly sync/awareness helpers. The V2
format uses run-length and delta-coding per field category,
producing smaller payloads than V1 for typical documents.

## Technical Context

**Language/Version**: Go 1.24+
**Primary Dependencies**: None at runtime (stdlib only); tests use the Go stdlib `testing` package
**Storage**: N/A (library)
**Testing**: Go stdlib `testing`. A deterministic ClientID for byte-parity fixtures is set via the `WithClientID` `NewDoc` option (no monkey-patching / mocking library)
**Target Platform**: Any Go-supported platform
**Project Type**: Library
**Performance Goals**: V2 encode 10K-op document < 50ms; V2 payloads smaller than V1 for repeated client IDs
**Constraints**: Byte-identical output to Yjs v13.6.27; zero external runtime dependencies
**Scale/Scope**: Single Go module, ~58 source files, all in package `y_crdt`

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
|-----------|--------|-------|
| I. Yjs Wire Compatibility | PASS | Spec mandates byte-identical V2 output vs JS Yjs v13.6.27 |
| II. Single-Package Library | PASS | Core V2 stays in root package; `protocol` is an optional subpackage |
| III. Zero External Runtime Dependencies | PASS | No new runtime deps needed |
| IV. Test-First Development | PASS | Compatibility tests written before implementation per spec |
| V. Cross-Language Compatibility Tests | PASS | JS reference payloads required for all V2 tests |
| VI. Root Cause Analysis | PASS | N/A — new feature, not a bug fix |
| VII. DRY — Single Source of Truth | PASS | V1/V2 share base encoding primitives via interfaces |
| VIII. Lint on Completion | PASS | Will run golangci-lint before commit |
| IX. No Legacy Code | PASS | V2 stub will be replaced with real implementation |
| X. No Busywork | PASS | Every artifact addresses a spec requirement |
| XI. Meaningful Tests Only | PASS | Tests verify byte-level correctness vs Yjs reference |
| XII. Latest Dependencies Always | PASS | No new deps to version-check |
| XIII. No Assumptions | PASS | V2 format will be verified against Yjs JS source |

## Project Structure

### Documentation (this feature)

```text
specs/001-v2-encoding-and-sync-protocol/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
└── tasks.md             # Phase 2 output (via /speckit.tasks)
```

### Source Code (repository root)

```text
# Root package (y_crdt) — existing + new files
update_encoder_v1.go          # Existing — will satisfy UpdateEncoder interface
update_decoder_v1.go          # Existing — will satisfy UpdateDecoder interface
update_encoder_decoder_v2.go  # Existing stub — replace with full V2 implementation
                              # Split into: update_encoder_v2.go, update_decoder_v2.go
interfaces.go                 # NEW — UpdateEncoder, UpdateDecoder, DSEncoder, DSDecoder interfaces
sync.go                       # Existing — refactor to accept interfaces
encoding.go                   # Existing — shared base primitives (no changes)
decoding.go                   # Existing — shared base primitives (no changes)
delete_set.go                 # Existing — unify WriteDeleteSet/WriteDeleteSetV2 via interface
updates.go                    # Existing — parameterize MergeUpdatesV2/DiffUpdateV2 for V2
merge.go                      # Existing — parameterize EncodeStateAsUpdateV2/ApplyUpdateV2

# Content type files (all need Write/Read signature changes)
content_string.go             # Write(*UpdateEncoderV1, ...) → Write(UpdateEncoder, ...)
content_binary.go             # Same pattern
content_json.go               # Same pattern
content_any.go                # Same pattern
content_embed.go              # Same pattern
content_format.go             # Same pattern
content_deleted.go            # Same pattern
content_doc.go                # Same pattern
content_type.go               # Same pattern
item.go                       # Same pattern
gc.go                         # Same pattern
skip.go                       # Same pattern
abstract_struct.go            # Same pattern
abstract_type.go              # Same pattern (type Write methods)
y_text.go, y_array.go, etc.   # Same pattern (type Write methods)

# Protocol subpackage — NEW
protocol/
├── protocol.go               # Message framing, type constants
├── sync.go                    # SyncHandler state machine
├── awareness.go               # Awareness helpers wrapping root package
└── protocol_test.go           # Sync handshake integration tests

# Test files
compatibility_v2_test.go      # NEW — V2 cross-language compatibility tests
v2_test_fixtures/             # NEW — JS-generated V2 reference payloads
```

**Structure Decision**: Root package extended with interfaces and V2
implementation. Protocol helpers in a new `protocol/` subpackage per
constitution principle II (optional import, no transport coupling).

## Complexity Tracking

No constitution violations — table not needed.
