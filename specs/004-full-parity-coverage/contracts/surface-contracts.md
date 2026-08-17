# Contract: Per-Surface Verification

What each surface must demonstrate. Existing surfaces are listed for completeness because C-H1.3
requires the registry to be exhaustive.

## Existing (retained, migrated onto the harness core)

| Surface | Direction A | Direction B (new) |
|---|---|---|
| `text`, `array`, `map`, `xml`, `applyDelta` | reference generates ops, this library replays natively, encoded state compared | this library constructs, reference decodes + re-encodes, bytes compared |
| `update` | reference update applied via V1 and V2 paths; 6 permuted orders for convergence | this library's encoded update round-trips |
| `snapshot`, `gc`, `subdoc` | strict-flag assertions | as above; GC via a gc-enabled document built here |

**C-S0**: `FUZZ_STRICT_GC`'s dead branch **was removed in `cbafa17`** (past tense — this is no longer feature work). `Doc.gc` is a boolean field, not a method, so
the previous guard never fired and `postGcState` duplicated `state`. GC *structural* parity comes
from byte identity; the visible-state check is a separate, weaker invariant and is stated as such.

## `undo` (US1, new)

- **C-S1.1**: Op streams interleave edits with undo, redo, scope restriction, capture coalescing,
  and stack clearing.
- **C-S1.2**: Comparison covers resulting state **and observable stack status** — whether undo/redo
  is available, plus stack-change events (FR-001b). A phantom undo entry and a lost redo each alter
  **no** encoded bytes; both occurred, so a bytes-only comparison cannot reach them.
- **C-S1.3**: Multi-participant streams included, compared **with no ordering carve-out**
  (FR-001a).
- **C-S1.4**: A capture window coalescing several edits into one step is exercised — without it,
  `Content.Copy` is never reached, which is how a tombstone-resurrection defect survived.

## `relpos` (US3, new)

- **C-S2.1**: Positions encoded here resolve identically there, and vice versa.
- **C-S2.2**: Includes anchors whose target is later deleted and then garbage-collected.
- **C-S2.3**: `compareRelativePositions` agrees on both sides for the same pair.

## `sync` (US3, new)

- **C-S3.1**: A full exchange driven message-by-message, each side alternately producing and
  consuming: step 1, step 2, update.
- **C-S3.2**: Both sides reach identical state.
- **C-S3.3**: Includes an exchange where one side holds state the other cannot yet interpret.

## `awareness` (US3, new)

- **C-S4.1**: Presence encoded here decodes there with matching entries and ordering.
- **C-S4.2**: Comparison includes emitted change/update payloads, not merely the resulting map
  (FR-004) — the events are what a consumer reacts to.
- **C-S4.3**: The managed type's renewal is verified against a reference peer: an otherwise-idle
  client is **not** dropped across a full timeout (SC-006a).
