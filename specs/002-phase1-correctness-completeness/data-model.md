# Phase 1 Data Model

Entities and state transitions *touched* by Phase 1. This is not a redesign — Phase 1 completes
behavior on existing structures. Field names are conceptual; exact Go identifiers are verified
against the code (and yjs source per FR-035) at implementation.

---

## ContentDoc (subdocument content) — work item 1.1
- **Holds**: a nested `*Doc` (`Doc`), and after integration a back-reference `Doc.Item`.
- **Relevant fields**: `Doc.ShouldLoad` (derived `shouldLoad || autoLoad`), `Doc.Gc`,
  `Doc.AutoLoad`, `Doc.Guid`.
- **State transitions** (driven by `ContentDoc.Integrate`/`Delete`):
  - *uninitialized* → **added**: on `Integrate` → `trans.SubdocsAdded += doc`, `doc.Item = item`.
  - **added** → **loaded**: if `ShouldLoad` → `trans.SubdocsLoaded += doc`.
  - **added** → *withdrawn*: if `Delete` in the same transaction it was added → remove from
    `SubdocsAdded` (no removal event).
  - **integrated** → **removed → destroyed**: on `Delete` (different transaction) →
    `trans.SubdocsRemoved += doc`; transaction cleanup destroys it.
- **Invariant**: `GC` is a no-op on `ContentDoc` (matches yjs).

## Nested type content (ContentType) + children — work item 1.2
- **Holds**: an `IAbstractType` (`Type`) whose content is the `_start` linked list of `*Item`
  plus the `_map` of key→latest `*Item`.
- **State transitions**:
  - **live** → **tombstoned (cascade)**: `ContentType.Delete` walks `_start` and `_map`; each live
    child → `item.Delete(trans)`; each already-tombstoned child with `id.clock <
    trans.BeforeState[client]` → appended to `trans.MergeStructs`; finally `delete(trans.Changed,
    Type)`.
  - **tombstoned** → **collected (GC)**: `ContentType.GC` walks `_start` (and `_map` following
    `.Left`) calling `item.GC(store, parentGCd=true)` → children replaced with `GC` structs via
    `ReplaceStruct`; then `_start=nil`, `_map=new`.
- **Invariants**: absent client in `BeforeState` ⇒ clock 0; post-delete `toJSON` of the type is
  empty; re-encoded update byte-matches yjs (GC-on and GC-off).

## Transaction (cascade plumbing) — used by 1.1/1.2
- **Relevant fields** (already present): `SubdocsAdded`, `SubdocsRemoved`, `SubdocsLoaded`,
  `MergeStructs`, `BeforeState`, `Changed`. Phase 1 only *feeds* these; no new fields.

## YText cursor / delta state — work item 1.3
- **`currPos.CurrentAttributes`** (map key→value, with `Null` sentinel): the negation pre-pass
  (1.3A) sets `attributes[key]=Null` for each active attribute not in the incoming insert.
- **Delta op accumulator** (`addOp`): an op is appended **only when produced** (guards:
  `deleteLen>0`, non-empty insert, `retain>0`); a `*ContentType` insert is represented as one
  insert op carrying the nested type (1.3B), in `GetDelta`/`DeleteText`/`ToDelta`.

## Snapshot — work item 1.4
- **Holds**: a delete set (`DeleteSet`) + a state vector (`map[client]clock`).
- **Encoding variants**: `EncodeSnapshotV1` (DS-V1 framing) and `EncodeSnapshotV2` (DS-V2);
  `EncodeSnapshot` ≡ V1. Decoders mirror: `DecodeSnapshotV1/V2`, `DecodeSnapshot` ≡ V1.
- **Dependency change**: `WriteStateVector`/`ReadStateVector` narrowed from the wide
  `UpdateEncoder/Decoder` to `DSEncoder/DSDecoder` (varint framing only).
- **Invariant**: round-trips in both versions; V2 bytes equal yjs; restorable only for gc=false.

## Awareness state — work item 1.7A
- **`States`**: `map[clientID]any` (presence payloads).
- **`Meta`**: `map[clientID]{Clock, LastUpdated}`.
- **Reaper** (new, owned by the `Awareness` instance): a goroutine + `time.Ticker`
  (`OutdatedTimeout/10`), a stop channel, and a `sync.Mutex` guarding `States`/`Meta`.
- **State transitions** per tick:
  - local present & `now-LastUpdated[self] >= OutdatedTimeout/2` → **renew** (re-set local; bump
    clock/LastUpdated).
  - remote `now-LastUpdated >= OutdatedTimeout` → **reap** → `RemoveAwarenessStates(..., "timeout")`
    emits `change`/`update` with `removed`.
- **Lifecycle**: reaper auto-starts on `NewAwareness`; `Destroy()` stops it (async via stopCh; not joined, to avoid a self-deadlock); no goroutine leak.
- **Invariant**: `GetStates` returns a copy; all mutators race-free; one time unit throughout.

## UndoManager — work item 1.6
- **`Scopes`**: `[]` of `IAbstractType | *Doc` (was type-only); plus stored `doc`.
- **New fields/options**: `IgnoreRemoteMapChanges bool`, `CaptureTransaction func(*Transaction)
  bool`, `CaptureTimeout` (field), `CurrStackItem`.
- **StackItem**: a reversible change with `Meta` (relative-position selection), insertions/
  deletions; `keepItem(item,false)` unpins on redo-discard.
- **Events**: `stack-item-added`, `stack-item-popped`, **`stack-item-updated`** (coalesce/merge),
  **`stack-cleared`** (new).
- **State transitions**: edit → push/coalesce (guard `lastChange>0`); undo/redo → pop + apply
  (`isDeletedByUndoStack` + `ignoreRemoteMapChanges` decide remote-write preservation); fresh edit
  → redo-discard with `keepItem(false)`; `Destroy()` → unbind listener + tracked origins.

## XML serialization value — work item 1.7B
- **`xmlAttrValueString(any) string`** (new shared helper): `nil→"null"`, primitives `%v`, arrays
  comma-joined, objects `[object Object]`; used by both `YXmlElement.ToString` and
  `YXmlText.ToString`. Boolean/string formatting marks always emit their wrapper node.

## Fuzz case record — work item 1.8
- **Existing**: per-case seed, op log, `State` (`{t,m,a,x}` JSON), `TextDelta` (parsed, unused).
- **New fields**: `subdocs` (`guid→toJSON`, sorted), `snapshotV1`/`snapshotV2` (bytes),
  `restoredState`, optional `postGcState`. Generator flags: `FUZZ_FEATURES`, `FUZZ_GC`,
  `FUZZ_SNAPSHOT`, `FUZZ_STRICT_{SUBDOCS,GC,SNAPSHOT,XML}`.
- **Nested-type strictness is always-on** (not toggleable): there is no `FUZZ_STRICT_NESTED`
  flag — nested types land strict immediately (per research.md R-1.8), so they are absent from the
  flag list by design.
