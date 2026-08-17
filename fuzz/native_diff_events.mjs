// Observer EVENT projections, compared against the reference.
//
// The gate's thirteen surfaces compare document state and read projections. What a
// deep observer is HANDED — the path to the changed type, and the per-key action
// map — was compared against nothing, and two defects lived there undisturbed
// through 1.1M seeds: getPathTo counted Items instead of summing Item.length, and
// the keys projection was dead by construction and always returned empty.
//
// Both needed a specific shape to become visible, which is why no incidental test
// caught them either:
//   - the index defect requires COALESCED predecessors, since it only differs from
//     the correct answer when one Item carries several values (ContentAny merges
//     adjacent primitives, so pushing 1,2,3 yields one Item of length 3)
//   - the path defect requires depth >= 2, since a one-level path is flat either way
// The generator below produces both on purpose rather than by luck.
import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'

// Both implementations must locate the same nested container, so the descent is a
// deterministic scan rather than a random pick.
function firstNestedArray(root) {
  for (let i = 0; i < root.length; i++) {
    const v = root.get(i)
    if (v instanceof Y.Array) return v
  }
  return null
}
function firstNestedMap(arr) {
  for (let i = 0; i < arr.length; i++) {
    const v = arr.get(i)
    if (v instanceof Y.Map) return v
  }
  return null
}

// A canonical event record: the path a deep observer receives, plus the key
// projection. Sorted so both sides emit byte-identical text.
function recordEvents(events) {
  return events.map((e) => {
    const keys = []
    e.keys.forEach((v, k) => keys.push([k, v.action, v.oldValue === undefined ? null : v.oldValue]))
    keys.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
    return { path: e.path, keys }
  })
}

function gen(seed, nOps) {
  const rng = mulberry32(seed)
  const doc = new Y.Doc({ gc: false })
  doc.clientID = 1
  const root = doc.getArray('a')

  const batches = []
  root.observeDeep((events) => batches.push(recordEvents(events)))

  const ops = []
  for (let i = 0; i < nOps; i++) {
    const r = rng()
    if (r < 0.35 || root.length === 0) {
      // Adjacent primitives coalesce into one ContentAny Item: this is what makes
      // an index computed from Item count diverge from one summing Item.length.
      const n = (rng() * 100) | 0
      root.push([n])
      ops.push({ op: 'pushnum', n })
    } else if (r < 0.5) {
      root.push([new Y.Array()])
      ops.push({ op: 'pusharr' })
    } else if (r < 0.65) {
      const inner = firstNestedArray(root)
      if (inner) {
        inner.push([new Y.Map()])
        ops.push({ op: 'nestmap' })
      } else {
        const n = (rng() * 100) | 0
        root.push([n])
        ops.push({ op: 'pushnum', n })
      }
    } else {
      const inner = firstNestedArray(root)
      const m = inner && firstNestedMap(inner)
      const key = 'abc'[(rng() * 3) | 0]
      if (m) {
        if (r < 0.9) {
          const v = (rng() * 50) | 0
          m.set(key, v)
          ops.push({ op: 'mapset', key, v })
        } else {
          m.delete(key)
          ops.push({ op: 'mapdel', key })
        }
      } else {
        const n = (rng() * 100) | 0
        root.push([n])
        ops.push({ op: 'pushnum', n })
      }
    }
  }

  return {
    seed,
    ops,
    state: hex(Y.encodeStateAsUpdate(doc)),
    events: JSON.stringify(batches),
  }
}

const s0 = parseInt(process.argv[2] || '1')
const n = parseInt(process.argv[3] || '1000')
const o = parseInt(process.argv[4] || '20')
for (let s = s0; s < s0 + n; s++) process.stdout.write(JSON.stringify(gen(s, o)) + '\n')
