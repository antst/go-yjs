// Direction B verifier (US2, FR-002).
//
// Direction A has the reference generate and this library replay, so this library's CONSTRUCTORS
// never run under the oracle — only its apply path. That is exactly where an encoding
// nondeterminism defect hid in feature 003 (an identically-built document produced four different
// encodings across forty runs).
//
// Here the library generates and encodes; this script has the reference decode and re-encode, and
// prints the reference's canonical bytes. The Go side compares. A mismatch means the library
// produced a non-canonical encoding — something direction A cannot detect, because in direction A
// the bytes came from the reference to begin with.
import * as Y from 'yjs'
import { hex } from './harness/index.mjs'
import pkg from './canonical.js'
const { canon } = pkg
import { createInterface } from 'node:readline'

const rl = createInterface({ input: process.stdin })
let n = 0

for await (const line of rl) {
  if (!line.trim()) continue
  const rec = JSON.parse(line)
  const out = { seed: rec.seed }

  try {
    const doc = new Y.Doc({ gc: false })
    Y.applyUpdateV2(doc, Buffer.from(rec.updateV2, 'hex'))
    // Re-encode with the reference. If the library's bytes are canonical, this round-trips
    // identically; if not, the difference is the library's non-canonicality.
    out.reencodedV2 = hex(Y.encodeStateAsUpdateV2(doc))
    out.reencodedV1 = hex(Y.encodeStateAsUpdate(doc))
    // Semantic check too: re-encodability alone would not catch a document the reference decodes
    // into something structurally different but coincidentally re-encodes the same.
    //
    // The types MUST be instantiated first: yjs's toJSON() includes only types that have been
    // ACCESSED, so on a freshly-decoded doc it returns {} and every seed would "mismatch" —
    // a property of the reference API, not of this library.
    //
    // canon() rather than JSON.stringify: Go map iteration and JS insertion order give different
    // key orders for the same logical content, so a raw string compare would report differences
    // that are not divergences. canon() sorts keys on both sides.
    const shape = {
      t: doc.getText('t').toString(),
      a: doc.getArray('a').toJSON(),
      m: doc.getMap('m').toJSON(),
    }
    out.canon = canon(shape)

    // T031 — direction B for SNAPSHOTS. The library encoded this snapshot; the reference must
    // decode it, re-encode it to the same bytes, and — crucially — be able to USE it. Byte
    // re-encodability alone would pass a snapshot that decodes into something the reference
    // cannot restore from, which is the failure that actually matters to a consumer.
    if (rec.snapshotV2) {
      const snap = Y.decodeSnapshotV2(Buffer.from(rec.snapshotV2, 'hex'))
      out.snapReencodedV2 = hex(Y.encodeSnapshotV2(snap))
      const restored = Y.createDocFromSnapshot(doc, snap)
      // Instantiate before toJSON for the same reason as above: yjs serializes only ACCESSED
      // types, so a freshly restored doc would otherwise canonicalize to {}.
      out.snapCanon = canon({
        t: restored.getText('t').toString(),
        a: restored.getArray('a').toJSON(),
        m: restored.getMap('m').toJSON(),
      })
    }

    // T032 — direction B for GC. A gc-ENABLED document built and encoded by this library, applied
    // and re-encoded by the reference. Direction A cannot reach this: there, garbage collection
    // has already happened on the reference side before Go ever sees the bytes, so Go's own GC
    // decisions — which items become GC structs, and how they encode — are never checked against
    // the reference at all.
    if (rec.gcUpdateV2) {
      const gdoc = new Y.Doc({ gc: true })
      Y.applyUpdateV2(gdoc, Buffer.from(rec.gcUpdateV2, 'hex'))
      out.gcReencodedV2 = hex(Y.encodeStateAsUpdateV2(gdoc))
      out.gcCanon = canon({
        t: gdoc.getText('t').toString(),
        a: gdoc.getArray('a').toJSON(),
        m: gdoc.getMap('m').toJSON(),
      })
    }
  } catch (e) {
    out.error = String(e && e.message ? e.message : e)
  }

  process.stdout.write(JSON.stringify(out) + '\n')
  n++
}

process.stderr.write(`verified=${n} surface=dirB\n`)
if (n === 0) {
  process.stderr.write('ERROR: direction-B verifier received 0 records — it compared NOTHING\n')
  process.exit(1)
}
