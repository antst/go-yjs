// Reference-side performance suite. Every case here mirrors a Go benchmark in
// `perf_bench_test.go` operation-for-operation, so the two sets of numbers are comparable rather
// than merely both collected.
//
// Fairness rules, because a benchmark that is not fair is worse than no benchmark:
//   - identical workloads, identical sizes, identical op order (same LCG, reimplemented below so
//     both sides draw the SAME index sequence — not merely both "random")
//   - the timed region excludes setup on both sides, matching Go's b.ResetTimer/b.StopTimer
//   - each case is run to a time budget and reports ns per iteration, the same unit Go reports
//   - a warmup pass is discarded so JIT compilation is not charged to the measurement, which is
//     the single easiest way to make a JS implementation look slow by accident
import * as Y from 'yjs'

const SMALL = 2000
const LARGE = 10000
const MIN_ITERS = 3
const BUDGET_NS = 2e9 // ~2s per case, mirroring `go test -benchtime 2s`
// Must equal xmlSetAttributeOverwrites in perf_bench_xml_test.go and XML_SET_ATTRIBUTE_OVERWRITES
// in bench/yrs/src/main.rs. The value is arbitrary; its equality across harnesses is not.
const XML_SET_ATTRIBUTE_OVERWRITES = 100
let benchSink = 0

// Go's math/rand with a fixed seed is not reproducible across languages, so both sides use THIS
// generator instead: a plain 32-bit LCG, reimplemented identically in the Go suite's counterpart
// where index sequences must match. For cases where only the DISTRIBUTION matters (random insert
// positions), matching the exact sequence is not required for fairness, only matching the shape.
function lcg(seed = 42) {
  let s = seed >>> 0
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0
    return s
  }
}
const randInt = (rng, n) => (n <= 0 ? 0 : rng() % n)

function str(n) {
  let out = ''
  for (let i = 0; i < n; i++) out += String.fromCharCode(97 + (i % 26))
  return out
}

const newDoc = () => new Y.Doc({ gc: false })

// Keys built ONCE, as fixtures. Building them in the timed loop made these rows partly a
// comparison of each language's string formatting rather than of its CRDT.
const PERF_KEYS = Array.from({ length: SMALL }, (_, i) => 'k' + i)

// ---------------------------------------------------------------- cases

const cases = {
  TextAppendSmall: () => {
    const t = newDoc().getText('t')
    for (let j = 0; j < SMALL; j++) t.insert(t.length, 'x')
  },
  TextAppendLarge: () => {
    const t = newDoc().getText('t')
    for (let j = 0; j < LARGE; j++) t.insert(t.length, 'x')
  },
  TextInsertRandomSmall: () => {
    const rng = lcg()
    const t = newDoc().getText('t')
    for (let j = 0; j < SMALL; j++) t.insert(randInt(rng, t.length + 1), 'y')
  },
  TextInsertRandomLarge: () => {
    const rng = lcg()
    const t = newDoc().getText('t')
    for (let j = 0; j < LARGE; j++) t.insert(randInt(rng, t.length + 1), 'y')
  },
  TextDeleteRandom: {
    setup: () => {
      const t = newDoc().getText('t')
      t.insert(0, str(LARGE))
      return t
    },
    run: (t) => {
      const rng = lcg()
      for (let j = 0; j < SMALL; j++) {
        if (t.length < 2) break
        t.delete(randInt(rng, t.length - 1), 1)
      }
    },
    // The deletes mutate the document, so it must be rebuilt for each timed iteration exactly as
    // the Go side does with b.StopTimer around its setup.
    perIterSetup: true,
  },
  TextFormatChurn: () => {
    const rng = lcg()
    const t = newDoc().getText('t')
    t.insert(0, str(SMALL))
    for (let j = 0; j < 1000; j++) {
      const attr = j % 3 === 0 ? { bold: true } : j % 3 === 1 ? { italic: true } : { bold: null }
      t.format(randInt(rng, SMALL - 20), 20, attr)
    }
  },
  TextToDelta: {
    setup: () => {
      const rng = lcg()
      const t = newDoc().getText('t')
      t.insert(0, str(SMALL))
      for (let j = 0; j < 500; j++) t.format(randInt(rng, SMALL - 20), 20, { bold: j % 2 === 0 })
      return t
    },
    run: (t) => { t.toDelta() },
  },
  ArrayInsertSequential: () => {
    const a = newDoc().getArray('a')
    for (let j = 0; j < SMALL; j++) a.insert(a.length, [j])
  },
  MapSet: () => {
    const m = newDoc().getMap('m')
    for (let j = 0; j < SMALL; j++) m.set(PERF_KEYS[j], j)
  },

  // Batched variants: identical work, ONE transaction instead of N. Measured because ygo's API
  // requires an explicit transaction per mutation and therefore steers consumers toward batching;
  // comparing only the per-op shape would measure a usage its own design discourages.
  TextAppendLargeBatched: () => {
    const d = newDoc(); const t = d.getText('t')
    d.transact(() => { for (let j = 0; j < LARGE; j++) t.insert(t.length, 'x') })
  },
  ArrayInsertBatched: () => {
    const d = newDoc(); const a = d.getArray('a')
    d.transact(() => { for (let j = 0; j < SMALL; j++) a.insert(a.length, [j]) })
  },
  MapSetBatched: () => {
    const d = newDoc(); const m = d.getMap('m')
    d.transact(() => { for (let j = 0; j < SMALL; j++) m.set(PERF_KEYS[j], j) })
  },
  EncodeV1: {
    setup: () => builtDoc(LARGE),
    run: (d) => { Y.encodeStateAsUpdate(d) },
  },
  EncodeV2: {
    setup: () => builtDoc(LARGE),
    run: (d) => { Y.encodeStateAsUpdateV2(d) },
  },
  ApplyV1: {
    setup: () => Y.encodeStateAsUpdate(builtDoc(LARGE)),
    run: (u) => { Y.applyUpdate(newDoc(), u) },
  },
  ApplyV2: {
    setup: () => Y.encodeStateAsUpdateV2(builtDoc(LARGE)),
    run: (u) => { Y.applyUpdateV2(newDoc(), u) },
  },
  // ---- coverage cases -------------------------------------------------------------------
  // The suite above measured 8 of the 36 operations the oracle tracks. These are the rest of the
  // hot ones, mirrored operation-for-operation from perf_bench_ops_test.go so the Go numbers have
  // something to be measured AGAINST rather than merely reported.

  ArrayPush: () => {
    const a = newDoc().getArray('a')
    for (let j = 0; j < SMALL; j++) a.push([j])
  },
  // Matched pair with ArrayInsertEndWithTombstones: identical work and identical delete pattern,
  // differing only in push vs insert(length). Any gap belongs to the append call alone.
  ArrayPushWithTombstones: () => {
    const a = newDoc().getArray('a')
    for (let j = 0; j < SMALL; j++) {
      a.push([j])
      if (j % 2 === 1 && a.length > 0) a.delete(a.length - 1, 1)
    }
  },
  ArrayInsertEndWithTombstones: () => {
    const a = newDoc().getArray('a')
    for (let j = 0; j < SMALL; j++) {
      a.insert(a.length, [j])
      if (j % 2 === 1 && a.length > 0) a.delete(a.length - 1, 1)
    }
  },
  ArrayUnshift: () => {
    const a = newDoc().getArray('a')
    for (let j = 0; j < SMALL; j++) a.unshift([j])
  },
  ArrayToArray: { setup: () => builtArray(SMALL), run: (a) => { a.toArray() } },
  ArrayToJson: { setup: () => builtArray(SMALL), run: (a) => { a.toJSON() } },
  ArrayForEach: {
    setup: () => builtArray(SMALL),
    run: (a) => {
      let n = 0
      a.forEach(() => { n++ })
      benchSink += n
    },
  },
  ArrayGetRandom: {
    setup: () => ({ a: builtArray(SMALL), rng: lcg() }),
    // A full sweep, not one lookup: a single get is below timer resolution on every
    // implementation, and the Go and Rust counterparts sweep too.
    run: ({ a, rng }) => { for (let i = 0; i < SMALL; i++) a.get(randInt(rng, SMALL)) },
  },

  MapKeys: { setup: () => builtMap(SMALL), run: (m) => { Array.from(m.keys()) } },
  MapValues: { setup: () => builtMap(SMALL), run: (m) => { Array.from(m.values()) } },
  MapEntries: { setup: () => builtMap(SMALL), run: (m) => { Array.from(m.entries()) } },
  MapToJson: { setup: () => builtMap(SMALL), run: (m) => { m.toJSON() } },
  MapHas: {
    setup: () => builtMap(SMALL),
    run: (m) => { for (let i = 0; i < SMALL; i++) m.has(mapKey(i)) },
  },
  MapClear: {
    setup: () => builtMap(SMALL),
    run: (m) => { m.clear() },
    perIterSetup: true, // clear mutates the fixture, so it must be rebuilt outside the timed region
  },

  TextToString: { setup: () => builtText(SMALL), run: (t) => { t.toString() } },
  TextToJson: { setup: () => builtText(SMALL), run: (t) => { t.toJSON() } },
  // Formatted, so toString walks a fragmented item chain rather than one merged run — the state a
  // rich-text consumer is actually in, and the one the unformatted case completely misses.
  TextToStringFormatted: {
    setup: () => {
      const t = builtText(SMALL)
      const rng = lcg()
      for (let j = 0; j < 500; j++) t.format(randInt(rng, SMALL - 20), 20, { bold: j % 2 === 0 })
      return t
    },
    run: (t) => { t.toString() },
  },
  TextInsertEmbed: () => {
    const t = newDoc().getText('t')
    t.insert(0, str(SMALL))
    for (let j = 0; j < 200; j++) t.insertEmbed(j, { img: 'x' })
  },

  // ---- XML surface + remaining ops ------------------------------------------------------
  // XML had no reference baseline at all. Its read paths walk the whole tree, so it is the least
  // safe surface to have been assuming about.

  XmlQuerySelector: { setup: () => builtXml(), run: (f) => { f.querySelector('span') } },
  XmlQuerySelectorAll: { setup: () => builtXml(), run: (f) => { f.querySelectorAll('div') } },
  XmlCreateTreeWalker: {
    setup: () => builtXml(),
    run: (f) => { for (const n of f.createTreeWalker(() => true)) { void n } },
  },
  XmlToString: { setup: () => builtXml(), run: (f) => { f.toString() } },
  XmlGetFirstChild: { setup: () => builtXml(), run: (f) => { void f.firstChild } },
  XmlSlice: { setup: () => builtXml(), run: (f) => { f.slice(0, XML_NODES / 2) } },
  XmlInsertAfter: {
    setup: () => {
      const f = newDoc().getXmlFragment('x')
      f.insert(0, [new Y.XmlElement('div')])
      return f
    },
    run: (f) => {
      const ref = f.firstChild
      for (let j = 0; j < 200; j++) f.insertAfter(ref, [new Y.XmlElement('div')])
    },
    perIterSetup: true, // mutates the fragment
  },
  // Replacing an attribute APPENDS a new item and tombstones the old one, so overwriting the same
  // key on a shared element grows history without bound and makes the measured workload depend on
  // how long the time budget let the loop run. A fresh element per iteration plus a fixed
  // XML_SET_ATTRIBUTE_OVERWRITES pins the history depth to the same value the Go and yrs harnesses
  // use; one reported op is that many replacements.
  XmlSetAttribute: {
    setup: () => builtXmlElement(),
    run: (el) => { for (let i = 0; i < XML_SET_ATTRIBUTE_OVERWRITES; i++) el.setAttribute('id', 'x') },
    perIterSetup: true,
  },
  XmlGetAttribute: {
    setup: () => builtXmlElement(),
    run: (el) => { for (let i = 0; i < 50; i++) el.getAttribute(mapKey(i)) },
  },
  XmlGetAttributes: { setup: () => builtXmlElement(), run: (el) => { el.getAttributes() } },
  XmlRemoveAttribute: {
    setup: () => builtXmlElement(),
    run: (el) => { for (let j = 0; j < 50; j++) el.removeAttribute(mapKey(j)) },
    perIterSetup: true, // removal mutates the element
  },

  // ApplyDelta is what every rich-text binding (Quill, ProseMirror) actually drives.
  // Go builds the delta once and creates the Doc/Text under b.StopTimer, timing ONLY ApplyDelta.
  // This previously timed newDoc().getText() as well, so the yjs figure included document
  // construction the Go figure excluded. perIterSetup because applyDelta mutates its fixture.
  TextApplyDelta: {
    setup: () => {
      const delta = []
      for (let j = 0; j < 200; j++) delta.push({ insert: 'chunk', attributes: { bold: j % 2 === 0 } })
      return { t: newDoc().getText('t'), delta }
    },
    run: ({ t, delta }) => { t.applyDelta(delta) },
    perIterSetup: true,
  },
  TextGetAttributes: {
    setup: () => {
      const t = builtText(SMALL)
      t.setAttribute('lang', 'en')
      return t
    },
    run: (t) => { t.getAttributes() },
  },

  ArraySplice: { setup: () => builtArray(SMALL), run: (a) => { a.slice(0, SMALL - 1) } },
  ArrayMap: { setup: () => builtArray(SMALL), run: (a) => { a.map((v) => v) } },
  MapGetSize: { setup: () => builtMap(SMALL), run: (m) => { void m.size } },

  ConcurrentMerge: {
    setup: () => {
      const mk = (clientID, tag) => {
        const d = new Y.Doc({ gc: false })
        d.clientID = clientID
        const t = d.getText('t')
        const rng = lcg()
        for (let j = 0; j < SMALL; j++) t.insert(randInt(rng, t.length + 1), tag)
        return Y.encodeStateAsUpdate(d)
      }
      return [mk(1, 'a'), mk(2, 'b')]
    },
    run: ([u1, u2]) => {
      const d = newDoc()
      Y.applyUpdate(d, u1)
      Y.applyUpdate(d, u2)
    },
  },
}

// Fixture builders for the coverage cases. Kept identical in shape to the Go helpers
// (benchArray/benchMap/benchText) so the two sides construct the same document before measuring.
function builtArray(n) {
  const a = newDoc().getArray('a')
  for (let j = 0; j < n; j++) a.insert(a.length, [j])
  return a
}

// Same key derivation as the Go mapKey, so both sides build keys of identical length and
// distribution rather than one side getting cheap short keys.
function mapKey(j) {
  return 'k' +
    String.fromCharCode(97 + (j % 26)) +
    String.fromCharCode(97 + (Math.floor(j / 26) % 26)) +
    String.fromCharCode(97 + (Math.floor(j / 676) % 26))
}

function builtMap(n) {
  const m = newDoc().getMap('m')
  for (let j = 0; j < n; j++) m.set(mapKey(j), j)
  return m
}

function builtText(n) {
  const t = newDoc().getText('t')
  t.insert(0, str(n))
  return t
}

const XML_NODES = 500

// Same tree the Go benchXmlTree builds: XML_NODES elements, two attributes each, one text child,
// with every third element a <span> so the selectors have something to discriminate on.
function builtXml() {
  const f = newDoc().getXmlFragment('x')
  for (let i = 0; i < XML_NODES; i++) {
    const el = new Y.XmlElement(i % 3 === 0 ? 'span' : 'div')
    el.setAttribute('id', mapKey(i))
    el.setAttribute('class', 'row')
    const txt = new Y.XmlText()
    txt.insert(0, 'cell')
    el.insert(0, [txt])
    f.insert(f.length, [el])
  }
  return f
}

function builtXmlElement() {
  const f = newDoc().getXmlFragment('x')
  const el = new Y.XmlElement('div')
  f.insert(0, [el])
  for (let i = 0; i < 50; i++) el.setAttribute(mapKey(i), 'v')
  return el
}

function builtDoc(n) {
  const rng = lcg()
  const d = newDoc()
  const t = d.getText('t')
  for (let j = 0; j < n; j++) t.insert(randInt(rng, t.length + 1), 'z')
  return d
}

// ---------------------------------------------------------------- runner

function measure(name, spec) {
  const isObj = typeof spec === 'object'
  const setup = isObj ? spec.setup : null
  const run = isObj ? spec.run : spec
  const perIter = isObj && spec.perIterSetup

  // Warmup, discarded: charging JIT compilation to the first measured iteration would understate
  // the reference by a large factor on the short cases.
  for (let i = 0; i < 2; i++) run(setup && (perIter || i === 0) ? setup() : lastSetup(setup, perIter))

  let fixture = !perIter && setup ? setup() : null
  let iters = 0
  // Accumulate RUN time only. The previous version took one timestamp before the loop and then
  // called setup() INSIDE it, so every perIterSetup case charged fixture construction to yjs --
  // silently, and in our favour. The budget still uses wall clock so an expensive fixture cannot
  // make a case run forever, but the reported figure excludes setup, matching Go's
  // b.StopTimer/b.StartTimer and the yrs harness's bench_setup.
  let elapsed = 0n
  const wall0 = process.hrtime.bigint()
  while (iters < MIN_ITERS || (process.hrtime.bigint() - wall0) < BigInt(BUDGET_NS)) {
    if (perIter && setup) fixture = setup()
    const t0 = process.hrtime.bigint()
    run(fixture)
    elapsed += process.hrtime.bigint() - t0
    iters++
    if (iters > 1e7) break
  }
  return { name, nsPerOp: Number(elapsed) / iters, iters }
}

let _cachedFixture = null
function lastSetup(setup, perIter) {
  if (!setup) return null
  if (perIter) return setup()
  if (_cachedFixture === null) _cachedFixture = setup()
  return _cachedFixture
}

const only = process.argv[2]
const results = []
for (const [name, spec] of Object.entries(cases)) {
  if (only && !name.includes(only)) continue
  _cachedFixture = null
  results.push(measure(name, spec))
}

// The ygo-shaped case: single random one-character inserts into a ~100k-char document, reported at
// FIXED iteration counts. It lives outside `cases` because the time-budget runner above is wrong
// for it -- the document grows as the loop runs, so an autoscaled iteration count would measure a
// different workload than the Go, Rust and ygo harnesses report at the same label.
//
// This case was previously missing from this harness entirely, which left the yjs column blank and
// read as though the reference could not be measured here. It could; it simply had not been
// written. Shape matches the Go benchmark exactly: build the 100k document once outside the timed
// region, then time `iters` inserts into the growing document.
if (!only || 'YText_RandomInsert_100k'.includes(only)) {
  for (const iters of [10, 1000, 10000]) {
    // Warm up on a THROWAWAY document. The workload mutates its document, so warming up on the
    // measured one would hand it a different starting state than the other three harnesses use --
    // but skipping warmup entirely would charge JIT compilation to a 10-iteration measurement,
    // which is the single easiest way to make the reference look slow by accident.
    {
      const w = newDoc().getText('t')
      w.insert(0, str(100000))
      const wrng = lcg()
      for (let i = 0; i < 200; i++) w.insert(randInt(wrng, w.length), 'x')
    }

    const t = newDoc().getText('t')
    t.insert(0, str(100000))
    const rng = lcg()
    const t0 = process.hrtime.bigint()
    for (let i = 0; i < iters; i++) t.insert(randInt(rng, t.length), 'x')
    const elapsed = Number(process.hrtime.bigint() - t0)
    results.push({ name: 'YText_RandomInsert_100k', nsPerOp: elapsed / iters, iters })
  }
}

process.stdout.write(JSON.stringify(results, null, 1) + '\n')
