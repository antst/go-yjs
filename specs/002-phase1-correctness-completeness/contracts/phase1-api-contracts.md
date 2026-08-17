# Phase 1 Behavioral Contracts (public API surfaces)

Contracts the implementation MUST satisfy. Signatures are conceptual (verified against code at
implementation); semantics MUST match `yjs@13.6.31` (FR-035) and preserve V1/V2 byte parity
(FR-032). Each contract names the work item, the FRs, and the strict gate it flips.

---

## C1 — Subdocument lifecycle (1.1; FR-008–011; gate: `FUZZ_STRICT_SUBDOCS`)
- `ContentDoc.Integrate(trans, item)`: sets `doc.Item=item`; `trans.SubdocsAdded += doc`; if
  `doc.ShouldLoad` then `trans.SubdocsLoaded += doc`.
- `ContentDoc.Delete(trans)`: if `doc ∈ trans.SubdocsAdded` remove it; else `trans.SubdocsRemoved
  += doc`.
- `ContentDoc.GC`: no-op (matches yjs).
- `ReadContentDoc`/`NewDoc`: `ShouldLoad = shouldLoad || autoLoad`.
- **Observable**: after inserting a `Doc` into a Map/Array it appears in `doc.GetSubdocs()`; an
  autoLoad subdoc fires `subdocs.loaded`; deleting moves it to removed+destroyed; cross-impl the
  structural subdoc invariant holds and the GUID set converges with yjs.

## C2 — Nested-type delete / GC cascade (1.2; FR-005–007; gate: `FUZZ_STRICT_GC` + nested-strict)
- `ContentType.Delete(trans)`: walk `_start` and `_map`; live child → `child.Delete(trans)`;
  tombstoned child with `clock < trans.BeforeState[client]` → `trans.MergeStructs += child`; then
  `delete(trans.Changed, type)`.
- `ContentType.GC(store)`: walk `_start` (and `_map` via `.Left`) calling `child.GC(store, true)`;
  then `_start=nil`, `_map=new`.
- **Observable**: deleting a nested type leaves empty `toJSON` and all children `Deleted()`;
  gc=true replaces children with `GC` structs; re-encoded update byte-matches yjs (V1+V2).

## C3 — Snapshot V1/V2 (1.4; FR-027–029; gate: `FUZZ_STRICT_SNAPSHOT`)
- `EncodeSnapshotV1(s) []byte` / `EncodeSnapshotV2(s) []byte`; `EncodeSnapshot ≡ V1`.
- `DecodeSnapshotV1(b) Snapshot` / `DecodeSnapshotV2(b) Snapshot`; `DecodeSnapshot ≡ V1`.
- `WriteStateVector(DSEncoder, sv)` / `ReadStateVector(DSDecoder) sv` (narrowed from the wide
  Update encoder/decoder).
- **Observable**: `DecodeSnapshotVx(EncodeSnapshotVx(s)) == s` for V1 and V2 (multi-client DS+SV);
  Go `EncodeSnapshotV2` bytes == yjs `encodeSnapshotV2`; Go decodes a yjs V2 snapshot; V1 default
  unchanged. (gc=false only.)

## C4 — Awareness lifecycle & expiry (1.7A; FR-020–023; tests: behavioral + `-race`, no fuzz gate)
- `NewAwareness(...)`: **auto-starts** the reaper goroutine (`time.Ticker(OutdatedTimeout/10)`).
- `Destroy()`: stops the reaper (async via stopCh; not joined, to avoid a self-deadlock); idempotent; no goroutine leak.
- `GetStates()`: returns a **copy**; safe under concurrent mutation.
- Reaper per tick: renew local if `now-LastUpdated[self] >= OutdatedTimeout/2`; reap remote if
  `now-LastUpdated >= OutdatedTimeout` (emits `removed`). One consistent time unit.
- **Observable**: stale remote removed + `removed` event; live local not reaped (clock advances);
  `-race` clean; create/destroy N times → stable goroutine count.

## C5 — UndoManager (1.6; FR-012–019; tests: behavioral + cross-impl where convergence matters)
- `RedoItem(trans, item, redoItems, itemsToDelete, ignoreRemoteMapChanges, um)` (6-arg, from
  3-arg); new `isDeletedByUndoStack`.
- `Scopes`: `IAbstractType | *Doc`; doc-scope supported; `GetDoc()` safe for a doc scope.
- New options/fields: `IgnoreRemoteMapChanges`, `CaptureTransaction`, `CaptureTimeout` (field),
  `CurrStackItem`; capture guard `lastChange>0`.
- Events: `stack-item-added`, `stack-item-updated` (coalesce/merge), `stack-cleared`.
- `Clear(clearUndo, clearRedo)`, `CanUndo()`, `CanRedo()`; redo-discard calls `keepItem(_,false)`.
- `Destroy()`: unregisters `afterTransaction` + tracked origins; `AddToScope`/tracked-origin APIs.
- **Observable**: undo preserves a concurrent remote map write by default, overwrites only with
  `IgnoreRemoteMapChanges=true` (final state byte-identical to yjs); whole-doc undo reverts a
  nested edit; coalesce → one `added` + one `updated`; `Clear()` → one `stack-cleared`; redo-
  discard unpins items (GC reclaims); `Destroy()` then mutate → no new entries. Reconcile with the
  already-merged seed-left/`redoItem` overlap to a single code path (FR-019).

## C6 — YText formatting & delta (1.3; FR-024–026; gate: existing text fuzz)
- `InsertText`: negation pre-pass — for each active `CurrentAttributes` key absent from the
  incoming `attributes`, set it to the `Null` sentinel before `MinimizeAttributeChanges`.
- `addOp`: append an op only when produced (no `{insert:""}`/`{delete:0}`/`{retain:0}`).
- `GetDelta`/`DeleteText`/`ToDelta`: handle `*ContentType` alongside `*ContentEmbed`
  (`GetContent()[0]` payload).
- **Observable**: unattributed insert into a bold run is unformatted; deltas have no no-op ops; a
  nested type in a YText appears as one insert op; cross-impl `event.delta`/`toDelta()` match yjs.

## C7 — XML string serialization (1.7B; FR-030; gate: `FUZZ_STRICT_XML`)
- `xmlAttrValueString(any) string` (shared): `nil→"null"`; primitives `%v`; arrays comma-joined;
  objects `[object Object]`. Used by `YXmlElement.ToString` and `YXmlText.ToString`. Boolean/
  string formatting marks always emit their wrapper node.
- **Observable**: `ToString()` byte-identical to yjs across the parity table (nested elements,
  object/boolean/numeric marks, embed, nil attr, Hook). No entity-escaping; no `ToDOM` (out of
  scope).

## C8 — Ordering guard (1.5; FR-031)
- No API change. A regression test encodes a ≥3-key/≥2-client `YMap` 100× and asserts byte-
  identical output == a checked-in yjs fixture (update + SV + DS).
