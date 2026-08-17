# y-crdt — Yjs CRDT Implementation in Go

Go library implementing the Yjs CRDT algorithms for real-time
collaborative editing. Provides document state management, binary
encoding/decoding, sync protocol, and awareness protocol — all
wire-compatible with the JavaScript Yjs reference implementation.

## Tech Stack

- **Language**: Go 1.24+
- **Module**: `github.com/antst/go-yjs`
- **Type**: Library (not a service)
- **Testing**: Go stdlib `testing` (deterministic ClientID via the `WithClientID` `NewDoc` option — no mocking library)
- **Reference**: JavaScript Yjs (`yjs` npm package)

## What's Implemented

- Y.Doc, Y.Text, Y.Array, Y.Map, Y.XmlFragment, Y.XmlElement, Y.XmlText
- All content types (string, binary, JSON, embed, format, deleted, any, doc, type)
- V1 encoding/decoding (complete, compatibility-tested against JS Yjs)
- State vectors, delete sets, update encoding/decoding
- Sync protocol (SyncStep1, SyncStep2, Update messages)
- Awareness protocol (encode/apply/remove)
- Transactions, undo manager, snapshots, relative positions
- Garbage collection

## Implemented (PR #1 — 001-v2-encoding-and-sync-protocol)

- **V2 encoding/decoding**: full `UpdateEncoderV2` / `UpdateDecoderV2` (column
  codecs + delta-coded delete sets), byte-exact vs Yjs 13.6.31.
- **Sync protocol subpackage**: `protocol/` provides message framing, a sync
  state machine (`SyncHandler`), and awareness helpers for consumers.
- **V2 compatibility tests**: cross-language fixtures (`v2_test_fixtures/`) plus
  a cross-impl fuzz gate (`fuzz/`).

## Anti-Patterns — Strictly Prohibited

1. Do not guess Yjs encoding behavior — verify against JS source
2. Do not add transport/server code to the core package
3. Do not add runtime dependencies without justification
4. Do not leave stub/placeholder implementations
5. Do not skip compatibility tests for new encoding paths
6. Do not apply speculative fixes — find root cause first
7. Do not add comments explaining obvious code
8. Do not create abstractions for hypothetical future needs
9. Do not rely on training data for dependency versions — check online
10. Do not create documentation files unless explicitly requested

## Development Workflow

- Always run `golangci-lint run` before committing
- Tests must defend real invariants — no coverage-padding tests
- Root cause analysis is mandatory before any bug fix
- Compatibility with JavaScript Yjs is the ultimate correctness test
- When in doubt about encoding behavior, read the Yjs JS source

## Layout

The repository root is a doc-only package (`doc.go`, zero exports). The CRDT
lives in `crdt/` as one package and is NOT to be split further: its white-box
tests reach into unexported state deliberately, so every internal boundary would
either export that state or lose the test.

- `crdt/` — the CRDT. `doc.go` (Y.Doc), `y_text.go`/`y_array.go`/`y_map.go`
  (shared types), `encoding.go`/`decoding.go` (binary primitives),
  `update_{encoder,decoder}_v1.go` and `update_{encoder,decoder}_v2.go` plus
  `update_encoder_decoder_v2.go` (codecs, V2 including delta-coded delete sets),
  `updates.go` (update processing and merge), `compatibility_test.go`
- `protocol/` — sync and awareness framing
- `backend/` — ports (`persistence`, `cluster`), shipped defaults (`memory`,
  `hub`), and the public `conformance` suites
- `internal/lib0` — encoding primitives shared with the CRDT
- `internal/oracle` — the differential harness
- `internal/apicontract` — the module-wide frozen public API contract

## Paths in tests

Repository assets are reached through `repoPath(t, ...)`, which walks up to
go.mod. Bare relative literals resolve against the package directory and broke
six times during the crdt/ move — one of them by SKIPPING rather than failing,
which reports success while testing nothing.
`crdt/repository_path_guard_test.go` enforces this and rejects the bare form by
name.

## Full Constitution

See `.specify/memory/constitution.md` for the complete set of
principles and governance rules.

## Active Technologies
- Go 1.24+ + None at runtime (stdlib only); tests use the Go stdlib `testing` package (001-v2-encoding-and-sync-protocol)
- N/A (library) (001-v2-encoding-and-sync-protocol)

- Go 1.26, zero module dependencies (copystructure was the last one and is gone; tests are stdlib-only and pin a deterministic ClientID via the `WithClientID` `NewDoc` option)

## Recent Changes

- **Phase 1 — Correctness Completeness (002)**: closed the known Yjs-parity gaps,
  each verified against yjs@13.6.31 / y-protocols@1.0.7 source: nested-type
  delete/GC cascade (`ContentType.Delete`/`GC`), functional subdocuments, Y.Text
  formatting (negation pre-pass, delta guards, nested types in deltas), snapshot
  V1/V2 split (V2 bytes byte-identical to Yjs), XML `ToString` parity
  (`xmlAttrValueString`), awareness reaper (goroutine + mutex + `Destroy`, ms
  units), and all 7 UndoManager gaps (6-arg `redoItem` with remote-map-change
  preservation + `ignoreRemoteMapChanges`, doc scope, selective `Clear`,
  stack-item-updated/cleared events, `CanUndo`/`CanRedo`, `Destroy`). The
  cross-impl fuzz gate (`fuzz/`) was widened to all five surfaces with
  `FUZZ_STRICT_{SUBDOCS,GC,SNAPSHOT,XML}` flags (all green).

<!-- SPECKIT START -->
Active feature plan: `specs/004-full-parity-coverage/plan.md`
(Full Parity Coverage & Awareness Reaper Redesign). Feature 003 merged proven byte-exact with
`yjs@13.6.31` at 1.1M seeds, then review found eleven defects the gate had been green over — because
coverage is a property of the GENERATORS, not the seed count. All seven generators together emit
five op kinds; relative positions, sync and awareness have zero differential coverage; undo has
none; and direction A never runs this library's constructors. This feature widens what is checked
(undo differential, direction B, the three wire formats, read-path coverage, per-surface fault
injection) under a three-tier gate where EVERY surface is enforced at PR time. It also closes the
undo ordering deviation — which Phase 0 traced to StructStore client first-insertion order, not the
delete set — and splits presence into a plain type (no goroutine, exported fields) and a managed
type (owns the reference timer; parity claims attach to it), since the reference's local-renewal
heartbeat cannot be made lazy. For technical context read that plan plus its sibling `spec.md`,
`research.md`, `data-model.md`, `contracts/`, and `quickstart.md`. (Prior features:
`specs/003-oracle-and-value-rep/`, `specs/002-phase1-correctness-completeness/`.)
<!-- SPECKIT END -->
