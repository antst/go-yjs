import * as Y from 'yjs'
import { Awareness, encodeAwarenessUpdate, applyAwarenessUpdate, removeAwarenessStates } from 'y-protocols/awareness.js'
import { mulberry32, hex } from './harness/index.mjs'

// Awareness differential (US3, FR-004, C-S4). One encode/decode function with ZERO differential
// coverage before this feature, on the presence protocol every collaborative UI depends on.
//
// FR-004 requires comparing the EMITTED CHANGE/UPDATE PAYLOADS, not merely the resulting presence
// map: the events are the contract a consumer reacts to, and a peer that ends with the right map
// while emitting the wrong added/updated/removed sets still drives the wrong UI.

function build(seed) {
  const rng = mulberry32(seed)
  const doc = new Y.Doc(); doc.clientID = 1
  const aw = new Awareness(doc)
  // The reference constructor starts a setInterval; stop it so the corpus is deterministic and the
  // process exits. Reaping/renewal are the managed type's concern (US6), not this differential's.
  clearInterval(aw._checkInterval)

  const events = []
  aw.on('update', ({ added, updated, removed }, origin) => {
    events.push({ ev: 'update', added: [...added].sort((a, b) => a - b),
                  updated: [...updated].sort((a, b) => a - b),
                  removed: [...removed].sort((a, b) => a - b) })
  })

  const ops = []
  const fields = ['name', 'color', 'cursor']
  const updates = []

  for (let i = 0; i < 10; i++) {
    const r = rng()
    if (r < 0.55) {
      const f = fields[(rng() * fields.length) | 0]
      const v = (rng() * 1000) | 0
      const st = aw.getLocalState() || {}
      st[f] = v
      aw.setLocalState({ ...st })
      ops.push({ op: 'setLocal', f, v })
    } else if (r < 0.75) {
      aw.setLocalState(null)
      ops.push({ op: 'clearLocal' })
    } else {
      // A remote client appears/updates, applied through the wire format.
      const other = 100 + ((rng() * 3) | 0)
      const remoteDoc = new Y.Doc(); remoteDoc.clientID = other
      const remote = new Awareness(remoteDoc)
      clearInterval(remote._checkInterval)
      remote.setLocalState({ name: (rng() * 500) | 0 })
      const upd = encodeAwarenessUpdate(remote, [other])
      applyAwarenessUpdate(aw, upd, 'remote')
      ops.push({ op: 'applyRemote', client: other, update: hex(upd) })
    }
    // The encoded state after each op: the wire format itself, compared byte for byte.
    const clients = [...aw.getStates().keys()].sort((a, b) => a - b)
    updates.push(clients.length ? hex(encodeAwarenessUpdate(aw, clients)) : '')
  }

  const clients = [...aw.getStates().keys()].sort((a, b) => a - b)
  return {
    seed, ops, events, updates,
    finalClients: clients,
    finalStates: JSON.stringify(clients.map(c => [c, aw.getStates().get(c)])),
  }
}

const s0 = parseInt(process.argv[2] || '1', 10)
const n = parseInt(process.argv[3] || '400', 10)
let emitted = 0
for (let s = s0; s < s0 + n; s++) { process.stdout.write(JSON.stringify(build(s)) + '\n'); emitted++ }
process.stderr.write(`emitted=${emitted} dropped=0 surface=awareness\n`)
if (emitted === 0) process.exit(1)
