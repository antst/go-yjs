// Native-op differential generator: builds random Y.Text op streams, applies them
// with yjs's OWN insert/format/delete (NOT replaying an update), and records the
// op stream + resulting state-update hex + toDelta. The Go side replays the SAME
// ops natively and compares — this exercises Go's FormatText/negation/cleanup,
// which the existing fuzz gate (which only replays yjs updates) never touches.
import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'
import { readsText } from './harness/reads.mjs'


const ATTRS = [
  { bold: true }, { bold: false }, { bold: null },
  { italic: true }, { italic: null },
  { color: 'red' }, { color: null },
  { bold: true, italic: true },
]


function genCase(seed, nOps) {
  const rng = mulberry32(seed)
  const doc = new Y.Doc({ gc: false })
  doc.clientID = 1
  const t = doc.getText('t')
  const ops = []
  for (let i = 0; i < nOps; i++) {
    const len = t.length
    const r = rng()
    const noDelete = process.env.ND_NODELETE === '1'
    if (len === 0 || r < 0.4 || (noDelete && r >= 0.8)) {
      // insert (maybe attributed)
      const idx = len === 0 ? 0 : (rng() * (len + 1)) | 0
      const s = 'abcde'[(rng() * 5) | 0]
      const useAttr = rng() < 0.4
      const attr = useAttr ? ATTRS[(rng() * ATTRS.length) | 0] : undefined
      // Record attr as ORDERED [key,value] pairs (Object.entries) so the Go replay
      // inserts format markers in the same order yjs does (its object key order).
      if (useAttr) { t.insert(idx, s, attr); ops.push({ op: 'insert', idx, s, attr: Object.entries(attr) }) }
      else { t.insert(idx, s); ops.push({ op: 'insert', idx, s }) }
    } else if (r < 0.62) {
      // insertEmbed — a public op no generator invoked before (FR-005).
      const idx = len === 0 ? 0 : (rng() * (len + 1)) | 0
      const embed = { type: 'img', w: (rng() * 40) | 0 }
      t.insertEmbed(idx, embed)
      ops.push({ op: 'insertEmbed', idx, embed })
    } else if (r < 0.8) {
      // format
      const idx = (rng() * len) | 0
      const flen = 1 + ((rng() * (len - idx)) | 0)
      const attr = ATTRS[(rng() * ATTRS.length) | 0]
      t.format(idx, flen, attr); ops.push({ op: 'format', idx, len: flen, attr: Object.entries(attr) })
    } else if (r < 0.86) {
      // Type-level attributes. setAttribute / removeAttribute are public Y.Text operations that
      // no generator drove, so their encoding had no differential coverage at all — distinct from
      // format(), which writes ContentFormat markers into the sequence.
      const k = ['x', 'lang'][(rng() * 2) | 0]
      if (rng() < 0.7) {
        const v = ['en', 'de', 1, true][(rng() * 4) | 0]
        t.setAttribute(k, v); ops.push({ op: 'setAttribute', k, v })
      } else {
        t.removeAttribute(k); ops.push({ op: 'removeAttribute', k })
      }
    } else {
      // delete
      const idx = (rng() * len) | 0
      const dlen = 1 + ((rng() * (len - idx)) | 0)
      t.delete(idx, dlen); ops.push({ op: 'delete', idx, len: dlen })
    }
  }
  return {
    seed,
    ops,
    state: hex(Y.encodeStateAsUpdate(doc)),
    // V2 bytes for the SAME document. Until now V2 had only 22 curated fixtures and no
    // differential pressure, so every randomised stream checked V1 only. Emitting both makes
    // the text surface a genuine V1+V2 differential at no extra generation cost.
    stateV2: hex(Y.encodeStateAsUpdateV2(doc)),
    delta: t.toDelta(),
    reads: readsText(t),
  }
}

const seedStart = parseInt(process.argv[2] || '1', 10)
const cases = parseInt(process.argv[3] || '500', 10)
const nOps = parseInt(process.argv[4] || '12', 10)
for (let s = seedStart; s < seedStart + cases; s++) {
  process.stdout.write(JSON.stringify(genCase(s, nOps)) + '\n')
}
