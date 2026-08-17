# Research: Full Parity Coverage & Awareness Reaper Redesign

Phase 0 — decisions (Decision / Rationale / Alternatives). Every claim about reference behaviour is
verified against the pinned `yjs@13.6.31` / `y-protocols@1.0.7` source under `fuzz/node_modules/`,
per Constitution XIII (No Assumptions). File paths below are relative to `fuzz/node_modules/`.

---

## R1 — Where the reference's undo restoration order originates (FR-001a)

**This was the feature's main open question**, and the answer moves the fix further down the stack
than the spec assumed. The spec says the deviation "requires an ordering that does not depend on Go
map iteration"; the source shows *which* map iteration, and it is not the delete set.

**Traced chain**, each step verified in source:

1. `yjs/src/utils/StructStore.js` — `StructStore.clients` is a JS `Map`, so iteration follows the
   order each client was **first inserted into the store**.
2. `getStateVector(store)` — `store.clients.forEach(...)` builds `sm`, a new `Map`, preserving that
   order. This becomes `transaction.afterState`.
3. `yjs/src/utils/UndoManager.js` — the stack item's insertions are built by
   `transaction.afterState.forEach((endClock, client) => { ... addToDeleteSet(insertions, client, ...) })`,
   so the delete set's client order inherits afterState's order.
4. `yjs/src/utils/DeleteSet.js` — `addToDeleteSet` uses
   `map.setIfUndefined(ds.clients, client, ...)`, which inserts on first touch, preserving order.
5. `sortAndMergeDeleteSet(ds)` sorts each client's **`DeleteItem` array** (`dels.sort((a,b) => a.clock - b.clock)`)
   and never reorders the clients themselves.
6. `iterateDeletedStructs(transaction, ds, f)` — `ds.clients.forEach(...)`, again insertion order.

**Decision**: reproduce **StructStore client first-insertion order**, and consult it where the undo
path currently iterates a Go map. The ordering is established at the store and merely carried by
everything downstream, so ordering the delete set alone would not reproduce it.

**Rationale**: Go's `StructStore.Clients` is `map[Number]*[]IAbstractStruct` and `GetStateVector`
returns `map[Number]Number` (`struct_store.go:44-53`) — the order is lost at step 1, before the
delete set exists. The existing `(client, clock)` sort in `undo_manager.go` was an attempt to
restore *a* determinism after that loss, which is why it is canonical but not the reference's.

**Why the orders should agree once tracked**: both implementations process the same op stream in
the same sequence, so "first time this client was seen" is the same event on both sides. This is a
prediction, not a proven fact — and it is precisely what the US1 differential exists to test. If
they disagree, the differential says so at low seed counts rather than silently.

**Alternatives considered**:
- *Order the delete set only* — rejected; the order is inherited from afterState, so a delete set
  ordered by anything else still diverges.
- *Keep the `(client, clock)` sort and weaken the comparison* — rejected by the FR-006 rule and by
  the spec's undo-ordering clarification.
- *Sort clients numerically on both sides* — rejected; it would require patching the reference,
  and parity means matching the reference as it is.

**Scope note for planning**: this makes FR-001a an engine change reaching `StructStore`, not a
localized fix in `undo_manager.go`. The narrowest form is an insertion-order client slice on
`StructStore` consulted by the undo path, leaving encoding paths (which already sort deliberately
and are byte-exact) untouched.

---

## R2 — Direction-B feasibility, including snapshot and GC (FR-002)

**Decision**: direction B is feasible for every surface with the reference's existing public API.
No harness-side gap blocks it.

**Verified available** in `yjs@13.6.31` (all present as functions):
`applyUpdateV2`, `encodeStateAsUpdateV2`, `decodeUpdateV2`, `encodeStateVector`,
`decodeStateVector`, `snapshot`, `encodeSnapshot`, `decodeSnapshot`, `encodeSnapshotV2`,
`decodeSnapshotV2`, `createDocFromSnapshot`, `createRelativePositionFromTypeIndex`,
`createAbsolutePositionFromRelativePosition`, `encodeRelativePosition`, `decodeRelativePosition`,
`compareRelativePositions`.

**Per-surface shape**:
- *Shared types* — this library builds and encodes; reference `applyUpdateV2` then
  `encodeStateAsUpdateV2`; compare bytes.
- *Snapshot* — this library snapshots and encodes; reference `decodeSnapshotV2` then
  `encodeSnapshotV2`; compare bytes. `createDocFromSnapshot` additionally checks the snapshot is
  semantically usable, not merely re-encodable.
- *GC* — GC has no encoding of its own; it is a state the document reaches. Direction B is a
  gc-enabled document built and encoded by this library, applied and re-encoded by the reference.
  Note the 003 `FUZZ_STRICT_GC` surface was a tautology (`typeof doc.gc === 'function'` is never
  true, since `Doc.gc` is a boolean field); GC structural parity comes from byte identity, and the
  visible-state check is a separate, weaker invariant.

**Alternatives considered**: a JSON round-trip instead of byte comparison — rejected, it cannot
detect non-canonical encodings, which is the only reason direction B exists.

---

## R3 — Wire-format differentials (FR-004)

**Decision**: all three uncovered wire formats are reachable with the pinned reference API.

- *Relative positions* — `createRelativePositionFromTypeIndex` / `encodeRelativePosition` /
  `decodeRelativePosition` / `createAbsolutePositionFromRelativePosition` /
  `compareRelativePositions`. Both directions: encode here → resolve there, and vice versa. The
  interesting cases are anchors whose target is later deleted and then garbage-collected (already
  listed as a spec edge case).
- *Sync protocol* — `y-protocols/sync.js` exports `writeSyncStep1`, `writeSyncStep2`, `writeUpdate`,
  `readSyncMessage`, `readSyncStep1`, `readSyncStep2`, `readUpdate`. A full exchange can be driven
  message-by-message with each side alternately producing and consuming.
- *Awareness* — `y-protocols/awareness.js` exports `Awareness`, `encodeAwarenessUpdate`,
  `applyAwarenessUpdate`, `removeAwarenessStates`, `modifyAwarenessUpdate`. Per FR-004 the
  comparison includes emitted change/update payloads, not just the resulting presence map.

---

## R4 — Ordered-map approach for FR-001a (Constitution III)

**Decision**: no new runtime dependency. Use the insertion-ordered pattern already in this
repository — `type_define.go`'s `objectData{keys []string; m map[string]any}`.

**Rationale**: Constitution III forbids third-party runtime dependencies in the engine without an
approved spec clarification, and every popular Go ordered-map package (`wk8/go-ordered-map`,
`elliotchance/orderedmap`, `iancoleman/orderedmap`) is internally a map plus a slice or linked
list — the same construction, so there is no performance argument either. What R1 needs is narrower
still: one insertion-order key slice on `StructStore`, not a general-purpose container.

**Alternatives considered**: adding an ordered-map dependency — rejected on both principle and
absence of benefit.

---

## R5 — Fault injection with no exempt surfaces (FR-008)

**Decision**: fault injection is expressed as **corrupting the expected value** for a surface,
which applies uniformly to anything the oracle compares.

**Rationale**: the spec removed the "unproven" escape hatch, so every surface must be faultable.
Any surface producing a comparable artefact (encoded bytes, rendered text, a delta, an event
payload) can have that artefact perturbed; if the oracle does not then report divergence, the
comparison is not wired correctly — which is the finding the self-test exists to produce. The 003
self-test compared this library against itself and covered one type only, so it demonstrated
encoder sensitivity rather than cross-implementation detection.

**Alternatives considered**: per-surface bespoke fault kinds — rejected as unnecessary; the
mutation kinds from 003 (drop-op, dup-op, reorder-op, permute-attr, corrupt-expectation) already
generalise, with applicability recorded per surface so an inapplicable kind is not silently counted.

---

## R6 — Presence type split (FR-012)

**Decision**: two types — a plain type holding exported state fields and never starting a
goroutine, and a managed type that owns the ticker and exposes accessors copying under its lock.

**Rationale**: verified in `y-protocols/awareness.js`, the reference constructor starts
`setInterval(..., floor(outdatedTimeout / 10))` — 3s at the pinned `outdatedTimeout = 30000` — doing
two things: renewing local state when half the timeout has elapsed (`setLocalState(getLocalState())`,
an *outbound* republish) and reaping remote clients past the full timeout via
`removeAwarenessStates(..., 'timeout')`, which emits `change` and `update`. Reaping is a read-time
judgement and can be lazy; renewal is triggered by elapsed time and cannot. Separating the types
makes "exported fields plus a library-owned writer" unrepresentable rather than merely discouraged
— relevant because an unguarded concurrent map read in Go aborts the process rather than raising a
recoverable error.

**Alternatives considered**: one type with exported fields and a documented contract — rejected in
clarification; a comment is not a safety mechanism for a fatal condition.

---

## R7 — Coverage report derived from the API surface (FR-005a)

**Decision**: derive the exercised-operation report from the package's exported method set, and
fail when an operation producing observable output has no generator invoking it.

**Rationale**: FR-005a requires the report not be a hand-maintained list, because such a list goes
stale silently when a method is added. Go's own toolchain can enumerate the exported method set of
the shared types, so the report can be generated rather than curated.

**Mechanism — pinned, because the obvious approach does not compile.** `internal/oracle` MUST NOT
import the root package. Verified: `native_diff_test.go`, `fuzz_gate_test.go` and
`oracle_selftest_test.go` are all `package y_crdt`, so if they import `internal/oracle` while
`internal/oracle` imports `y_crdt` to enumerate its exported methods, the test binary has an import
cycle and fails to build.

**Decision**: derive the operation set by **reflection over instances passed in by the caller** —
the root package hands `internal/oracle` values of its shared types, and the harness reflects over
their method sets. Dependency points one way only (`y_crdt` → `internal/oracle`), so no cycle
exists. This keeps FR-005a's anti-staleness property: the method set comes from the type at
runtime, not a list.

**Alternatives considered**:
- *`internal/oracle` imports `y_crdt` directly* — rejected: import cycle, a compile error.
- *AST parsing via `go/packages`* — viable and cycle-free, but adds a toolchain dependency and
  parses source rather than observing the actual types; rejected as heavier for no gain.
- *Move the coverage test to an external `y_crdt_test` package* — would break the cycle, but only
  for that one test file while the rest of the differentials stay in-package; rejected as a
  partial fix that leaves the constraint implicit.
- *A checked-in list reviewed by hand* — rejected by FR-005a for exactly the decay mode it
  describes.

**Exercised-op derivation (the other half)**: `Operations` comes from reflection above, but
`Exercised` must come from the generators, which are JavaScript. The link MUST be explicit: each
generator emits the op names it produced into its corpus health output, and a declared
op-name → Go-method mapping is checked for **exhaustiveness in both directions** — an op with no
mapped method, or a method no op maps to, fails. Without this, SC-003 and FR-005a rest on an
undefined join between a JS op name and a Go method name.

---

## Open items deliberately left to implementation

- Whether StructStore client first-insertion order agrees between implementations across all op
  streams (R1). Predicted yes; the US1 differential is the test, and disagreement is a finding
  rather than a blocker.
- Per-surface seed weighting within the fast/full/scale tiers, tuned against the ~7 minute fast-tier
  budget (SC-001a) once the surfaces exist to measure.

---

## R8 — `generate.js` violates seeded determinism (found during T014, 2026-08-13)

**Finding**: C-H5.1 requires "same seed → same corpus, for every generator". `fuzz/generate.js`
does not satisfy it. Measured: two runs of the *identical* code at seed 1 produced different
`updateV1`/`updateV2`/`snapshot*`/`subdocUpdateV1` for every record, and different `state` for
**19 of 60** records in `concurrent` mode.

**Cause**: `generate.js` never pins a clientID (`grep -n clientID fuzz/generate.js` → no match), so
each `new Y.Doc()` gets a random one. Every encoding embeds it, and in `concurrent` mode the
clientID is also the merge tie-break, so the *converged state itself* legitimately varies run to
run. The five `native_diff_*.mjs` generators do pin it (`doc.clientID = 1`) and are byte-stable —
which is why T014's migration verified byte-identical on those five.

**Why it matters**: a divergence found at seed N cannot be reproduced by re-running seed N. The
seed identifies the op stream (verified stable: `ops` matched across runs) but not the document
identity, so the failing case cannot be regenerated for diagnosis. That is the reproducibility the
seeded-corpus design exists to provide.

**Not attributable to this feature** — pre-existing, and it did not affect T014's migration
verification, which used the byte-stable generators plus the deterministic fields of the others.

**Recorded as an open finding rather than fixed here** (FR-006: a deliberate restriction or known
gap is recorded, not absorbed silently). Pinning the clientID would change every corpus the
update-gate produces and is a change to make deliberately, on its own, with the gate re-verified —
not folded into a DRY migration.

---

## R9 — R1 was half the fix: the order must be carried through ITERATION too (2026-08-13)

R1 identified where the reference's undo order originates (StructStore first-insertion order) and
concluded the fix reaches `StructStore`. Implementing that alone left the undo differential red —
and, worse, **nondeterministic**: the same corpus gave different divergence counts run to run.

**Root cause of the residual**: `IterateDeletedStructs` walks `ds.Clients`, a Go map. Ordering the
delete set's *construction* does nothing if its *iteration* is randomised. The reference iterates a
JS `Map` there, so insertion order is preserved end to end; the Go port lost it at the last hop.

So the order has to be carried at BOTH layers: `StructStore.clientOrder` (where it originates) and
`DeleteSet.clientOrder` (how it is walked). `AddToDeleteSet` records first-touch order and
`ClientOrder()` replays it, falling back to a numeric sort for delete sets built by direct map
assignment so the walk is never randomised.

**Measured effect**:

| State | Divergence (400 seeds) | Reproducible? |
|---|---|---|
| Before any ordering fix | 21/400 | no |
| StructStore ordering only | 4/400 | **no** — varied run to run |
| Both layers ordered | 7/400 | **yes** — identical seed list across 3 runs |

The middle row is the trap: 4 looks better than 7 but was an unreproducible sample. The stable 7 is
strictly more useful, because those seeds can now be diagnosed.

**RESOLVED** — root cause of the residual 7, found by tracing seed 68 op-by-op against the
reference (`fuzz/undo_trace.mjs`).

The divergence appeared at the `undo` op with **identical visible text and identical stack
lengths**: purely structural. Decoding both states with the reference showed the same two items
restored with **swapped clocks** — Go created `"e"` then `"c"`, the reference `"c"` then `"e"`.
Instrumenting the reference's stack item showed its deletions delete set ordered **client 2 before
client 1**, while Go used 1 before 2.

Why: `MergeDeleteSets` and `NewDeleteSetFromStructStore` build their maps by **direct assignment**,
bypassing the order tracking, and iterate their sources with `range` over a Go map. The reference's
`mergeDeleteSets` iterates `dss[i].clients` (a JS Map) and `merged.clients.set(...)`, preserving
first-touch order; `createDeleteSetFromStructStore` likewise iterates `ss.clients`. So the order
was lost at construction even though `AddToDeleteSet` recorded it.

Fixed at every construction site — merge, store-derived, and the decode path — each now iterating
its source in order and recording first-touch. Result: **0/400, then 0/3000 seeds, stable across
runs.**

**The general lesson, third instance of it in this feature**: the reference carries client order
through a *chain* of Map iterations, and Go loses it at *every* hop that uses a bare `range`.
Fixing one hop moves the symptom rather than removing it. The hops are: StructStore (origin),
delete-set construction (merge / store-derived / decode), and delete-set iteration.

## T067pre — countability semantics for the text layer (decision, taken before the fix)

**Question the task forced open.** T067 originally said the whitelists should become "a `default:`
arm *or* `IsCountable()`". Those are two different semantics, so the fix could not be applied until
one was chosen (Constitution XIII).

**What the reference actually does.** `yjs@13.6.31` `src/types/YText.js` contains EIGHT switches on
`content.constructor`. They are not uniform, and that non-uniformity is the whole answer:

| ref line | function | shape |
|---|---|---|
| 65 | `ItemTextListPosition.forward` | `case ContentFormat:` / `default:` |
| 93 | `findNextPosition` | `case ContentFormat:` / `default:` |
| 307 | `formatText` insert/negate loop | `case ContentFormat:` / `default:` |
| 390 | `cleanupFormattingGap` | `case ContentFormat:` only, **no default** |
| 463 | `cleanupYTextFormatting` | `case ContentFormat:` / `default:` |
| 545 | `deleteText` | **whitelist**: `ContentType`, `ContentEmbed`, `ContentString` |
| 722 | event/delta path | **whitelist** |
| 1045 | `ToDelta` | **whitelist** |

**Decision: `default:`, not `IsCountable()`, and only at the five sites that have a `default:` arm.**

Reasons:
1. It is a *structural* translation of the reference rather than a semantic re-derivation, so there
   is no reasoning gap to be wrong about — Constitution XIII's point exactly.
2. `IsCountable()` is NOT equivalent. `ContentDeleted` is non-countable, yet the reference's default
   arm still reaches it and is stopped instead by the `!deleted` guard. Substituting a countability
   predicate would change which guard does the work, and would silently diverge for any content
   kind whose countability differs from its deleted-ness.
3. Three of the eight reference switches genuinely ARE whitelists. A blanket conversion would have
   broken `deleteText` and `ToDelta`. The fix is therefore surgical: four Go sites changed
   (`Forward`, `FindNextPosition`, the `FormatText` loop, `CleanupYTextFormatting`), three left
   alone deliberately, and `cleanupFormattingGap` needed nothing.

**Reachability — this was a live defect, not a latent one.** Probed against the reference: the
content kinds reachable inside a `Y.Text` via its own API are `ContentString`, `ContentEmbed`,
`ContentType`, `ContentFormat` and `ContentDeleted`. `ContentDeleted` always carries the deleted bit
(verified 100/100 over randomised delete streams), so the whitelist agreed there. But a document
whose root is read as `Y.Text` can legitimately receive an update in which that root carries
array-shaped items — the item list is the same machinery — and a plain `applyUpdate` then puts
`ContentAny` and `ContentBinary` into the text. The reference counts them (length 5 for
`ContentAny(2) + ContentBinary(1) + ContentString(2)`); Go did not, so every later index was wrong.

**Verification.** `y_text_content_kind_test.go` pins nine cases (six insert positions plus format,
delete and baseline) against `yjs@13.6.31` with client IDs and GC pinned, comparing length,
`ToString`, `ToDelta` and the full `encodeStateAsUpdate` bytes. Four cases failed before the fix and
all nine pass after. Re-gated at full-tier volume afterwards: text 25 000 seeds (V1 and V2),
applyDelta / array / map / xml 3 000 each — zero divergences.

## T078a — FR-016 bar (b): can the oracle catch each defect again?

FR-016 sets two bars. Bar (a) — every defect ships a test that fails when the fix is reverted — is
the minimum and is met for all of them. Bar (b) is the harder one: a defect on a *registered
surface* must be reachable by that surface's **differential**, so a recurrence is caught by the
oracle and not only by a unit test someone remembered to write. Bar (b) was VERIFIED by reverting
the fix and re-running the surface, not asserted from inspection.

| # | Defect | Surface | Bar (b) | How it was verified |
|---|---|---|---|---|
| 1 | Undo entry ordering (StructStore client first-insertion order) | `undo:A` | met | 3 000 seeds; ordering divergence was how it was FOUND |
| 2 | Phantom undo entry from an empty transaction | `undo:A` | met | stack comparison diverges on revert |
| 3 | Redo lost to an empty transaction | `undo:A` | met | stack comparison diverges on revert |
| 4 | Tombstone resurrection on redo | `undo:A` | met | state bytes diverge on revert |
| 5 | Delete-set client ordering | `undo:A`, `update:A` | met | both surfaces diverge on revert |
| 6 | `YXmlFragment.ToString` dropped non-XML children | `xml:A` | met | `str` comparison diverges |
| 7 | Binary attribute rendered `[1 2 3]` not `1,2,3` | `xml:A` | **met only after widening the generator** | see below |
| 8 | Relative-position panic on a non-map root | `relpos:A` | met | the differential is what surfaced the panic |
| 9 | Text content-kind countability (FR-014d) | `text:A/B`, `applyDelta:A` | met | 25 000 + 3 000 seeds after the change |
| 10 | Presence/reaper split | `awareness:A` | met | update payload compared against the reference |
| 11 | `UndoManager.AddToScope` dropped slices | — | **NOT reachable** | see below |
| 12 | Snapshot cross-format decode (FR-015) | — | **not applicable** | see below |
| 13 | `ToDelta` with a snapshot panicked (unhashable map key) | `snapshot:A` | **met only after widening the generator** | see below |
| 14 | Plain `ToDelta` was emitted by the corpus but never compared | `update:A` | met | comparison added in both modes |

### #7 — the case where bar (b) was genuinely unmet

The binary-attribute fix shipped with a passing unit test, and the `xml` surface was registered, so
by inspection bar (b) looked satisfied. It was not: `native_diff_xml.mjs` drew attribute values from
`['x','y'] | int | true | null` and never emitted a `Uint8Array`, so **no seed at any volume could
reach the `[]uint8` arm of `xmlAttrValueString`**. The oracle would have been green through a
regression.

Fixed by widening the generator to emit binary attribute values (tagged `{"__bin":[...]}` on the
wire, since a `Uint8Array` does not survive `JSON.stringify` as an array) and teaching the Go replay
to reconstruct them. Verified by reverting the fix: 1 102 of 3 000 cases now diverge on `str`;
restored, 0 of 3 000. This is the concrete argument for FR-016 bar (b) existing at all — a
registered surface is not the same as a surface whose generator can reach the code.

### #13 — a public code path that had never executed

Found while chasing the touched-code coverage floor, not by review. `SplitSnapshotAffectedStructs`
keyed `Transaction.Meta` with the **function value itself** — a literal port of the reference, where
`transaction.meta.set(splitSnapshotAffectedStructs, …)` works because a JS `Map` keys functions by
identity. Go func values are not hashable, so the call panicked with *"hash of unhashable type"*
every single time. Since that is the only path into it, `ToDelta` with any non-nil snapshot
panicked outright: **the entire track-changes rendering was dead code that had never once run**.

The oracle could not have caught it, for two independent reasons:

1. `generate.js` emitted only the PLAIN `toDelta()`, never `toDelta(snapshot, prevSnapshot)`.
2. The gate declared a `TextDelta` field and then **never compared it** — so even the plain delta
   had no coverage in the update surface.

Both are now closed. The snapshot surface emits a mid-stream snapshot as `prevSnapshot` so the
`added` and `removed` ychange branches are both reached, and the delta is compared in single AND
concurrent modes. Verified with teeth: reverting the fix takes the gate from 300/300 pass to
300/300 fail. Go's output is pinned byte-for-byte against `yjs@13.6.31` for the plain,
snapshot-aware and `computeYChange` variants.

### #14 — the emitted-but-unchecked field

`textDelta` had been in the corpus since feature 002 and the gate never read it. Comparing document
`toJSON()` does NOT imply delta equality: two documents can render identical characters with
different formatting-run boundaries. Now compared in both modes, including the permuted-apply-order
convergence check, where formatting-run divergence is exactly the failure a character-level
comparison hides.

This is the same lesson as #7, in a different place: **a field that is generated is not a field that
is checked**, just as a registered surface is not a surface whose generator can reach the code.

### #11 — outside the oracle's reach, recorded as a limit

`AddToScope` accepting a slice is a **Go-side API-shape** defect. The reference's `addToScope` takes
a JS array natively, so there is no reference behaviour to differ from: the oracle compares
documents and update bytes, and a scope argument that Go silently dropped produces a different
document only via undo operations the generator would have to be told to perform against a
slice-shaped scope. No surface can reach it. Covered by bar (a) only
(`TestAddToScopeAcceptsSlices`), and recorded here as a known limit of the oracle's reach rather
than left unstated.

### #12 — not a defect, so bar (b) does not apply

The snapshot cross-format decode is a property of the FORMAT (no version marker) and is identical in
the reference. There is no divergence for a differential to find. Pinned instead by
`TestSnapshotCrossFormatDecodeMatchesReference`. See the FR-015 finding in `spec.md`.

## T072 — scale-tier results

Per-cell volumes recorded here rather than left implicit, since SC-001's floor is exactly the kind
of claim that rots when it lives only in a CI log. The floor itself is now enforced mechanically
(`oracle.TierFloor` / `CheckCellVolume`, wired into `fuzz/run-gate.sh`), so a scale run that would
under-serve a cell fails rather than silently reporting success — see T071b.

### Full scale run through the entrypoint — 1 000 000 seeds, both directions, ZERO divergences

`bash fuzz/run-gate.sh --tier scale --dir both`, all thirteen surfaces, **877 s wall clock**.
Per-cell volume 76 923, i.e. **7.7x the 10 000-seed floor** SC-001 requires, and the floor is now
checked mechanically before the run rather than read off afterwards.

| surface | seeds | result |
|---|---|---|
| `applyDelta` | 76 923 | 0 divergences |
| `array` | 76 923 | 0 |
| `awareness` | 76 923 | 0 wire, 0 event, 0 client |
| `gc` | 76 923 | 0 (via the update gate's `FUZZ_STRICT_GC` assertions) |
| `map` | 76 923 | 0 |
| `relpos` | 76 923 | 0 decode, 0 encode, 0 survival |
| `snapshot` | 76 923 | 0 (via `FUZZ_STRICT_SNAPSHOT`, now including snapshot-aware `ToDelta`) |
| `subdoc` | 76 923 | 0 (via `FUZZ_STRICT_SUBDOC`) |
| `sync` | 76 923 | 0 msg, 0 reply, 0 converge, 0 text |
| `text` | 76 923 | 0 V1, 0 V2, 0 V2 round-trip |
| `undo` | 76 923 | 0 state, 0 stack |
| `update` (single) | 76 923 | 76 923/76 923 pass, 7 692 300 ops |
| `update` (concurrent) | 76 923 | 76 923/76 923 pass, 16 923 060 ops — the permuted-apply-order convergence invariant |
| `xml` | 76 923 | 0 state, 0 `ToString` |
| **direction B** (6 surfaces) | 76 923 | 0 byte, 0 semantic |

### Native-op surfaces, direction A, standalone run (80 000 seeds each, 400 000 aggregate)

| cell | seeds | state divergences | notes |
|---|---|---|---|
| `text:A` | 80 000 | 0 | also 0 V2 divergences and 0 V2 round-trip divergences |
| `array:A` | 80 000 | 0 | |
| `map:A` | 80 000 | 0 | |
| `xml:A` | 80 000 | 0 | 0 `ToString` divergences, now including binary attributes |
| `applyDelta:A` | 80 000 | 0 | |

Wall clock: 26.3 s to verify all 400 000 replayed cases (generation is the dominant cost and runs
separately). Every cell is far above the 10 000-seed floor.

## T079 — honest coverage statement

What the oracle proves, what it does not, and where the remaining risk is. Written so a reader can
tell the difference between *verified*, *tested*, and *merely implemented* — the distinction feature
003 lost when it reported a green gate over eleven defects.

### Proven by differential against `yjs@13.6.31`

Thirteen surfaces are registered and none is pending. Nineteen (surface, direction) cells are
realized, and every one runs in the **fast** tier — asserted mechanically in three places that all
derive from the registry rather than from a hand-kept list: an in-code invariant (`Register`
rejects a cell that omits the fast tier), a CI step, and the pre-push hook.

| direction | surfaces |
|---|---|
| **A** (reference generates → Go replays → byte-compare) | all 13 |
| **B** (Go generates → reference decodes/re-encodes) | text, array, map, xml, applyDelta, update, **snapshot**, **gc** |

Direction B is realized on the eight surfaces where this library's own constructors produce the
document — 21 cells in total.

Two of those were added last (T031/T032) and are worth naming, because each closes a blind spot
direction A structurally cannot see:

- **`snapshot:B`** — this library encodes the snapshot; the reference decodes it, re-encodes it to
  the same bytes, *and restores from it* via `createDocFromSnapshot`. The restore is the point:
  byte re-encodability alone would pass a snapshot that round-trips as bytes while being unusable
  to a consumer.
- **`gc:B`** — a gc-ENABLED document built and encoded here, applied and re-encoded there. In
  direction A the reference collects before Go ever sees the bytes, so this library's OWN garbage
  collection decisions — which items become GC structs, and how they encode — had never been
  compared against the reference at all. Verified on documents that genuinely collect (96 structs,
  52 collected at seed 1), not on a build that would have encoded identically with GC off. That closes feature 003's unmet FR-006, under which direction A never exercised Go's
construction path at all — a whole half of the library was replayed-into but never generated-from.

### Verified but NOT differentially covered — stated plainly

- **V2 encoding.** Byte-exactness is real and now under randomised pressure: the text surface emits
  V2 alongside V1 on every seed, including a V2 encode → `ApplyUpdateV2` → V1 round-trip, and 25 000
  seeds ran clean. But V2 is **not a separate registered surface**; the other twelve surfaces
  compare V1 bytes only. V2 also has 22 curated cross-language fixtures. Treat V2 as
  *fixture-and-text-surface* verified, not as carrying the same continuous pressure as V1.
- **API completeness (updated).** All **95** of the reference's public exports now have a Go
  counterpart. The six that were genuinely absent — `typeMapGetAllSnapshot`,
  `snapshotContainsUpdate`, `equalDeleteSets`, `decodeUpdate`/`V2`, `obfuscateUpdate`/`V2` and
  `logType` — are implemented against the yjs source and covered by the oracle, not only by unit
  tests. Go's obfuscator is byte-identical to the reference. `AbstractConnector` is answered by a
  `Connector` INTERFACE rather than a ported class: the reference's version carries no behaviour and
  its own docstring advises against inheriting it, so the Go form of that contract is an interface a
  real transport implements.

  Two deliberate Go-side deviations, both recorded at the call site: `TypeMapGetAllSnapshot` emits
  keys sorted (Go map iteration is randomised, and the reference's insertion order is not
  reproducible), and `LogType` returns a string rather than printing.

- **Four reference V2 exports have no named Go equivalent.** `mergeUpdatesV2` and
  `parseUpdateMetaV2` are *reachable* — Go parameterises the codec, so
  `MergeUpdates(updates, NewUpdateDecoderV2, NewUpdateEncoderV2)` is the same operation, and
  `v2_api_reachability_test.go` proves it. `decodeUpdate`/`decodeUpdateV2` (a debug inspector) and
  `obfuscateUpdate`/`obfuscateUpdateV2` (a privacy scrubber for bug reports) are **absent in both
  V1 and V2** — a genuine API gap, not a codec gap.
- **`UndoManager.AddToScope` with a slice.** A Go-side API-shape defect with no reference behaviour
  to differ from. Unit-tested only; recorded above as outside the oracle's reach.
- **Snapshot cross-format decode.** Not a divergence at all — the format carries no version marker
  and the reference behaves identically. Pinned by a test, documented at the API.

### The exercised-op link (FR-005a), as built

The coverage report derives each surface's operation list by reflection, so a newly added public
method appears as unexercised without anyone editing a list. What was missing until T042a was the
other end of the join: the tests hand-listed which Go methods they *believed* the generators
reached, and `MarkExercised` silently ignored any name it did not recognise. A renamed generator op
therefore reduced real coverage while the report kept claiming full coverage.

Now the join is declared once (`operationOpMappings()`) as generator-op-name → Go-method, and
checked from both ends:

- `ValidateMapping` fails if a mapping entry names a method that does not exist on the type, **or**
  if a derived method is named by no op at all.
- `TestOperationCoverageOpsAreDeclaredByGenerators` reads the real corpora and pushes every op name
  the generators actually emitted through the mapping; an undeclared op fails.

Both halves found real drift the moment they were switched on: four mapping entries named methods
that do not exist (`GetLength`, `Splice`, `ForEach` on the wrong types) and had been silently
ignored for two features, and the `map` generator's `delete` op was declared as `del`.

### Where the risk actually sits now

The defects this feature found were **not** found by running more seeds. Every one came from
widening what the generators *emit* or what the gate *compares*:

- a surface registered but whose generator could not reach the code (binary attributes)
- a field generated for three features and never compared (`textDelta`)
- a public path with no generator input at all, which turned out to have never executed
  (`ToDelta` with a snapshot)

That is the standing lesson: **coverage is a property of the generators and the comparisons, not of
the seed count.** Feature 003 was byte-exact at 1.1M seeds and still had eleven defects. The
remaining risk is therefore concentrated in code the generators still cannot reach, not in code
they reach too few times. The fault-injection self-test (13 surfaces, 51 applicable kinds, 51
detected) is what keeps the comparisons themselves honest.

## Performance comparison against the reference (post-implementation measurement)

Suite: `perf_bench_test.go` + `fuzz/perf_bench.mjs`, 14 matched scenarios. Raw output committed as
`perf-go.txt` / `perf-yjs.json`. Apple M1 Max, Go 1.26, Node 26.5, `yjs@13.6.31`.

**Comparability is established, not assumed.** Both sides drive the same 32-bit LCG so they draw the
SAME index sequence (random-insert cost depends on where indices land, so "both random" would not be
enough), and the documents the two suites build were verified to **encode to identical bytes** at
both 2 000 and 10 000 ops. Setup is outside the timed region on both sides; JS warmup is discarded.

**Result: median 1.42x slower than the reference.** Go is FASTER on encoding (V1 1.55x, V2 1.13x)
and at parity on array insert (1.05x) and concurrent merge (1.15x). Worst gaps: `ToDelta` 4.12x,
apply V2 2.72x, sequential append 2.24x, apply V1 1.93x.

**Root causes, profiled:**

- The insert path's CPU profile is dominated by GC machinery (`kevent`, `madvise`,
  `pthread_cond_wait`) — allocation pressure, not compute. `Transact` accounts for **98%** of
  allocations, `CleanupTransactions` alone **47%**, `NewTransaction` **20%**. A single-character
  insert costs **62 allocations / 8.8 KB**: the transaction wrapper, not the CRDT work inside it.
- `ToDelta` allocations are almost entirely `NewObject` / `Object.Set`. `Object` backs every
  attribute set with a `map[string]any` plus a key slice, and attribute maps hold one or two keys in
  practice. A representation choice, not an algorithmic one.
- **Search markers are correct** — the classic port regression is absent. The cap matches the
  reference (80), and 2 k → 10 k ops costs 5.96x for a 5x workload, i.e. linear.

**Counter-intuitive finding worth recording for consumers:** batching all 10 000 appends into ONE
transaction is **20x SLOWER** than per-op transactions (679 ms vs 33 ms), despite allocating 4x
less — deferred item merging leaves the list unmerged for every subsequent walk. This is NOT a Go
defect: the reference shows the same pathology harder (769 ms vs 16 ms, 47x), and Go is *faster*
than the reference in the batched case.

**Ranked optimisation targets** (not done in this feature; performance was out of its scope):
1. Small-map representation for `Object` — widest gap, broadest reach.
2. Lazily allocate the per-transaction map/set/state-vector set — largest single allocation block.
3. V2 *decode* specifically — V2 encode is faster than the reference while V2 apply is 2.72x
   slower, so the asymmetry localises the cost.

Caveats: single machine, run-to-run noise ~±10% (treat ratios under ~1.2x as parity); synthetic
scenarios chosen to probe fragile shapes rather than model a real session — a recorded editing trace
is the next step for representativeness.
