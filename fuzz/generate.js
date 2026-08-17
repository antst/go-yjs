'use strict';
// Cross-impl fuzz generator: emits NDJSON cases for the Go gate (fuzz_gate_test.go)
// to replay through this y-crdt fork. Extends the MVP-0 v1 harness with a V2
// path and Y.Text formatting coverage.
//
// Modes:
//   single     : one doc, random ops -> {updateV1, updateV2, state, textDelta}
//   concurrent : base -> two divergent streams -> exchange & converge in JS;
//                emit base/u1/u2 in BOTH v1 and v2 so Go can apply either format
//                in any order and must reach `state`.
//
// Usage: node generate.js <mode> <seedStart> <count> <opsPerCase>
const { canon } = require('./canonical');
const { mulberry32, getTypes, applyRandomOp, Y } = require('./ops');

const mode = process.argv[2] || 'single';
const seedStart = parseInt(process.argv[3] || '1', 10);
const count = parseInt(process.argv[4] || '100', 10);
const opsPer = parseInt(process.argv[5] || '50', 10);

const b64 = (u8) => Buffer.from(u8).toString('base64');

// Per-surface feature toggles, parsed per fuzz/contracts/fuzz-gate-contract.md:
//   FUZZ_FEATURES : CSV of optional op classes (e.g. `subdocs`). Nested types are a
//                   baseline surface (always on unless FUZZ_FEATURES=0 — see ops.js).
//   FUZZ_GC       : `on` | `off` | `both` (on/both emit postGcState).
//   FUZZ_SNAPSHOT : `0` | `1`/`on`.
// A FUZZ_STRICT_* flag also turns on its surface's generation so a strict run is
// self-contained. With everything off the records match the base gate (plus the
// harmless xmlString, only asserted under FUZZ_STRICT_XML).
const FEATURE_SET = new Set((process.env.FUZZ_FEATURES || '').split(',').map((s) => s.trim()).filter(Boolean));
const GC_MODE = (process.env.FUZZ_GC || 'off').toLowerCase();
const FEAT_SUBDOCS = FEATURE_SET.has('subdocs') || process.env.FUZZ_STRICT_SUBDOCS === '1';
const FEAT_GC = ['on', 'both', '1'].includes(GC_MODE) || process.env.FUZZ_STRICT_GC === '1';
const FEAT_SNAPSHOT = ['1', 'on'].includes((process.env.FUZZ_SNAPSHOT || '').toLowerCase()) || process.env.FUZZ_STRICT_SNAPSHOT === '1';

function genSingle(seed) {
  const rng = mulberry32(seed);
  const doc = new Y.Doc();
  const types = getTypes(doc);
  doc.transact(() => {
    for (let i = 0; i < opsPer; i++) applyRandomOp(rng, types, 'S');
  });
  const rec = {
    seed,
    ops: opsPer,
    updateV1: b64(Y.encodeStateAsUpdate(doc)),
    updateV2: b64(Y.encodeStateAsUpdateV2(doc)),
    state: canon(doc.toJSON()),
    // Y.Text delta carries the format attributes that toJSON() drops; the Go
    // gate compares it to assert formatting-attribute parity after V2 apply.
    textDelta: canon(types.text.toDelta()),
    // XML serialization (work item 1.7B). toJSON drops attribute/mark detail;
    // toString() is the byte-exact surface the Go gate compares under STRICT_XML.
    xmlString: types.xml.toString(),
  };

  // Six reference operations that had no Go counterpart until now. Unit tests prove the Go
  // versions RUN; only these fields prove they AGREE with the reference, which is the whole
  // reason the oracle exists (FR-016 bar (b)).
  {
    const u1 = Y.encodeStateAsUpdate(doc);
    const u2 = Y.encodeStateAsUpdateV2(doc);

    // obfuscateUpdate / obfuscateUpdateV2 — byte-exact. The obfuscator is deterministic (its
    // counter and caches are driven purely by traversal order), so anything less than a byte
    // comparison would be leaving evidence on the table.
    rec.obfuscatedV1 = b64(Y.obfuscateUpdate(u1));
    rec.obfuscatedV2 = b64(Y.obfuscateUpdateV2(u2));

    // decodeUpdate / decodeUpdateV2 — the introspection primitive. Compare the struct count and
    // the delete set it reports, and assert the two encodings describe the SAME document.
    const d1 = Y.decodeUpdate(u1);
    const d2 = Y.decodeUpdateV2(u2);
    rec.decodedStructs = d1.structs.length;
    rec.decodedStructsV2 = d2.structs.length;
    rec.decodedDs = canon([...d1.ds.clients.entries()]
      .sort((a, b) => a[0] - b[0])
      .map(([c, items]) => [c, items.map((it) => [it.clock, it.len])]));

    // equalDeleteSets — V1 and V2 must decode to equal delete sets, and a set with an extra
    // range must NOT compare equal (the negative case is what gives the check teeth).
    rec.dsEqualAcrossFormats = Y.equalDeleteSets(d1.ds, d2.ds);
    const bumped = Y.createDeleteSet();
    d1.ds.clients.forEach((items, c) => bumped.clients.set(c, items.map((it) => ({ ...it }))));
    bumped.clients.set(999999, [{ clock: 0, len: 1 }]);
    rec.dsEqualAfterExtraClient = Y.equalDeleteSets(d1.ds, bumped);

    // logType — its RENDERING is implementation-specific (the reference console.logs objects),
    // but the traversal it reports is comparable, and traversal is the only thing that can be
    // wrong. Emit the child counts the Go string must report.
    let n = types.text._start, total = 0, deleted = 0;
    while (n) { total++; if (n.deleted) deleted++; n = n.right }
    rec.logTypeChildren = total;
    rec.logTypeDeleted = deleted;
  }

  // GC surface (work item 1.2): GC must not change VISIBLE state. This was previously
  // `if (typeof doc.gc === 'function') doc.gc()` — but yjs Doc.gc is a BOOLEAN field, not a
  // method (verified: `typeof new Y.Doc().gc === 'boolean'`), so that branch never ran and
  // postGcState was just a second copy of `state` taken from an untouched doc. The Go check
  // then re-ran a comparison the base v2 variant already made: identical doc construction
  // (gc=true), identical input, identical expectation. It asserted nothing.
  //
  // Now postGcState is the toJSON of a gc=FALSE replay of the SAME seed. The main `doc` is
  // gc=true (Y.Doc defaults to gc:true, so GC has already run in it), so the Go check —
  // "apply the update to a gc=TRUE doc, compare against the gc=FALSE visible state" —
  // becomes a genuine cross-configuration invariant: collecting garbage must not change
  // what is observable, in either implementation.
  //
  // NOTE: GC *structural* parity is NOT what this surface covers, and never was — that
  // comes from the byte-identity block (Go's EncodeStateAsUpdateV2 vs yjs's), which
  // compares post-GC encodings directly.
  if (FEAT_GC) {
    const gdoc = new Y.Doc({ gc: false });
    const gt = getTypes(gdoc);
    const grng = mulberry32(seed);
    gdoc.transact(() => { for (let i = 0; i < opsPer; i++) applyRandomOp(grng, gt, 'G'); });
    rec.postGcState = canon(gdoc.toJSON());
  }

  // Snapshot surface (work item 1.4): a SEPARATE gc=false doc replayed with the
  // same seed (snapshots / createDocFromSnapshot need surviving structs). Emit its
  // V1 update (so Go rebuilds the same store), both snapshot encodings, and the
  // restored toJSON.
  if (FEAT_SNAPSHOT) {
    const sdoc = new Y.Doc({ gc: false });
    const st = getTypes(sdoc);
    const srng = mulberry32(seed);
    sdoc.transact(() => { for (let i = 0; i < opsPer; i++) applyRandomOp(srng, st, 'S'); });
    const snap = Y.snapshot(sdoc);
    rec.snapDocV1 = b64(Y.encodeStateAsUpdate(sdoc));
    rec.snapshotV1 = b64(Y.encodeSnapshot(snap));
    rec.snapshotV2 = b64(Y.encodeSnapshotV2(snap));
    // Instantiate the restored doc's shared types before toJSON: createDocFromSnapshot
    // returns them lazily, and toJSON only includes accessed types (the Go gate reads
    // them via GetText/GetMap/... so this keeps both sides apples-to-apples).
    const restored = Y.createDocFromSnapshot(sdoc, snap);
    getTypes(restored);
    rec.restoredState = canon(restored.toJSON());
    // Snapshot-AWARE toDelta (the track-changes rendering). The gate previously emitted only the
    // plain toDelta, so the snapshot-aware path had no differential coverage at all — and Go's
    // implementation of it panicked on every call ("hash of unhashable type": Transaction.Meta was
    // keyed with the function value, which JS Maps allow and Go maps do not). A whole public code
    // path was dead and the oracle could not see it. Emitting it makes the snapshot surface reach
    // that path (FR-016 bar (b)).
    //
    // A second, EARLIER snapshot is taken mid-stream so `prevSnapshot` differs from `snapshot` and
    // both the "added" and "removed" ychange branches are exercised rather than only one.
    const edoc = new Y.Doc({ gc: false });
    const et = getTypes(edoc);
    const erng = mulberry32(seed);
    const half = Math.max(1, Math.floor(opsPer / 2));
    edoc.transact(() => { for (let i = 0; i < half; i++) applyRandomOp(erng, et, 'S'); });
    const earlySnap = Y.snapshot(edoc);
    edoc.transact(() => { for (let i = half; i < opsPer; i++) applyRandomOp(erng, et, 'S'); });
    const lateSnap = Y.snapshot(edoc);
    rec.ychangeDocV1 = b64(Y.encodeStateAsUpdate(edoc));
    rec.ychangeEarlySnapV1 = b64(Y.encodeSnapshot(earlySnap));
    rec.ychangeLateSnapV1 = b64(Y.encodeSnapshot(lateSnap));
    rec.ychangeDelta = canon(et.text.toDelta(lateSnap, earlySnap));

    // typeMapGetAllSnapshot — the Y.Map counterpart of the snapshot-aware toDelta above, which
    // this feature found had never once executed in Go. Same history/time-travel area, so it gets
    // the same differential treatment rather than a unit test alone.
    //
    // Taken against the MID-STREAM snapshot (earlySnap on edoc), NOT the end-of-document one.
    // With an end-of-document snapshot every live value is visible in it, so the function
    // degenerates to typeMapGetAll and its whole reason for existing — walking each key's item
    // chain back to the version the snapshot can see — is never exercised. Verified: mutating the
    // left-walk out of the Go implementation did not fail the gate until this changed.
    //
    // The raw result holds CONTENT values, and a nested type is a live Y type with parent
    // back-pointers — canon() recurses forever on it. Both sides therefore project each value
    // through toJSON first, so what is compared is the value's SHAPE rather than its identity.
    rec.mapSnapshotAll = canon(Object.fromEntries(
      Object.entries(Y.typeMapGetAllSnapshot(et.map, earlySnap)).map(([k, v]) =>
        [k, v && typeof v.toJSON === 'function' ? v.toJSON() : v])));

    // snapshotContainsUpdate — must be TRUE for the update the snapshot was taken from and FALSE
    // once the document has moved on. Both directions are emitted; a predicate that always
    // returned true would otherwise pass.
    rec.snapContainsSelf = Y.snapshotContainsUpdate(snap, Y.encodeStateAsUpdate(sdoc));
    const moved = new Y.Doc({ gc: false });
    Y.applyUpdate(moved, Y.encodeStateAsUpdate(sdoc));
    moved.getText('t').insert(0, 'LATER');
    rec.snapContainsLater = Y.snapshotContainsUpdate(snap, Y.encodeStateAsUpdate(moved));
    rec.snapLaterUpdateV1 = b64(Y.encodeStateAsUpdate(moved));
  }

  // Subdocs surface (work item 1.1, the 5th surface). Embedded subdocs do NOT
  // round-trip through toJSON (live they serialize as {}, after re-apply they
  // vanish), so they are checked STRUCTURALLY: a dedicated doc with embedded
  // subdocs, its V1 update, and the sorted guid set Go must see in GetSubdocs().
  if (FEAT_SUBDOCS) {
    const ddoc = new Y.Doc();
    const dmap = ddoc.getMap('m');
    const guids = [];
    ddoc.transact(() => {
      const n = 1 + ((rng() * 3) | 0);
      for (let i = 0; i < n; i++) {
        const g = 'sub-' + seed + '-' + i;
        dmap.set('s' + i, new Y.Doc({ guid: g }));
        guids.push(g);
      }
    });
    guids.sort();
    rec.subdocUpdateV1 = b64(Y.encodeStateAsUpdate(ddoc));
    rec.subdocGuids = guids;
  }

  return rec;
}

function genConcurrent(seed) {
  const rng = mulberry32(seed);

  const base = new Y.Doc();
  const baseTypes = getTypes(base);
  const baseOps = Math.max(2, (opsPer / 5) | 0);
  base.transact(() => {
    for (let i = 0; i < baseOps; i++) applyRandomOp(rng, baseTypes, 'B');
  });
  const baseV1 = Y.encodeStateAsUpdate(base);
  const baseV2 = Y.encodeStateAsUpdateV2(base);
  const baseSV = Y.encodeStateVector(base);

  const d1 = new Y.Doc();
  Y.applyUpdate(d1, baseV1);
  const d2 = new Y.Doc();
  Y.applyUpdate(d2, baseV1);

  const t1 = getTypes(d1);
  const t2 = getTypes(d2);

  d1.transact(() => { for (let i = 0; i < opsPer; i++) applyRandomOp(rng, t1, '1'); });
  d2.transact(() => { for (let i = 0; i < opsPer; i++) applyRandomOp(rng, t2, '2'); });

  const u1v1 = Y.encodeStateAsUpdate(d1, baseSV);
  const u2v1 = Y.encodeStateAsUpdate(d2, baseSV);
  const u1v2 = Y.encodeStateAsUpdateV2(d1, baseSV);
  const u2v2 = Y.encodeStateAsUpdateV2(d2, baseSV);

  Y.applyUpdate(d1, u2v1);
  Y.applyUpdate(d2, u1v1);
  const s1 = canon(d1.toJSON());
  const s2 = canon(d2.toJSON());
  if (s1 !== s2) {
    return { seed, jsDiverged: true, s1, s2 };
  }

  return {
    seed,
    ops: baseOps + 2 * opsPer,
    baseV1: b64(baseV1), baseV2: b64(baseV2),
    u1v1: b64(u1v1), u2v1: b64(u2v1),
    u1v2: b64(u1v2), u2v2: b64(u2v2),
    full1V2: b64(Y.encodeStateAsUpdateV2(d1)),
    full2V2: b64(Y.encodeStateAsUpdateV2(d2)),
    state: s1,
    // Convergence must hold for the DELTA as well as the flat string: two permuted apply orders
    // can agree on every character while disagreeing on where formatting runs begin and end.
    // Comparing only toJSON() left that class of divergence invisible.
    textDelta: canon(getTypes(d1).text.toDelta()),
  };
}

const gen = mode === 'concurrent' ? genConcurrent : genSingle;
for (let i = 0; i < count; i++) {
  const rec = gen(seedStart + i);
  process.stdout.write(JSON.stringify(rec) + '\n');
}
