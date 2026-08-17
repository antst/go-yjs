// Read-path sweeps, one per surface.
//
// The op streams the generators emit drive only the MUTATING half of each type's API. Measured,
// that is 18 of 50 derived operations (36%) — every read and query operation was reachable only by
// unit tests, and two of the defects this feature found (a text rendering that dropped children of
// unexpected kinds, a delta that omitted its attribute-presence flag) lived exactly there.
//
// So each case additionally reports what every READ operation returns, and the Go side computes the
// same bundle and compares. Reads cannot be driven as ops — they change nothing — but they can be
// compared as observations, which is what closes the gap.
//
// Both sides must produce byte-identical canonical output, so: values are projected through toJSON
// (a nested type is a live object with parent back-pointers that no canonicaliser can walk), key
// order is sorted by canon() on both sides, and probe keys/queries are FIXED rather than random so
// the two implementations ask the same questions.
import pkg from '../canonical.js'
const { canon } = pkg

const proj = (v) => (v && typeof v.toJSON === 'function' ? v.toJSON() : v === undefined ? null : v)

// Fixed probe sets. Random probes would ask the two implementations different questions.
export const MAP_PROBE_KEYS = ['a', 'b', 'c', 'd', 'e', 'missing']
export const XML_PROBE_TAGS = ['div', 'span', 'p', 'nosuchtag']

/** Count non-deleted items (not elements) by walking the item list, mirroring Go's Range. */
function itemCount(type) {
  let n = 0
  for (let it = type._start; it !== null; it = it.right) if (!it.deleted) n++
  return n
}

export function readsArray(a) {
  const len = a.length
  const gets = []
  for (let i = 0; i < Math.min(3, len); i++) gets.push(proj(a.get(i)))
  let forEachCount = 0
  a.forEach(() => { forEachCount++ })
  return canon({
    len,
    toJSON: a.toJSON(),
    toArray: a.toArray().map(proj),
    gets,
    mapIdx: a.map((_v, i) => i),
    forEachCount,
    itemCount: itemCount(a),
  })
}

export function readsMap(m) {
  const has = {}
  const gets = {}
  for (const k of MAP_PROBE_KEYS) {
    has[k] = m.has(k)
    gets[k] = m.has(k) ? proj(m.get(k)) : null
  }
  const entries = {}
  for (const [k, v] of m.entries()) entries[k] = proj(v)
  let forEachCount = 0
  m.forEach(() => { forEachCount++ })
  const keys = [...m.keys()].sort()
  return canon({
    size: m.size,
    toJSON: m.toJSON(),
    keys,
    // Yjs has no append-to-buffer variant. The reference result is the semantic
    // equivalent that Go's AppendKeys must match without exposing cached storage.
    appendKeys: [...keys],
    values: keys.map((k) => proj(m.get(k))),
    entries,
    has,
    gets,
    forEachCount,
  })
}

export function readsXml(x) {
  const len = x.length
  const qs = {}
  const qsa = {}
  for (const tag of XML_PROBE_TAGS) {
    const one = x.querySelector(tag)
    qs[tag] = one ? one.toString() : null
    qsa[tag] = x.querySelectorAll(tag).length
  }
  // The tree walker visits every node; count them so a walker that silently returns nothing
  // (which is what the Go stub used to do) cannot pass.
  let walked = 0
  for (const _n of x.createTreeWalker(() => true)) walked++
  const first = x.firstChild
  return canon({
    len,
    toString: x.toString(),
    toJSON: x.toJSON(),
    toArray: x.toArray().map((n) => n.toString()),
    slice: x.slice(0, Math.min(2, len)).map((n) => n.toString()),
    get0: len > 0 ? x.get(0).toString() : null,
    firstChild: first ? first.toString() : null,
    querySelector: qs,
    querySelectorAllCount: qsa,
    treeWalkerCount: walked,
  })
}

export function readsText(t) {
  const attrs = t.getAttributes()
  const probes = {}
  for (const k of ['x', 'lang', 'missing']) {
    const v = t.getAttribute(k)
    probes[k] = v === undefined ? null : v
  }
  return canon({
    toString: t.toString(),
    toJSON: t.toJSON(),
    toDelta: t.toDelta(),
    attributes: attrs === undefined ? {} : attrs,
    getAttribute: probes,
  })
}
