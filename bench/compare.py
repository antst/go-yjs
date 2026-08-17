#!/usr/bin/env python3
"""Render the three-way benchmark comparison from a bench/run-all.sh output directory.

This library is the BASELINE. A ratio above 1.00x means this library is faster than the other
implementation on that scenario; below 1.00x means the other implementation is faster.

    python3 bench/compare.py bench-results
"""
import json
import re
import statistics
import sys
from pathlib import Path

ORDER = [
    "TextAppendSmall", "TextAppendLarge",
    "TextInsertRandomSmall", "TextInsertRandomLarge",
    "TextDeleteRandom", "TextFormatChurn", "TextToDelta",
    "ArrayInsertSequential", "MapSet",
    "EncodeV1", "EncodeV2", "ApplyV1", "ApplyV2",
    "ConcurrentMerge", "YText_RandomInsert_100k",
]

# Batched variants: identical work in ONE transaction instead of N. Reported as a SEPARATE table
# rather than mixed into the medians above, because the two shapes measure different things and
# every implementation measured so far has a different standing in each. Folding them together
# produces a median that describes neither.
BATCHED = ["TextAppendLargeBatched", "ArrayInsertBatched", "MapSetBatched"]


def parse_go(path):
    """Go benchmark output. Median across -count runs; the ygo-shaped case is keyed by benchtime."""
    out = {}
    for line in path.read_text().splitlines():
        m = re.match(
            r"Benchmark(\w+)-\d+\s+\d+\s+([\d.]+) ns/op(?:\s+(\d+) B/op\s+(\d+) allocs/op)?"
            r"(?:\s+# benchtime=(\S+))?", line)
        if not m:
            continue
        name, ns, allocs, bt = m.group(1), float(m.group(2)), m.group(4), m.group(5)
        # MUST match bench/status.py's selection, or the same raw run renders as two different
        # numbers in the two summaries — which it did: 3,265 ns here against 2,187 ns there.
        # 10000x rather than 10x because the 10x point's samples spread 42-71% of their own median
        # and its document is still only ~21 physical Items, so it barely touches the path the row
        # measures. Unmarked rows are rejected outright: an unmarked row is autoscaled, which
        # measures a different workload for this history-sensitive case.
        if name == "YText_RandomInsert_100k" and bt != "10000x":
            continue
        e = out.setdefault(name, {"ns": [], "allocs": allocs})
        e["ns"].append(ns)
    return {k: {"ns": statistics.median(v["ns"]), "allocs": v["allocs"]} for k, v in out.items()}


def parse_js(path):
    """yjs harness output; the ygo-shaped case appears at several iteration counts — take fixed 10000.

    Without the iters filter the dict comprehension would silently keep whichever entry came last
    for a repeated name, rather than the one the other three implementations are compared at.
    """
    out = {}
    for r in json.loads(path.read_text()):
        # See the Go parser above: must match bench/status.py.
        if r["name"] == "YText_RandomInsert_100k" and r.get("iters") != 10000:
            continue
        out.setdefault(r["name"], r["nsPerOp"])
    return out


def parse_rust(path):
    """yrs harness output; the ygo-shaped case appears at several iteration counts — take fixed 10."""
    out = {}
    for line in path.read_text().splitlines():
        m = re.match(r"(\w+)\s+([\d.]+) ns/op\s+\(iters=(\d+)\)", line.strip())
        if not m:
            continue
        name, ns, iters = m.group(1), float(m.group(2)), int(m.group(3))
        # See the Go parser above: must match bench/status.py.
        if name == "YText_RandomInsert_100k" and iters != 10000:
            continue
        out.setdefault(name, ns)
    return out


def main():
    outdir = Path(sys.argv[1] if len(sys.argv) > 1 else "bench-results")
    go = parse_go(outdir / "go.txt") if (outdir / "go.txt").exists() else {}
    js = parse_js(outdir / "yjs.json") if (outdir / "yjs.json").exists() else {}
    rs = parse_rust(outdir / "yrs.txt") if (outdir / "yrs.txt").exists() else {}
    # ygo's harness emits the same "name  N ns/op  (iters=N)" shape as the Rust one.
    yg = parse_rust(outdir / "ygo.txt") if (outdir / "ygo.txt").exists() else {}

    missing = [n for n, d in (("go", go), ("yjs", js), ("yrs", rs), ("ygo", yg)) if not d]
    if missing:
        print(f"!! MISSING data for: {', '.join(missing)} — this is a PARTIAL comparison\n")

    fmt = lambda v: f"{v/1e6:.3f}" if v else "—"

    def render(title, keys):
        print(f"\n{title}")
        print(f"{'scenario':<26}{'ours(ms)':>10}{'yjs':>9}{'vs us':>8}"
              f"{'yrs':>9}{'vs us':>8}{'ygo':>10}{'vs us':>8}")
        print("-" * 88)
        jr, rr, gr = [], [], []
        rows = 0
        for k in keys:
            o = go.get(k, {}).get("ns")
            if not o:
                continue
            rows += 1
            y, r, g = js.get(k), rs.get(k), yg.get(k)
            a = f"{y/o:.2f}x" if y else "—"
            b = f"{r/o:.2f}x" if r else "—"
            c = f"{g/o:.2f}x" if g else "—"
            for val, acc in ((y, jr), (r, rr), (g, gr)):
                if val:
                    acc.append(val / o)
            print(f"{k:<26}{fmt(o):>10}{fmt(y):>9}{a:>8}{fmt(r):>9}{b:>8}{fmt(g):>10}{c:>8}")
        if not rows:
            print("  (no data)")
            return
        print()
        for label, acc in (("yjs", jr), ("yrs", rr), ("ygo", gr)):
            if acc:
                print(f"vs {label} : median {statistics.median(acc):.3f}x"
                      f"   faster on {sum(1 for x in acc if x > 1)}/{len(acc)}")

    render("PER-OPERATION  (one implicit transaction per mutation)", ORDER)
    render("BATCHED  (identical work, ONE transaction)", BATCHED)
    print("\n(ratio > 1.00x means THIS LIBRARY is faster)")


if __name__ == "__main__":
    main()
