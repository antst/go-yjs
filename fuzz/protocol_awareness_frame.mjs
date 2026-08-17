import * as Y from 'yjs'
import * as encoding from 'lib0/encoding.js'
import * as decoding from 'lib0/decoding.js'
import {
  Awareness,
  applyAwarenessUpdate,
  encodeAwarenessUpdate
} from 'y-protocols/awareness.js'
import * as syncProtocol from 'y-protocols/sync.js'

const fromHex = hex => Uint8Array.from(Buffer.from(hex, 'hex'))
const toHex = bytes => Buffer.from(bytes).toString('hex')

const mode = process.argv[2]

if (mode === 'decode') {
  const frame = fromHex(process.argv[3] || '')
  const decoder = decoding.createDecoder(frame)
  const messageType = decoding.readVarUint(decoder)
  if (messageType !== 1) {
    throw new Error(`message type ${messageType}, want awareness type 1`)
  }
  const body = decoding.readVarUint8Array(decoder)
  if (decoding.hasContent(decoder)) {
    throw new Error('awareness frame has trailing bytes after its length-prefixed body')
  }

  // Apply through the real y-protocols decoder. Reading the body alone would
  // only prove lib0 framing; applying it proves the extracted bytes are a real
  // awareness update rather than a coincidentally well-sized byte sequence.
  const bodyDecoder = decoding.createDecoder(body)
  const count = decoding.readVarUint(bodyDecoder)
  if (count !== 1) {
    throw new Error(`awareness update contains ${count} clients, want 1`)
  }
  const clientID = decoding.readVarUint(bodyDecoder)

  const doc = new Y.Doc()
  const awareness = new Awareness(doc)
  applyAwarenessUpdate(awareness, body, 'go-frame-differential')
  const state = awareness.getStates().get(clientID)
  if (state === undefined) {
    throw new Error(`y-protocols did not apply client ${clientID}`)
  }
  process.stdout.write(JSON.stringify({
    messageType,
    bodyHex: toHex(body),
    clientID,
    state
  }))
  awareness.destroy()
  doc.destroy()
} else if (mode === 'encode') {
  const doc = new Y.Doc()
  // A stable small id keeps the reverse fixture byte-stable and representable
  // on every Go architecture. Y.Doc.clientID is the source Awareness captures.
  doc.clientID = 424242
  const awareness = new Awareness(doc)
  const state = { name: 'js-peer', cursor: { anchor: 3, head: 5 } }
  awareness.setLocalState(state)
  const body = encodeAwarenessUpdate(awareness, [awareness.clientID])

  // This is the canonical y-websocket awareness envelope: the outer message
  // type followed by a lib0 length-prefixed awareness-update byte array.
  const encoder = encoding.createEncoder()
  encoding.writeVarUint(encoder, 1)
  encoding.writeVarUint8Array(encoder, body)
  const frame = encoding.toUint8Array(encoder)
  process.stdout.write(JSON.stringify({
    messageType: 1,
    frameHex: toHex(frame),
    bodyHex: toHex(body),
    clientID: awareness.clientID,
    state
  }))
  awareness.destroy()
  doc.destroy()
} else if (mode === 'decode-sync') {
  const frame = fromHex(process.argv[3] || '')
  const decoder = decoding.createDecoder(frame)
  const messageType = decoding.readVarUint(decoder)
  if (messageType !== 0) {
    throw new Error(`message type ${messageType}, want sync type 0`)
  }
  const doc = new Y.Doc({ gc: false })
  const reply = encoding.createEncoder()
  const syncType = syncProtocol.readSyncMessage(decoder, reply, doc, 'go-frame-audit')
  if (decoding.hasContent(decoder)) {
    throw new Error(`sync sub-message ${syncType} left trailing bytes`)
  }
  process.stdout.write(JSON.stringify({ messageType, syncType }))
  doc.destroy()
} else {
  throw new Error(`usage: node protocol_awareness_frame.mjs decode <frameHex> | encode | decode-sync <frameHex>`)
}
