import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'

// Relative-position differential (US3, FR-004). This wire format had ZERO differential coverage
// before this feature, and unlike an internal defect a divergence here is a silent interop break
// with real third-party clients — a stored position resolving to the wrong place.
//
// Emits, per seed: the document, a set of relative positions encoded by the REFERENCE, and where
// the reference resolves each one back to. The Go side decodes those bytes, resolves them against
// the same document, and must agree — then re-encodes and must produce the same bytes.
//
// Deliberately includes anchors whose target is later DELETED (a spec edge case): a relative
// position must survive its anchor being tombstoned, which is the whole reason the format exists.

function build(seed) {
  const rng = mulberry32(seed)
  const doc = new Y.Doc({ gc: false })
  doc.clientID = 1
  const txt = doc.getText('t')
  const arr = doc.getArray('a')
  const letters = 'abcdef'

  const ops = []
  for (let i = 0; i < 14; i++) {
    const r = rng()
    if (r < 0.5 || txt.length === 0) {
      const idx = txt.length === 0 ? 0 : (rng() * (txt.length + 1)) | 0
      const ch = letters[(rng() * letters.length) | 0]
      txt.insert(idx, ch); ops.push({ op: 'tinsert', idx, ch })
    } else if (r < 0.7) {
      const idx = (rng() * txt.length) | 0
      txt.delete(idx, 1); ops.push({ op: 'tdelete', idx })
    } else {
      const idx = arr.length === 0 ? 0 : (rng() * (arr.length + 1)) | 0
      const v = (rng() * 100) | 0
      arr.insert(idx, [v]); ops.push({ op: 'ainsert', idx, v })
    }
  }

  // Positions across the whole range, both assoc values. assoc decides which side of the gap the
  // position sticks to, so both must be exercised — they encode differently.
  const positions = []
  for (let idx = 0; idx <= txt.length; idx++) {
    for (const assoc of [0, -1]) {
      const rp = Y.createRelativePositionFromTypeIndex(txt, idx, assoc)
      const abs = Y.createAbsolutePositionFromRelativePosition(rp, doc)
      positions.push({
        target: 't', idx, assoc,
        encoded: hex(Y.encodeRelativePosition(rp)),
        resolvedIndex: abs ? abs.index : null,
        resolvedAssoc: abs ? abs.assoc : null,
      })
    }
  }

  // Now delete some content and re-resolve the SAME encoded positions: the anchor may be
  // tombstoned, and both sides must agree on where the position lands afterwards.
  const afterDelete = []
  if (txt.length > 1) {
    const delIdx = (rng() * (txt.length - 1)) | 0
    txt.delete(delIdx, 1)
    ops.push({ op: 'tdelete', idx: delIdx })
    for (const p of positions) {
      const rp = Y.decodeRelativePosition(Buffer.from(p.encoded, 'hex'))
      const abs = Y.createAbsolutePositionFromRelativePosition(rp, doc)
      afterDelete.push({ encoded: p.encoded, resolvedIndex: abs ? abs.index : null })
    }
  }

  return { seed, ops, state: hex(Y.encodeStateAsUpdate(doc)), positions, afterDelete }
}

const s0 = parseInt(process.argv[2] || '1', 10)
const n = parseInt(process.argv[3] || '400', 10)
let emitted = 0
for (let s = s0; s < s0 + n; s++) { process.stdout.write(JSON.stringify(build(s)) + '\n'); emitted++ }
process.stderr.write(`emitted=${emitted} dropped=0 surface=relpos\n`)
if (emitted === 0) process.exit(1)
