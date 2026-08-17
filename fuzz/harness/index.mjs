// ESM face of the shared harness. The implementation lives in core.cjs so the CommonJS generators
// share it rather than keeping their own copy (Principle VII).
import core from './core.cjs'

export const mulberry32 = core.mulberry32
export const hex = core.hex
export const b64 = core.b64

/**
 * Corpus emitter with a health line.
 *
 * The health line goes to stderr and MUST NOT be discarded by callers: it is how a degenerate or
 * empty corpus is caught. A corpus emitting nothing used to pass the gate green (`cases=0`), so
 * `emitted=0` exits non-zero here rather than leaving that to the consumer.
 */
export function createEmitter({ surface, out = process.stdout, err = process.stderr } = {}) {
  let emitted = 0
  let dropped = 0
  const ops = new Set()

  return {
    /** Record one case. `opNames` feeds the exercised-operation report (FR-005a). */
    emit(record, opNames = []) {
      out.write(JSON.stringify(record) + '\n')
      emitted++
      for (const o of opNames) ops.add(o)
    },
    drop() {
      dropped++
    },
    finish() {
      err.write(
        `emitted=${emitted} dropped=${dropped} surface=${surface} ` +
          `opsExercised=${[...ops].sort().join(',')}\n`
      )
      if (emitted === 0) {
        err.write(
          `ERROR: surface=${surface} emitted 0 cases — a gate run against this corpus would ` +
            `compare NOTHING and report green\n`
        )
        process.exit(1)
      }
      if (dropped > 0) {
        err.write(`ERROR: surface=${surface} dropped ${dropped} seed(s); a partial corpus hides which seeds failed\n`)
        process.exit(1)
      }
    },
  }
}
