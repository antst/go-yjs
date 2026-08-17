import * as Y from 'yjs'
import * as syncProtocol from 'y-protocols/sync.js'
import * as encoding from 'lib0/encoding'
import * as decoding from 'lib0/decoding'
import { mulberry32, hex } from './harness/index.mjs'

// Sync-protocol differential (US3, FR-004, C-S3). Eight encode/decode functions with ZERO
// differential coverage before this feature. A divergence here is a silent interop break: a Go
// peer and a JS peer would fail to converge, or converge on different content.
//
// The exchange is driven MESSAGE BY MESSAGE rather than compared only at the end, so a divergence
// is located at the message that caused it. Each message the reference produces is emitted as
// bytes; the Go side consumes those exact bytes and must produce byte-identical replies.

function build(seed) {
  const rng = mulberry32(seed)
  const letters = 'abcde'

  // Two peers with DIFFERENT content, so the exchange has real work to do in both directions.
  const A = new Y.Doc({ gc: false }); A.clientID = 1
  const B = new Y.Doc({ gc: false }); B.clientID = 2

  const seed_ops = []
  for (const [doc, tag] of [[A, 'A'], [B, 'B']]) {
    const txt = doc.getText('t'); const arr = doc.getArray('a')
    const n = 3 + ((rng() * 5) | 0)
    for (let i = 0; i < n; i++) {
      if (rng() < 0.6) {
        const idx = txt.length === 0 ? 0 : (rng() * (txt.length + 1)) | 0
        const ch = letters[(rng() * letters.length) | 0]
        txt.insert(idx, ch); seed_ops.push({ doc: tag, op: 'tinsert', idx, ch })
      } else {
        const idx = arr.length === 0 ? 0 : (rng() * (arr.length + 1)) | 0
        const v = (rng() * 100) | 0
        arr.insert(idx, [v]); seed_ops.push({ doc: tag, op: 'ainsert', idx, v })
      }
    }
  }

  // The exchange, as a JS client would drive it.
  const messages = []

  // 1. A -> B : syncStep1 (A's state vector)
  const e1 = encoding.createEncoder()
  syncProtocol.writeSyncStep1(e1, A)
  const step1 = encoding.toUint8Array(e1)
  messages.push({ from: 'A', to: 'B', kind: 'step1', bytes: hex(step1) })

  // 2. B replies : syncStep2 (everything A is missing)
  const e2 = encoding.createEncoder()
  syncProtocol.readSyncMessage(decoding.createDecoder(step1), e2, B, null)
  const step2 = encoding.toUint8Array(e2)
  messages.push({ from: 'B', to: 'A', kind: 'step2', bytes: hex(step2) })

  // 3. A applies it
  const e3 = encoding.createEncoder()
  syncProtocol.readSyncMessage(decoding.createDecoder(step2), e3, A, null)
  messages.push({ from: 'A', to: 'B', kind: 'reply-to-step2', bytes: hex(encoding.toUint8Array(e3)) })

  // 4. B -> A : the reverse direction, so both sides both produce AND consume.
  const e4 = encoding.createEncoder()
  syncProtocol.writeSyncStep1(e4, B)
  const step1b = encoding.toUint8Array(e4)
  messages.push({ from: 'B', to: 'A', kind: 'step1', bytes: hex(step1b) })

  const e5 = encoding.createEncoder()
  syncProtocol.readSyncMessage(decoding.createDecoder(step1b), e5, A, null)
  const step2b = encoding.toUint8Array(e5)
  messages.push({ from: 'A', to: 'B', kind: 'step2', bytes: hex(step2b) })

  const e6 = encoding.createEncoder()
  syncProtocol.readSyncMessage(decoding.createDecoder(step2b), e6, B, null)
  messages.push({ from: 'B', to: 'A', kind: 'reply-to-step2', bytes: hex(encoding.toUint8Array(e6)) })

  return {
    seed,
    ops: seed_ops,
    messages,
    // Both peers must converge, and to the SAME content.
    finalA: hex(Y.encodeStateAsUpdate(A)),
    finalB: hex(Y.encodeStateAsUpdate(B)),
    textA: A.getText('t').toString(),
    textB: B.getText('t').toString(),
  }
}

const s0 = parseInt(process.argv[2] || '1', 10)
const n = parseInt(process.argv[3] || '400', 10)
let emitted = 0
for (let s = s0; s < s0 + n; s++) { process.stdout.write(JSON.stringify(build(s)) + '\n'); emitted++ }
process.stderr.write(`emitted=${emitted} dropped=0 surface=sync\n`)
if (emitted === 0) process.exit(1)
