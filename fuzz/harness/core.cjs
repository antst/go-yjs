// CommonJS core of the shared harness, so both the ESM generators (native_diff_*.mjs) and the
// CommonJS ones (ops.js, consumed by generate.js) use ONE implementation. index.mjs re-exports
// this; migrating only the .mjs files would have left copies behind in exactly the place the
// extraction was meant to remove them.

/**
 * Seeded PRNG. Same seed must yield the same corpus, for every generator (C-H5.1).
 *
 * The paren placement on the final line is load-bearing: `((t ^ (t >>> 14)) >>> 0)`. An earlier
 * copy read `((t^(t>>>14)>>>0))`, which yields signed/negative values and a skewed distribution —
 * a degenerate corpus that reported a false "0 divergence".
 */
function mulberry32(a) {
  return function () {
    a |= 0
    a = (a + 0x6d2b79f5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

/** Hex-encode for byte comparison against the Go side. */
function hex(u8) {
  return Buffer.from(u8).toString('hex')
}

/** Base64-encode, for payloads the Go side decodes rather than hex-compares. */
function b64(u8) {
  return Buffer.from(u8).toString('base64')
}

module.exports = { mulberry32, hex, b64 }
