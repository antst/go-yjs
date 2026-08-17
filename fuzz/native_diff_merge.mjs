// Merged-update BYTES, compared against the reference.
//
// mergeUpdates is a public API whose output is wire bytes other peers consume,
// and its byte layout is decided by a scheduler: which reader is drained next,
// how ties are broken, where Skips land. Nothing compared those bytes. The gate's
// thirteen surfaces cover documents and read projections; direction B catches a
// non-canonical DOCUMENT encoding, but a non-canonical MERGE was invisible.
//
// Measured, not assumed: inverting the scheduler's client ordering (yjs writes
// higher clients first) leaves a 20,000-case both-direction gate run fully green.
// So this surface exists because the existing ones demonstrably cannot see it.
//
// The inputs are emitted as hex and replayed verbatim rather than regenerated, so
// both implementations merge byte-identical updates. Any difference in the output
// is then the merge itself, never a difference in what was merged.
import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'

// One update per client. Overlap is deliberate: merging disjoint updates would
// never exercise the tie and requeue paths, which is where the ordering lives.
function buildUpdates(seed, nDocs, nOps) {
  const rng = mulberry32(seed)
  const updatesV1 = []
  const updatesV2 = []
  const shared = new Y.Doc({ gc: false })
  shared.clientID = 1
  shared.getArray('a').push([0, 1, 2])
  const base = Y.encodeStateAsUpdate(shared)

  for (let d = 0; d < nDocs; d++) {
    const doc = new Y.Doc({ gc: false })
    // Distinct clients, but every doc starts from the same base so their struct
    // ranges overlap and the scheduler has to interleave rather than concatenate.
    doc.clientID = 2 + d
    Y.applyUpdate(doc, base)
    const arr = doc.getArray('a')
    const txt = doc.getText('t')
    for (let i = 0; i < nOps; i++) {
      const r = rng()
      if (r < 0.4) arr.push([(rng() * 100) | 0])
      else if (r < 0.6) arr.insert(arr.length ? (rng() * arr.length) | 0 : 0, [(rng() * 100) | 0])
      else if (r < 0.8) txt.insert(txt.length ? (rng() * txt.length) | 0 : 0, 'abcde'[(rng() * 5) | 0])
      else if (arr.length > 1) arr.delete((rng() * (arr.length - 1)) | 0, 1)
    }
    updatesV1.push(Y.encodeStateAsUpdate(doc))
    updatesV2.push(Y.encodeStateAsUpdateV2(doc))
  }
  return { updatesV1, updatesV2 }
}

function gen(seed, nDocs, nOps) {
  const { updatesV1, updatesV2 } = buildUpdates(seed, nDocs, nOps)
  return {
    seed,
    inputsV1: updatesV1.map(hex),
    inputsV2: updatesV2.map(hex),
    mergedV1: hex(Y.mergeUpdates(updatesV1)),
    mergedV2: hex(Y.mergeUpdatesV2(updatesV2)),
  }
}

const s0 = parseInt(process.argv[2] || '1')
const n = parseInt(process.argv[3] || '400')
const o = parseInt(process.argv[4] || '8')
for (let s = s0; s < s0 + n; s++) {
  // 2..5 updates: enough readers for ties and requeues without making the corpus huge.
  const nDocs = 2 + (s % 4)
  process.stdout.write(JSON.stringify(gen(s, nDocs, o)) + '\n')
}
