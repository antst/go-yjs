# Fuzz Gate Contract (work item 1.8)

The widened cross-impl gate is the regression oracle (Constitution V; FR-001–004). One
deterministic generator (mulberry32 seed), reused V1/V2 dual-apply machinery — **not** forked
scripts.

## Generator flags (env)
| Flag | Values | Meaning |
|---|---|---|
| `FUZZ_FEATURES` | csv of `nested,subdocs` | enable optional op classes. **`nested` is a baseline surface — always on** unless `FUZZ_FEATURES=0`; `subdocs` is opt-in. (`xmlhook` is **not yet implemented**: XML breadth is covered via `XmlText`/`XmlElement` ops; a distinct `Y.XmlHook` op class is deferred.) |
| `FUZZ_GC` | `on` \| `off` \| `both` | `on`/`both` emit `postGcState`. The implemented GC assertion is a single gc-on post-GC state check; the gc-on-vs-gc-off **two-run** comparison is simplified — gc-on struct re-encode parity is already proven by the byte-identity check. |
| `FUZZ_SNAPSHOT` | `0` \| `1` (or `on`) | emit snapshot blobs + restored state |
| `FUZZ_STRICT_SUBDOCS` | `0`/`1` (default `0`) | assert subdoc convergence |
| `FUZZ_STRICT_GC` | `0`/`1` (default `0`) | assert post-GC state + GC-struct re-encode |
| `FUZZ_STRICT_SNAPSHOT` | `0`/`1` (default `0`) | assert snapshot round-trip / bytes |
| `FUZZ_STRICT_XML` | `0`/`1` (default `0`) | assert XML `ToString` parity |

Nested-types assertion is **ON from 1.8** (no flag / always strict). All other strict flags
default OFF so 1.8 merges green; each fix PR flips exactly one ON.

## Record schema (JS → Go)
Existing: `seed`, `ops`, `updateV1`/`updateV2` (bytes), `state` (`{t,m,a,x}` JSON via
`ToString`/`ToJson`), `textDelta` (parsed; assertion still deferred). **New** (each present only
when its generator flag is on, consumed by `fuzz_gate_test.go`'s `fuzzSingleRec`):
- `xmlString` (STRICT_XML).
- `postGcState` (STRICT_GC) — JS's post-GC canonical state.
- `snapDocV1` (the gc=false doc update), `snapshotV1`, `snapshotV2` (bytes), `restoredState`
  (STRICT_SNAPSHOT).
- `subdocUpdateV1` (a V1 doc update embedding subdocs) + `subdocGuids` (the converged GUID set)
  (STRICT_SUBDOCS) — there is **no** `{guid: toJSON}` subdocs object; embedded subdocs do not
  round-trip through `toJSON`.

Keep `canonical.js` ↔ `fuzzCanon` in lockstep.

## Assertions (per case)
1. V1-apply, V2-apply, and Go V2 re-encode round-trip all converge to `state` (single +
   concurrent, 6 apply orders) — existing, retained. A byte-identity block re-encodes through the
   full-doc V1/V2 paths and compares bytes to the JS originals (key-ordering guard).
2. With its strict flag ON:
   - **STRICT_SUBDOCS**: apply `subdocUpdateV1`, then `GetSubdocs()` GUID set == `subdocGuids`
     (structural GUID-set convergence — not a `{guid: toJSON}` comparison).
   - **STRICT_GC**: apply V2 to a gc=true doc, then its canonical state == `postGcState` (JS's
     post-GC `toJSON`); the `ContentType.GC` re-encode parity is covered by the byte-identity block.
   - **STRICT_SNAPSHOT**: rebuild the gc=false doc from `snapDocV1`; Go `EncodeSnapshotV2` bytes
     == `snapshotV2`; a yjs V2 snapshot decodes (`DecodeSnapshotV2`); restored doc == `restoredState`.
   - **STRICT_XML**: `XmlFragment.ToString()` == `xmlString` (byte-equal).
3. Keep the `recover()` around each apply so current no-op stubs report (not crash) until their
   fix lands.

## Version
Generator + fixtures pin **`yjs@13.6.31`** (bump from 13.6.27; verified current 2026-06-24).
Regenerate existing `v2_test_fixtures/` against it (a byte diff is a real finding).

## Acceptance (1.8 done)
Generator emits cases for all five areas; Go consumer (`fuzz_gate_test.go`) decodes/compares the
new payloads; with all strict flags OFF the widened gate is **green on the current base** (proves
harness wiring, not the fixes). Phase-1 done = all `FUZZ_STRICT_*` ON and green (SC-001).
