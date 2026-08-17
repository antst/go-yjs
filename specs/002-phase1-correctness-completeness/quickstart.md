# Phase 1 Quickstart & Validation

How to validate each fix end-to-end. Run from the repository root. Implementation details live in
`tasks.md` (next) and the code; this is the run/validation guide.

## Prerequisites
- **Go 1.24+** (`go version`).
- **Node.js + `yjs@13.6.31`** for the cross-impl harness. First step (FR-035):
  ```bash
  # re-verify it is still the current latest before pinning
  npm view yjs version
  (cd fuzz && npm i yjs@13.6.31) && (cd v2_test_fixtures && npm i yjs@13.6.31)
  ```
  Then read the actual source under `fuzz/node_modules/yjs/src/*.js` when implementing each fix;
  cite `file:line`. Never use training data for Yjs behavior.

## Core test commands
```bash
go test ./...                       # all unit tests
go test ./... -race                 # awareness concurrency (1.7A) MUST pass
go test ./... -coverprofile cover.out && go tool cover -func=cover.out   # ≥95% on new code
golangci-lint run                   # clean before each commit
bash fuzz/run-gate.sh               # cross-impl fuzz gate (set FUZZ_* flags below)
```

## Validation by work item (maps to Success Criteria)

| Item | Validate | Pass = |
|---|---|---|
| **1.8** gate | `bash fuzz/run-gate.sh` with all `FUZZ_STRICT_*` **off** on current base | green (harness proven) → SC-001 wiring |
| **1.1** subdocs | unit: insert `Doc` → in `GetSubdocs()`, autoLoad → `loaded`, delete → removed+destroyed; then `FUZZ_STRICT_SUBDOCS=1` | SC-003 |
| **1.2** GC cascade | unit: Array⊃Map, delete → children `Deleted()`, `toJSON` empty; gc=true → `GC` structs + identical re-encode; then `FUZZ_STRICT_GC=1` (+ nested strict) | SC-002 |
| **1.3** text | unit: bold run + unattributed insert → separate unformatted op; no `{retain:0}`/`{delete:0}`/`{insert:""}`; nested type in YText → one insert op; cross-impl `toDelta()` | SC-006 |
| **1.4** snapshot | unit: `DecodeSnapshotVx(EncodeSnapshotVx(s))==s` (V1+V2, multi-client); Go V2 bytes == yjs fixture; decode a yjs V2 snapshot; then `FUZZ_STRICT_SNAPSHOT=1` (gc=false) | SC-007 |
| **1.5** ordering | `go test -run Ordering -count=1` (encodes 100×) byte-identical == yjs fixture | SC-009 |
| **1.6** undo | unit: 2-client same map key → undo preserves remote (default), overwrites only with `IgnoreRemoteMapChanges`; whole-doc undo; coalesce events; redo-discard GC; `Destroy()` | SC-004 |
| **1.7A** awareness | unit (injected/shrunk clock + `-race`): stale remote reaped + `removed` event; local renews (not reaped); create/destroy N → stable goroutine count | SC-005, SC-010 |
| **1.7B** XML | unit: parity table `ToString()` byte-equal to yjs (element/text/fragment, object/bool/num marks, embed, nil attr, Hook); then `FUZZ_STRICT_XML=1` | SC-008 |

## Phase-1 Definition of Done (gate)
```bash
# all strict flags ON, single + concurrent, V1 + V2:
FUZZ_FEATURES=nested,subdocs FUZZ_GC=both FUZZ_SNAPSHOT=1 \
FUZZ_STRICT_SUBDOCS=1 FUZZ_STRICT_GC=1 FUZZ_STRICT_SNAPSHOT=1 FUZZ_STRICT_XML=1 \
  bash fuzz/run-gate.sh
go test ./... -race                 # green
go test ./... -coverprofile cover.out   # ≥95% new code
golangci-lint run                   # clean
# existing compatibility_test.go + compatibility_v2_test.go byte-exact fixtures stay green
# SC-012: no perf regression vs pre-phase baseline (bench gate, when available)
```
Done = every gap fixed/scoped, all `FUZZ_STRICT_*` ON & green, byte-parity maintained, `-race`
clean, no goroutine/listener leaks, upstream reconciliation honored (no merged fix reverted).
