#!/usr/bin/env python3
"""Generate the performance status page from a bench output directory.

    python3 bench/status.py bench-results [-o status.html]

This is the tool that turns raw harness output into the page we publish, so the page can be
regenerated after every batch of optimizations instead of being hand-edited. Hand-editing a
performance page is how stale numbers survive: the edit is easy, the re-measurement is not, and
the two drift silently apart.

WHAT IT ENFORCES

Every operation the oracle's coverage mapping tracks appears in the output, whether or not it was
measured. An operation with no benchmark is printed as UNMEASURED rather than omitted, because an
omitted row reads as "fine" and an unmeasured one reads as what it is. The same rule applies to
reference columns: a missing competitor number is either GO-ONLY (the operation has no counterpart
in that implementation, verified) or MISSING (nobody wrote the case yet). Those are very different
facts and the page distinguishes them.

Ratios are reference/ours, so above 1.00x means this library is faster.
"""
import argparse
from datetime import datetime
import subprocess
import json
import re
import statistics
import sys
from pathlib import Path

# The 36 operations the oracle's coverage mapping tracks, grouped for presentation. The benchmark
# name is the key; `op` is the tracked operation it exercises.
CATEGORIES = [
    ("Text — write", [
        ("TextAppendSmall", "Insert"), ("TextAppendLarge", "Insert"),
        ("TextInsertRandomSmall", "Insert"), ("TextInsertRandomLarge", "Insert"),
        ("TextDeleteRandom", "Delete"), ("TextFormatChurn", "Format"),
        ("TextInsertEmbed", "InsertEmbed"), ("TextApplyDelta", "ApplyDelta"),
    ]),
    ("Text — read", [
        ("TextToDelta", "ToDelta"), ("TextToString", "ToString"),
        ("TextToJson", "ToJson"), ("TextToStringFormatted", "ToString"),
        ("TextGetAttributes", "GetAttributes"),
    ]),
    ("Array — write", [
        ("ArrayInsertSequential", "Insert"), ("ArrayPush", "Push"),
        ("ArrayPushWithTombstones", "Push"), ("ArrayInsertEndWithTombstones", "Insert"),
        ("ArrayUnshift", "Unshift"), ("ArrayFrom", "From"),
    ]),
    ("Array — read", [
        ("ArrayToArray", "ToArray"), ("ArrayToJson", "ToJson"),
        ("ArrayForEach", "ForEach"), ("ArrayGetRandom", "Get"),
        ("ArraySplice", "Splice"), ("ArrayMap", "Map"), ("ArrayRange", "Range"),
    ]),
    ("Map", [
        ("MapSet", "Set"), ("MapKeys", "Keys"), ("MapValues", "Values"),
        ("MapEntries", "Entries"), ("MapToJson", "ToJson"), ("MapHas", "Has"),
        ("MapClear", "Clear"), ("MapGetSize", "GetSize"),
    ]),
    ("XML", [
        ("XmlQuerySelector", "QuerySelector"), ("XmlQuerySelectorAll", "QuerySelectorAll"),
        ("XmlCreateTreeWalker", "CreateTreeWalker"), ("XmlToString", "ToString"),
        ("XmlGetFirstChild", "GetFirstChild"), ("XmlSlice", "Slice"),
        ("XmlInsertAfter", "InsertAfter"), ("XmlSetAttribute", "SetAttribute"),
        ("XmlGetAttribute", "GetAttribute"), ("XmlGetAttributes", "GetAttributes"),
        ("XmlRemoveAttribute", "RemoveAttribute"),
    ]),
    ("Codec & sync", [
        ("EncodeV1", "—"), ("EncodeV2", "—"), ("ApplyV1", "—"), ("ApplyV2", "—"),
        ("ConcurrentMerge", "—"), ("YText_RandomInsert_100k", "Insert"),
    ]),
    ("Batched — one transaction", [
        ("TextAppendLargeBatched", "Insert"), ("ArrayInsertBatched", "Insert"),
        ("MapSetBatched", "Set"),
    ]),
]

# Benchmarks with no counterpart in a given implementation. Recorded explicitly, with the reason,
# so a blank cell can never be mistaken for an unwritten case -- the last time a cell was blank
# here it was hiding a 3.1x loss, not a missing feature.
GO_ONLY = {
    "ArrayRange": "item-level iterator; Go-only convenience, no yjs/yrs equivalent",
    "ArrayFrom": "constructs a detached array; no direct yjs/yrs counterpart",
}

# Per-implementation absences: the operation exists here and in some references, but genuinely not
# in the named one. Distinct from GO_ONLY (absent from every reference) and from a case nobody has
# written yet. Each entry states WHY, because "n/a" without a reason is indistinguishable from
# "we did not get to it" -- and an unexplained blank in this comparison once hid a 3.1x loss.
NOT_APPLICABLE = {
    ("XmlQuerySelector", "yrs"): "yrs exposes no CSS-selector query API",
    ("XmlQuerySelectorAll", "yrs"): "yrs exposes no CSS-selector query API",
    ("TextGetAttributes", "yrs"): "yrs TextRef carries no type-level attributes (XmlTextRef does)",
    ("TextGetAttributes", "ygo"): "ygo YText exposes no type-level attribute accessor",
    ("MapValues", "ygo"): "ygo YMap has no Values accessor",
    ("MapClear", "ygo"): "ygo YMap has no Clear",
    ("MapGetSize", "ygo"): "ygo YMap exposes no size accessor",
    ("ArrayMap", "ygo"): "ygo YArray has no Map",
    ("XmlQuerySelector", "ygo"): "ygo exposes no CSS-selector query API",
    ("XmlQuerySelectorAll", "ygo"): "ygo exposes no CSS-selector query API",
    ("XmlCreateTreeWalker", "ygo"): "ygo exposes no tree walker",
    ("XmlSlice", "ygo"): "ygo YXmlFragment has no Slice",
    # ygo's ToJSON MARSHALS -- json.Marshal(ToSlice()) / (Entries()) / (ToString()) -- where ours
    # and yjs's toJSON return the value itself. Strictly more work, so the rows are not comparable.
    ("TextToJson", "ygo"): "ygo ToJSON calls json.Marshal; ours and yjs's return the value",
    ("ArrayToJson", "ygo"): "ygo ToJSON calls json.Marshal; ours and yjs's return the value",
    ("MapToJson", "ygo"): "ygo ToJSON calls json.Marshal; ours and yjs's return the value",
}


def parse_go(path):
    out = {}
    for line in path.read_text().splitlines():
        m = re.match(r"Benchmark(\w+)-\d+\s+\d+\s+([\d.]+) ns/op"
                     r"(?:\s+(\d+) B/op\s+(\d+) allocs/op)?(?:\s+# benchtime=(\S+))?", line)
        if not m:
            continue
        name, ns, nbytes, allocs, bt = (m.group(1), float(m.group(2)),
                                        m.group(3), m.group(4), m.group(5))
        # This case is history-sensitive -- the document grows as the loop runs -- so ONLY an
        # explicitly-marked fixed-count row is comparable. Accepting unmarked rows would silently
        # admit autoscaled ones from a `-bench .` sweep, which measure a different workload; that
        # exact mistake produced a bogus 4.5x claim once already.
        #
        # 10000x, not the 10x this used to take. All four harnesses emit 10x/1000x/10000x, so any
        # of them is comparable, but they are not equally INFORMATIVE. Measured across three
        # commits, the 10x point's five samples spread 42-71% of their own median, which cannot
        # resolve the differences we ask it about -- it reported a 35% regression whose samples
        # overlapped the baseline's range almost entirely. The 10000x point spread 2.7-20%. It is
        # also the more representative workload: at 10x the 100k-character document is still only
        # ~21 physical Items, so it barely touches the position-lookup path the row exists to
        # measure, while at 10000x it is ~19,000. The document grows 10% over the run, identically
        # for every implementation, so the comparison stays fair.
        if name == "YText_RandomInsert_100k" and bt != "10000x":
            continue
        e = out.setdefault(name, {"ns": [], "b": nbytes, "allocs": allocs})
        e["ns"].append(ns)
    return {k: {"ns": statistics.median(v["ns"]), "b": v["b"], "allocs": v["allocs"]}
            for k, v in out.items()}


def parse_js(path):
    out = {}
    for r in json.loads(path.read_text()):
        # See parse_go: the 10000x point, because the 10x one is too noisy to resolve what we ask
        # of it and too small to exercise the path it measures.
        if r["name"] == "YText_RandomInsert_100k" and r.get("iters") != 10000:
            continue
        out.setdefault(r["name"], r["nsPerOp"])
    return out


def parse_ns_table(path):
    """The yrs and ygo harnesses share a `name  N ns/op  (iters=N)` output shape."""
    out = {}
    for line in path.read_text().splitlines():
        m = re.match(r"(\w+)\s+([\d.]+) ns/op\s+\(iters=(\d+)\)", line.strip())
        if not m:
            continue
        name, ns, iters = m.group(1), float(m.group(2)), int(m.group(3))
        # See parse_go for why 10000 rather than 10.
        if name == "YText_RandomInsert_100k" and iters != 10000:
            continue
        out.setdefault(name, ns)
    return out


def load(outdir):
    p = Path(outdir)
    def maybe(fn, name):
        f = p / name
        return fn(f) if f.exists() else {}
    return (maybe(parse_go, "go.txt"), maybe(parse_js, "yjs.json"),
            maybe(parse_ns_table, "yrs.txt"), maybe(parse_ns_table, "ygo.txt"))


# A ratio this extreme in EITHER direction has, every single time so far, turned out to be a
# harness defect rather than a real difference: setup inside a timed region, an
# operation-per-transaction mismatch, a runner ignoring its own perIterSetup flag, and in-loop key
# formatting comparing each language's fmt implementation. Five for five. So the tool now says so
# rather than leaving it to whoever happens to read the table sceptically.
IMPLAUSIBLE_RATIO = 10.0

# Rows whose reported figure is NOT one call of the named operation. Where an operation accumulates
# CRDT history, timing a single call against a shared fixture makes the measurement depend on how
# many iterations each harness happened to run, so the timed region performs a fixed number of calls
# on a fresh fixture instead. The absolute number then looks large next to its neighbours, which is
# exactly why it is stated here: the ratios remain valid because all implementations use the same
# count, but nobody should read the ns figure as a per-call cost.
# Rows whose measurement PREDATES a harness fairness fix, so the competitor number in the current
# data set is not comparable and the row must be re-measured before it is quoted. Recorded rather
# than silently dropped: a missing row reads as "fine", and an unexplained loss reads as a real one.
#
# Empty as of b97070d: XmlGetFirstChild was the only entry and has been re-measured on the fixed
# harness (0.80x against yrs, where the incomparable version read 0.40x). Kept as a mechanism
# because it will be needed again — every harness fix invalidates its own row's history.
STALE_ROWS = {}

UNIT_NOTES = {
    "XmlSetAttribute": "one op = 100 replacements of the same key on a fresh 50-attribute element; "
                       "replacing a key appends an item and tombstones the old one, so a shared "
                       "fixture would let each harness's iteration policy choose the history depth",
}

# Extreme ratios that have been INVESTIGATED and are genuinely algorithmic, with the reason. This
# works like NOT_APPLICABLE: the point is not to suppress the warning but to record that someone
# checked, so the flagged list shrinks toward zero as rows are verified rather than being ignored
# wholesale. An entry here is a claim that both sides were confirmed to time the same work.
VERIFIED_EXTREME = {
    ("MapGetSize", "yjs"): "ours maintains a counter; yjs .size iterates and filters deleted",
    ("MapGetSize", "yrs"): "ours maintains a counter; yrs len() walks the map",
    ("XmlGetFirstChild", "ygo"): "ygo Children() materialises the whole child slice",
    ("TextAppendLarge", "ygo"): "ygo has no tail-append fast path; quadratic on this shape",
    ("ArrayInsertSequential", "ygo"): "same: no tail fast path",
    ("TextAppendSmall", "ygo"): "same: no tail fast path",
    ("ArrayGetRandom", "ygo"): "ygo takes a document RWMutex on every read; we take none",
    # Verified by scaling rather than inspection: yjs's per-op cost for a batched append GROWS with
    # n -- 21.4us at 2,500 rising to 127.0us at 20,000, total time ~n^1.86 -- because nothing merges
    # inside an open transaction, so each insert traverses a longer unmerged run. Ours is linear
    # after the coalescing work. The ratio is real and widens with document size.
    ("TextAppendLargeBatched", "yjs"): "yjs batched append is superlinear (~n^1.86); ours is linear",
    ("TextAppendLargeBatched", "ygo"): "same shape: ygo has no tail-append fast path either",
    ("TextAppendLarge", "ygo"): "ygo has no tail-append fast path; quadratic on this shape",
}

# Rows where BOTH sides perform the operation but under different ownership contracts, so the
# ratio measures API shape rather than efficiency. Distinct from NOT_APPLICABLE (the operation is
# absent) and from VERIFIED_EXTREME (the ratio is real and algorithmic). Recording these stops
# someone optimising an algorithm that is already fine -- the danger is real, since a moderate loss
# looks exactly like a tractable one.
SEMANTIC_MISMATCH = {
    ("MapKeys", "yrs"):
        "ours returns a caller-OWNED slice; yrs keys() returns an iterator. At 2,000 keys the whole "
        "cost is one 32,768-byte allocation: Keys() measures 5,323 ns/1 alloc while AppendKeys into "
        "a reused buffer does the same work in 396 ns with zero allocations -- 13.4x, and faster "
        "than the yrs figure this row is compared against. The traversal is not the cost.",
}

# A caveat that applies to the WHOLE ygo column rather than any single row: ygo acquires a
# document-level RWMutex on every read and write -- 71 lock sites across its crdt package -- so it
# is thread-safe by default where this library is not. Part of every ygo margin is that lock. The
# comparison is still worth making, but it is a speed-versus-safety tradeoff and not a like-for-like
# efficiency result, so the page says so rather than letting the ratios imply otherwise.
YGO_LOCK_CAVEAT = ("ygo acquires a document RWMutex on every read and write (71 lock sites), so it "
                   "is thread-safe by default where this library is not. Part of every ygo margin "
                   "is that lock: a speed-versus-safety tradeoff, not a like-for-like result.")


def classify(ours, ref, name=None, impl=None):
    """Ratio and verdict for one reference cell."""
    if ours is None:
        return None, "unmeasured"
    # NOT_APPLICABLE means the two sides do not perform the same operation. That is true whether or
    # not the other implementation produced a number -- ygo's ToJSON marshals where ours returns the
    # value, so it HAS a figure and the figure is meaningless. Suppress the ratio rather than
    # publishing a 1000x that measures json.Marshal.
    if name is not None and (name, impl) in NOT_APPLICABLE:
        return None, "na"
    if ref is None:
        if name is not None and ((name, impl) in NOT_APPLICABLE or name in GO_ONLY):
            return None, "na"
        return None, "noref"
    r = ref / ours
    if r < 1.0:
        return r, "loss"
    if r < 1.5:
        return r, "thin"
    return r, "win"


def best_competitor(cells, srcs, name):
    """Fastest reference for this benchmark: (label, ns, ratio-vs-ours).

    Answers the question the three separate columns cannot -- "are we the fastest, and if not, who
    beats us and by how much" -- without the reader mentally scanning three ratios per row.
    Implementations with no counterpart are excluded rather than counted as infinitely slow.
    """
    best_label, best_ns = None, None
    for label, src in srcs:
        v = src.get(name)
        if v is None:
            continue
        if best_ns is None or v < best_ns:
            best_label, best_ns = label, v
    return best_label, best_ns


def build_rows(go, js, rs, yg):
    rows = []
    for cat, entries in CATEGORIES:
        for name, op in entries:
            ours = go.get(name, {}).get("ns")
            cells = {}
            for label, src in (("yjs", js), ("yrs", rs), ("ygo", yg)):
                cells[label] = classify(ours, src.get(name), name, label)
            bl, bns = best_competitor(cells, (("yjs", js), ("yrs", rs), ("ygo", yg)), name)
            rows.append({
                "cat": cat, "name": name, "op": op, "ours": ours,
                "allocs": go.get(name, {}).get("allocs"),
                "cells": cells,
                "goonly": name in GO_ONLY,
                "best_label": bl,
                "best_ratio": (bns / ours) if (bns and ours) else None,
            })
    return rows


def summarize(rows):
    s = {"total": len(rows), "measured": 0, "unmeasured": [], "losses": [], "thin": [],
         "pending": [], "fastest": 0, "ranked": 0, "implausible": []}
    for r in rows:
        if r["ours"] is None:
            s["unmeasured"].append(r["name"])
            continue
        s["measured"] += 1
        if r["best_ratio"] is not None:
            s["ranked"] += 1
            if r["best_ratio"] > 1.0:
                s["fastest"] += 1
        for label, (ratio, verdict) in r["cells"].items():
            if verdict == "loss":
                s["losses"].append((r["name"], label, ratio))
            elif verdict == "thin":
                s["thin"].append((r["name"], label, ratio))
            elif verdict == "noref":
                s["pending"].append((r["name"], label))
            if (ratio is not None
                    and (ratio >= IMPLAUSIBLE_RATIO or ratio <= 1.0 / IMPLAUSIBLE_RATIO)
                    and (r["name"], label) not in VERIFIED_EXTREME):
                s["implausible"].append((r["name"], label, ratio))
    s["losses"].sort(key=lambda x: x[2])
    return s


def text_report(rows, s):
    print(f"{'benchmark':<32}{'ours(ms)':>11}{'vs best':>12}{'yjs':>9}{'yrs':>9}{'ygo':>9}")
    print("-" * 79)
    cat = None
    for r in rows:
        if r["cat"] != cat:
            cat = r["cat"]
            print(f"\n== {cat}")
        o = f"{r['ours']/1e6:.4f}" if r["ours"] else "UNMEASURED"
        cs = []
        for label in ("yjs", "yrs", "ygo"):
            ratio, verdict = r["cells"][label]
            cs.append("—" if ratio is None else f"{ratio:.2f}x")
        b = "—" if r["best_ratio"] is None else f"{r['best_ratio']:.2f}x({r['best_label']})"
        mark = "*" if r["name"] in UNIT_NOTES else ""
        print(f"{r['name']+mark:<32}{o:>11}{b:>12}{cs[0]:>9}{cs[1]:>9}{cs[2]:>9}")
    print(f"\nmeasured {s['measured']}/{s['total']}")
    for name, note in UNIT_NOTES.items():
        print(f"* {name}: {note}")
    for name, why in STALE_ROWS.items():
        print(f"! {name} NOT COMPARABLE in this data set: {why}")
    if s["unmeasured"]:
        print(f"UNMEASURED: {', '.join(s['unmeasured'])}")
    if s["pending"]:
        print(f"REFERENCE CELLS PENDING: {len(s['pending'])} "
              f"(harness case not run: {', '.join(sorted({n for n, _ in s['pending']}))})")
    if s["implausible"]:
        print(f"\nVERIFY HARNESS PARITY -- {len(s['implausible'])} ratio(s) beyond "
              f"{IMPLAUSIBLE_RATIO:.0f}x. Every extreme ratio investigated so far was a harness")
        print("defect, not a real difference. Confirm both sides time the same work before quoting:")
        for name, label, rt in sorted(s["implausible"], key=lambda x: -max(x[2], 1 / x[2])):
            print(f"  {name:<30} vs {label:<4} {rt:>9.2f}x")
    if s["losses"]:
        print("\nLOSSES (ranked, worst first):")
        for name, label, ratio in s["losses"]:
            if (name, label) in SEMANTIC_MISMATCH:
                print(f"  {name:<32} vs {label:<4} {ratio:.2f}x  [SEMANTICS DIFFER — not an efficiency result]")
                print(f"      {SEMANTIC_MISMATCH[(name, label)]}")
                continue
            print(f"  {name:<32} vs {label:<4} {ratio:.2f}x  ({1/ratio:.1f}x slower)")


def html(rows, s, meta):
    def cell(r, label):
        ratio, verdict = r["cells"][label]
        if verdict == "unmeasured":
            return '<td class="c"><span class="p u">—</span></td>'
        if verdict == "na":
            # A verified statement about the OTHER implementation, carrying its reason.
            why = NOT_APPLICABLE.get((r["name"], label)) or GO_ONLY.get(r["name"], "")
            return f'<td class="c"><span class="p n" title="{why}">n/a</span></td>'
        if verdict == "noref":
            # Nobody has run that harness case yet. This says something about US, not about them,
            # and must never render like the line above.
            return '<td class="c"><span class="p q" title="harness case not run yet">pending</span></td>'
        klass = {"loss": "l", "thin": "t", "win": "w"}[verdict]
        return f'<td class="c"><span class="p {klass}">{ratio:.2f}×</span></td>'

    body = []
    cat = None
    for r in rows:
        if r["cat"] != cat:
            cat = r["cat"]
            body.append(f'<tr class="cat"><td colspan="6">{cat}</td></tr>')
        ours = f'{r["ours"]/1e6:.4f}' if r["ours"] else '<span class="p u">unmeasured</span>'
        # "vs best" is the fastest competitor for this row: the single number that answers
        # "are we ahead of the field", which three separate ratios do not.
        if r["best_ratio"] is None:
            bcell = '<td class="c"><span class="p u">—</span></td>'
        else:
            bk = "l" if r["best_ratio"] < 1.0 else ("t" if r["best_ratio"] < 1.5 else "w")
            bcell = (f'<td class="c b"><span class="p {bk}" title="fastest competitor: '
                     f'{r["best_label"]}">{r["best_ratio"]:.2f}×</span></td>')
        note = UNIT_NOTES.get(r["name"])
        nm = (f'{r["name"]}<sup class="un" title="{note}">unit</sup>' if note else r["name"])
        if r["name"] in STALE_ROWS:
            nm += f'<sup class="un" title="{STALE_ROWS[r["name"]]}">stale</sup>' 
        body.append(
            f'<tr><td class="n">{nm}</td><td class="o">{ours}</td>' + bcell
            + cell(r, "yjs") + cell(r, "yrs") + cell(r, "ygo") + '</tr>')

    losses = "".join(
        f'<li><code>{n}</code> vs {l} — <b>{1/rt:.1f}× slower</b></li>'
        for n, l, rt in s["losses"]) or "<li>none</li>"

    return TEMPLATE.format(
        rows="\n".join(body), losses=losses,
        measured=s["measured"], total=s["total"],
        nloss=len(s["losses"]), meta=" · ".join(meta),
        nloss_class="b" if s["losses"] else "g",
        npending=len(s["pending"]),
        nfastest=s["fastest"], nranked=s["ranked"],
        nimplausible=len(s["implausible"]),
        ygo_caveat=YGO_LOCK_CAVEAT,
        implausible_class="b" if s["implausible"] else "g",
        fastest_class="g" if s["ranked"] and s["fastest"] == s["ranked"] else "b",
    )


TEMPLATE = """<title>Performance Status</title>
<style>
:root{{--bg:#F6F7F9;--s:#FFF;--s2:#EFF1F5;--ink:#161A21;--ink2:#3E4654;--ink3:#6E7789;
--rule:#DCE0E8;--rule2:#C3C9D6;--acc:#42548A;--accs:#E7EAF4;
--w:#1C7565;--ws:#DCEDE8;--l:#B0543A;--ls:#F3E2DC;--t:#8A6A18;--ts:#F5EBD9;
--mono:ui-monospace,"SF Mono",Menlo,Consolas,monospace;
--sans:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}}
@media (prefers-color-scheme:dark){{:root:not([data-theme="light"]){{
--bg:#0F1217;--s:#161A21;--s2:#1D222B;--ink:#E6EAF1;--ink2:#B4BCCB;--ink3:#828C9E;
--rule:#272D38;--rule2:#39414F;--acc:#8FA3D8;--accs:#1F2637;
--w:#5FBFA8;--ws:#152A26;--l:#E08A6C;--ls:#33211A;--t:#D9A441;--ts:#2C2415}}}}
:root[data-theme="dark"]{{--bg:#0F1217;--s:#161A21;--s2:#1D222B;--ink:#E6EAF1;--ink2:#B4BCCB;
--ink3:#828C9E;--rule:#272D38;--rule2:#39414F;--acc:#8FA3D8;--accs:#1F2637;
--w:#5FBFA8;--ws:#152A26;--l:#E08A6C;--ls:#33211A;--t:#D9A441;--ts:#2C2415}}
*{{box-sizing:border-box}}
body{{margin:0;background:var(--bg);color:var(--ink);font-family:var(--sans);font-size:16px;
line-height:1.6;-webkit-font-smoothing:antialiased}}
.wrap{{max-width:64rem;margin:0 auto;padding:3rem 1.5rem 4.5rem}}
.mast{{border-bottom:2px solid var(--ink);padding-bottom:1.25rem;margin-bottom:1.5rem}}
.eyebrow{{font-family:var(--mono);font-size:.68rem;letter-spacing:.16em;text-transform:uppercase;
color:var(--acc);margin:0 0 .75rem}}
h1{{font-size:clamp(1.8rem,4vw,2.6rem);line-height:1.06;letter-spacing:-.025em;font-weight:650;
margin:0 0 .6rem}}
.stand{{font-size:1.05rem;color:var(--ink2);margin:0;max-width:44em}}
.meta{{font-family:var(--mono);font-size:.7rem;color:var(--ink3);margin-top:1rem}}
.staleban{{background:#7f1d1d;color:#fff;font-weight:700;padding:.75rem 1rem;border-radius:.4rem;
margin:0 0 1rem;font-size:.85rem}}
.cards{{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));gap:.75rem;
margin:1.5rem 0 2rem}}
.card{{background:var(--s);border:1px solid var(--rule);border-radius:6px;padding:1rem}}
.card .k{{font-family:var(--mono);font-size:.62rem;letter-spacing:.12em;text-transform:uppercase;
color:var(--ink3);margin-bottom:.4rem}}
.card .v{{font-family:var(--mono);font-size:1.5rem;font-weight:650;letter-spacing:-.02em}}
.card .cd{{font-size:.7rem;color:var(--ink3);margin-top:.2rem}}
.v.g{{color:var(--w)}} .v.b{{color:var(--l)}}
.tw{{overflow-x:auto;border:1px solid var(--rule);border-radius:6px;background:var(--s)}}
table{{border-collapse:collapse;width:100%;font-size:.85rem;font-variant-numeric:tabular-nums}}
th{{font-family:var(--mono);font-size:.6rem;letter-spacing:.1em;text-transform:uppercase;
color:var(--ink3);background:var(--s2);border-bottom:1.5px solid var(--rule2);padding:.7rem;
font-weight:700;text-align:right;white-space:nowrap}}
th:first-child{{text-align:left}}
td{{padding:.55rem .7rem;border-bottom:1px solid var(--rule);text-align:right;color:var(--ink2);
white-space:nowrap}}
td.n{{text-align:left;font-family:var(--mono);font-size:.76rem;color:var(--ink)}}
td.o{{font-family:var(--mono);font-weight:650;color:var(--ink);background:var(--accs)}}
td.c{{width:5.5rem}}
td.c.b{{background:var(--s2);border-left:2px solid var(--rule2);border-right:2px solid var(--rule2)}}
tr.cat td{{background:var(--s2);font-family:var(--mono);font-size:.64rem;letter-spacing:.12em;
text-transform:uppercase;color:var(--ink3);font-weight:700;text-align:left;padding:.6rem .7rem}}
.p{{font-family:var(--mono);font-size:.72rem;font-weight:700;padding:.15em .45em;border-radius:3px}}
.un{{font-size:.6em;vertical-align:super;opacity:.55;cursor:help;margin-left:.25em}}
.p.w{{color:var(--w);background:var(--ws)}}
.p.l{{color:var(--l);background:var(--ls)}}
.p.t{{color:var(--t);background:var(--ts)}}
.p.u,.p.n{{color:var(--ink3)}}
.p.q{{color:var(--t);background:var(--ts)}}
h2{{font-size:1.2rem;font-weight:650;margin:2.5rem 0 .5rem}}
ul{{max-width:60ch;padding-left:1.2rem}} li{{margin-bottom:.35rem}}
code{{font-family:var(--mono);font-size:.86em;background:var(--s2);padding:.1em .35em;
border-radius:3px}}
.legend{{font-family:var(--mono);font-size:.68rem;color:var(--ink3);margin-top:.75rem}}
p{{max-width:64ch}}
</style>
<div class="wrap">
<div class="mast">
  <p class="eyebrow">Generated by bench/status.py — do not hand-edit</p>
  <h1>Performance Status</h1>
  <p class="stand">Every operation the differential oracle tracks, measured against the yjs
  reference, yrs and reearth/ygo on one machine in one session. Ratios are reference ÷ ours, so
  above 1.00× means this library is faster.</p>
  <p class="meta">{meta}</p>
</div>
<div class="cards">
  <div class="card"><div class="k">operations measured</div><div class="v">{measured}/{total}</div></div>
  <div class="card"><div class="k">losses outstanding</div><div class="v {nloss_class}">{nloss}</div></div>
  <div class="card"><div class="k">reference cells pending</div><div class="v">{npending}</div></div>
  <div class="card"><div class="k">fastest of all four</div><div class="v {fastest_class}">{nfastest}/{nranked}</div></div>
  <div class="card"><div class="k">verify harness parity</div><div class="v {implausible_class}">{nimplausible}</div><div class="cd">ratios beyond 10x</div></div>
</div>
<div class="tw">
<table>
<thead><tr><th>benchmark</th><th>ours (ms)</th><th>vs best</th><th>vs yjs</th><th>vs yrs</th><th>vs ygo</th></tr></thead>
<tbody>
{rows}
</tbody>
</table>
</div>
<p class="legend">ygo caveat: {ygo_caveat}</p>
<p class="legend">green = we lead · amber = lead under 1.5× · red = we are slower ·
n/a = no counterpart in that implementation · — = not measured</p>
<h2>Outstanding losses</h2>
<ul>{losses}</ul>
</div>
"""


def provenance(outdir):
    """Provenance the page must carry, derived rather than supplied.

    A rendered page gets handed around on its own, so anything a reader needs in
    order to trust it has to be ON it. Two failures motivated this: a status.html
    whose mtime was today over raw data measured two days earlier, and a tracked
    page 62 commits behind mainline that named no commit at all. Neither said so
    anywhere a reader would look.

    Returns (lines, stale_reason). stale_reason is None when the data was measured
    at the commit currently checked out.
    """
    out = Path(outdir)
    lines, recorded, recorded_measured = [], None, None
    prov = out / "PROVENANCE"
    if prov.exists():
        for line in prov.read_text().splitlines():
            if line.startswith("commit:"):
                recorded = line.split(":", 1)[1].strip()
            if line.startswith("measured:"):
                recorded_measured = line.split(":", 1)[1].strip()
            if line.strip():
                lines.append(line.strip())

    # The raw measurement time, not the render time. status.py can be re-run at
    # any point over untouched inputs, which is precisely how a stale page acquires
    # a fresh timestamp.
    #
    # A RECORDED time beats an mtime, because mtimes lie in the common case:
    # checking a dataset out of git, or copying it between machines, rewrites every
    # timestamp to now. That produced a page claiming data was measured minutes ago
    # when it had been collected hours earlier on another host.
    if recorded_measured:
        lines.append(f"raw data measured: {recorded_measured}")
    else:
        raw = [out / n for n in ("go.txt", "yjs.json", "yrs.txt", "ygo.txt")]
        stamps = [f.stat().st_mtime for f in raw if f.exists()]
        if stamps:
            measured = datetime.fromtimestamp(max(stamps)).strftime("%Y-%m-%d %H:%M")
            lines.append(f"raw data measured (file mtime, may be a copy time): {measured}")
    lines.append(f"rendered: {datetime.now().strftime('%Y-%m-%d %H:%M')}")

    head = None
    try:
        head = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True,
                              text=True, cwd=str(Path(__file__).resolve().parent.parent)
                              ).stdout.strip() or None
    except Exception:
        head = None

    if recorded is None:
        return lines, "this dataset records no source commit, so it cannot be placed in history"
    if head and recorded != head:
        behind = ""
        try:
            r = subprocess.run(["git", "rev-list", "--count", f"{recorded}..HEAD"],
                               capture_output=True, text=True,
                               cwd=str(Path(__file__).resolve().parent.parent))
            if r.returncode == 0 and r.stdout.strip():
                behind = f", {r.stdout.strip()} commits behind"
        except Exception:
            pass
        return lines, f"measured at {recorded[:7]}{behind}; the checkout is {head[:7]}"

    # A hand-written warning outranks a matching commit, because a human can know
    # something the metadata cannot — a harness bug, a polluted host, a superseded
    # method. It is checked by CONTENT, not by filename: bench-results-amd64 also
    # holds a STALE.md, but that one is a dataset-selection note explaining a
    # deliberate cross-window pairing, and treating its title as a staleness
    # declaration reported a true verdict for an entirely wrong reason.
    marker = out / "STALE.md"
    if marker.exists():
        text = marker.read_text()
        if "do not quote" in text.lower():
            first = next((l.strip().lstrip("# ").strip() for l in text.splitlines() if l.strip()), "")
            return lines, f"{marker.name} says: {first}"
    return lines, None


def banner(reason):
    bar = "=" * 78
    print(f"\n\033[41;97m{bar}\033[0m")
    print(f"\033[41;97m STALE — DO NOT QUOTE THESE NUMBERS \033[0m")
    print(f"\033[41;97m {reason} \033[0m")
    print(f"\033[41;97m Regenerate from a run at the current commit. \033[0m")
    print(f"\033[41;97m{bar}\033[0m\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("outdir", nargs="?", default="bench-results")
    ap.add_argument("-o", "--output", default=None, help="write HTML here")
    ap.add_argument("--meta", action="append", default=[],
                    help="provenance line, repeatable (commit, host, load)")
    ap.add_argument("--allow-stale", action="store_true",
                    help="print a stale dataset anyway and exit 0")
    args = ap.parse_args()

    go, js, rs, yg = load(args.outdir)
    if not go:
        sys.exit(f"no go.txt in {args.outdir} — nothing to report")
    rows = build_rows(go, js, rs, yg)
    s = summarize(rows)

    # Provenance is checked for EVERY invocation, not only when rendering HTML.
    # It used to be checked inside `if args.output:`, so the plain terminal
    # report — the way this is actually run when someone asks "how fast are we?"
    # — printed a full comparison table with no staleness check at all. That is
    # how a four-day-old superseded dataset got quoted as current. The banner
    # goes both before and after the table because a long table scrolls its own
    # header off the screen.
    lines, stale = provenance(args.outdir)
    if stale:
        banner(stale)
    text_report(rows, s)
    if stale:
        banner(stale)

    if args.output:
        meta = args.meta + lines
        page = html(rows, s, meta or [args.outdir])
        if stale:
            page = page.replace(
                '<div class="wrap">',
                '<div class="wrap"><p class="staleban">STALE — DO NOT QUOTE THESE NUMBERS: '
                + stale + '. Regenerate from a run at the current commit.</p>', 1)
        Path(args.output).write_text(page)
        print(f"\nwrote {args.output}")
        if stale:
            print(f"WARNING: page marked stale — {stale}")

    # Exit non-zero so a stale dataset cannot be quoted by a script either.
    if stale and not args.allow_stale:
        sys.exit(2)


if __name__ == "__main__":
    main()
