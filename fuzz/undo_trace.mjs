// Per-op trace of the reference side for one undo seed, so a divergence can be located at the
// exact operation rather than inferred from the final state. Mirrors undo_gen.mjs exactly.
import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'

const seed = parseInt(process.argv[2], 10)
const nOps = parseInt(process.argv[3] || '20', 10)

const rng = mulberry32(seed)
const doc = new Y.Doc({ gc: false }); doc.clientID = 1
const txt = doc.getText('t'); const arr = doc.getArray('a')
const remote = new Y.Doc({ gc: false }); remote.clientID = 2
const rtxt = remote.getText('t')
const um = new Y.UndoManager([txt, arr], { captureTimeout: 100000 })
const letters = 'abcde'
const trace = []

for (let i = 0; i < nOps; i++) {
  const r = rng()
  let op
  if (r < 0.34) {
    const idx = txt.length === 0 ? 0 : (rng() * (txt.length + 1)) | 0
    const ch = letters[(rng() * letters.length) | 0]
    txt.insert(idx, ch); op = { op: 'tinsert', idx, ch }
  } else if (r < 0.46 && txt.length > 0) {
    const idx = (rng() * txt.length) | 0
    txt.delete(idx, 1); op = { op: 'tdelete', idx }
  } else if (r < 0.58) {
    const idx = arr.length === 0 ? 0 : (rng() * (arr.length + 1)) | 0
    const v = (rng() * 100) | 0
    arr.insert(idx, [v]); op = { op: 'ainsert', idx, v }
  } else if (r < 0.66 && arr.length > 0) {
    const idx = (rng() * arr.length) | 0
    arr.delete(idx, 1); op = { op: 'adelete', idx }
  } else if (r < 0.74) {
    const ch = letters[(rng() * letters.length) | 0]
    rtxt.insert(0, ch)
    Y.applyUpdate(doc, Y.encodeStateAsUpdate(remote))
    Y.applyUpdate(remote, Y.encodeStateAsUpdate(doc))
    op = { op: 'remote', ch }
  } else if (r < 0.82) {
    um.stopCapturing(); op = { op: 'stopCapturing' }
  } else if (r < 0.92) {
    um.undo(); op = { op: 'undo' }
  } else {
    um.redo(); op = { op: 'redo' }
  }
  trace.push({ i, ...op, text: txt.toString(), arr: JSON.stringify(arr.toArray()),
               state: hex(Y.encodeStateAsUpdate(doc)),
               undoLen: um.undoStack.length, redoLen: um.redoStack.length })
}
process.stdout.write(JSON.stringify({ seed, trace }) + '\n')
