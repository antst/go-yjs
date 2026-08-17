import * as Y from 'yjs'
import { mulberry32, hex } from './harness/index.mjs'

// Undo/redo differential generator (US1). Undo had ZERO differential coverage before this feature,
// yet four defects were found in it by hand — a phantom undo entry, a lost redo, tombstone
// resurrection on redo, and a scope-argument crash. None was reachable by a green oracle at any
// seed count, because no generator emitted an undo op.
//
// Two properties are deliberate:
//
//  - A capture window (captureTimeout) that COALESCES several edits into one undo step. Without
//    coalescing, undo reverts only the last op and ContentType.copy() is never reached — which is
//    how tombstone resurrection survived feature 003.
//  - MULTI-PARTICIPANT streams. Restoration order is only observable when more than one client has
//    deleted content, and that order is exactly what FR-001a is about.
//
// Emitted per case: the op stream, the final encoded state, and the observable stack status
// (canUndo/canRedo). The stack status matters because a phantom undo entry and a lost redo alter
// NO encoded bytes — a state-only comparison cannot reach either.

function build(seed, nOps) {
  const rng = mulberry32(seed)
  const doc = new Y.Doc({ gc: false })
  doc.clientID = 1
  const txt = doc.getText('t')
  const arr = doc.getArray('a')

  // A second client, so deletions come from more than one participant and restoration order
  // becomes observable.
  const remote = new Y.Doc({ gc: false })
  remote.clientID = 2
  const rtxt = remote.getText('t')

  // captureTimeout large enough that consecutive edits coalesce into ONE stack item.
  const um = new Y.UndoManager([txt, arr], { captureTimeout: 100000 })

  const ops = []
  const letters = 'abcde'

  for (let i = 0; i < nOps; i++) {
    const r = rng()
    if (r < 0.34) {
      const idx = txt.length === 0 ? 0 : (rng() * (txt.length + 1)) | 0
      const ch = letters[(rng() * letters.length) | 0]
      txt.insert(idx, ch)
      ops.push({ op: 'tinsert', idx, ch })
    } else if (r < 0.46 && txt.length > 0) {
      const idx = (rng() * txt.length) | 0
      txt.delete(idx, 1)
      ops.push({ op: 'tdelete', idx })
    } else if (r < 0.58) {
      const idx = arr.length === 0 ? 0 : (rng() * (arr.length + 1)) | 0
      const v = (rng() * 100) | 0
      arr.insert(idx, [v])
      ops.push({ op: 'ainsert', idx, v })
    } else if (r < 0.66 && arr.length > 0) {
      const idx = (rng() * arr.length) | 0
      arr.delete(idx, 1)
      ops.push({ op: 'adelete', idx })
    } else if (r < 0.74) {
      // Remote edit merged in: creates content owned by client 2, so a later undo has deletions
      // from two participants and the restoration ORDER becomes observable (FR-001a).
      const ch = letters[(rng() * letters.length) | 0]
      rtxt.insert(0, ch)
      Y.applyUpdate(doc, Y.encodeStateAsUpdate(remote))
      Y.applyUpdate(remote, Y.encodeStateAsUpdate(doc))
      ops.push({ op: 'remote', ch })
    } else if (r < 0.82) {
      um.stopCapturing()
      ops.push({ op: 'stopCapturing' })
    } else if (r < 0.92) {
      um.undo()
      ops.push({ op: 'undo' })
    } else {
      um.redo()
      ops.push({ op: 'redo' })
    }
  }

  return {
    seed,
    ops,
    state: hex(Y.encodeStateAsUpdate(doc)),
    // Observable stack status — invisible to a bytes-only comparison.
    canUndo: um.canUndo(),
    canRedo: um.canRedo(),
    undoLen: um.undoStack.length,
    redoLen: um.redoStack.length,
  }
}

const s0 = parseInt(process.argv[2] || '1', 10)
const n = parseInt(process.argv[3] || '500', 10)
const o = parseInt(process.argv[4] || '20', 10)
let emitted = 0
for (let s = s0; s < s0 + n; s++) {
  process.stdout.write(JSON.stringify(build(s, o)) + '\n')
  emitted++
}
process.stderr.write(`emitted=${emitted} dropped=0 surface=undo\n`)
if (emitted === 0) process.exit(1)
