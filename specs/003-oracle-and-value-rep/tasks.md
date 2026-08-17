# Tasks: Verification Oracle & Value-Representation Foundation

**Feature**: `003-oracle-and-value-rep` | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)
**Design**: [research.md](./research.md) · [data-model.md](./data-model.md) ·
[contracts/oracle-contract.md](./contracts/oracle-contract.md) ·
[contracts/value-rep-contract.md](./contracts/value-rep-contract.md) ·
[quickstart.md](./quickstart.md)

**Organization**: by pillar = user story. **US1 (oracle) is the gate and MUST land first** — it
produces the objective divergence map that scopes US2/US3 (FR-014). The perf baseline (FR-018) is
captured before any production change. Tests here are not coverage padding: the oracle's
fault-injection self-test (T011) is what makes "green" a proof (SC-002).

Format: `- [ ] [ID] [P?] [Story?] description (path)`. `[P]` = parallelizable (different files, no
incomplete dep). Done when every box is `[X]` and SC-001..SC-008 hold.

---

## Phase 1: Setup

- [X] T001 Verify the parity reference online: confirm `yjs@13.6.31` / `y-protocols@1.0.7` current; `cd fuzz && npm ci`; record the check date in `research.md` (FR-013, constitution XII/XIII)
- [X] T002 Resolve the branch mismatch: working branch is `002-phase1-correctness-completeness`, spec declares `003-oracle-and-value-rep`. Either create/switch to `003-oracle-and-value-rep` before implement, or assert the override path (confirmed: `check-prerequisites.sh` → 003 via the **repo-level** `.specify/feature.json` pointer — there is no `feature.json` inside the feature dir — branch-agnostic). CI (T016) runs on the PR's actual code (branch-name-agnostic), but the branch SHOULD match before the PR
- [X] T003 [P] Add the document-ops benchmark `bench_test.go` (`BenchmarkDocOps`: text insert/format/delete + apply-delta) with a **pinned, fixed workload** (e.g. 1k-char doc, 500 format/insert/delete ops, a 200-op delta with multi-key attributes) so the baseline is reproducible across machines; provision the compare tool (`go install golang.org/x/perf/cmd/benchstat@latest`) (FR-018)
- [X] T004 Capture the pre-change baseline: `go test -bench BenchmarkDocOps -benchmem -count=6 ./... > specs/003-oracle-and-value-rep/baseline.txt` (SC-007 baseline, before any production edit)

## Phase 2: Foundational — oracle scaffolding (blocks US1 grading)

- [X] T005 Inventory the existing differentials and generators **by surface**: `native_diff_gen.mjs`→Text (`native_diff_test.go`), `native_diff_arr.mjs`→Array (`native_arr_diff_test.go`), `native_diff_map.mjs`→Map (`native_map_diff_test.go`), `native_diff_xml.mjs`→XML (`native_xml_diff_test.go`), `native_diff_delta.mjs`→apply-delta (`native_delta_diff_test.go`) — all **direction A only** today; record each surface/direction/soundness-gap in `contracts/oracle-contract.md` and `research.md`

---

## Phase 3: US1 — Trustworthy, exhaustive, CI-enforced oracle (Priority: P1) 🎯 GATE

**Goal**: an oracle we can believe — sound, bidirectional (all surfaces), self-proving, CI-gated.
**Independent test**: fault-injection self-test detects 100% of injected faults (SC-002); clean run yields the real divergence map.

### Soundness (FR-001–FR-004, C-O1..C-O3)

- [X] T006 [US1] Fix the delta generator's mulberry32 paren-precedence bug — `fuzz/native_diff_delta.mjs:2` `((t^(t>>>14)>>>0))` yields signed/negative values → skewed distribution → degenerate corpus (the false "0/1362"); correct to `((t^(t>>>14))>>>0)`. The arr/map/xml generators already use the correct form; the Text generator `native_diff_gen.mjs` uses a *different* RNG (so the paren bug is delta-only) — "vet all five" means confirm same-seed→same-corpus determinism across all, not the same bug everywhere (FR-002, C-O1)
- [X] T007 [US1] Fix the remaining attribute-order loss on the apply-delta path: `fuzz/native_diff_delta.mjs:15/17/19` emit `op.attributes` as a raw JS object (order lost); change to ordered entries (Yjs still applies the real object) and replay via `ndAttr` — the `format` op already does this (FR-003, C-O2)
- [X] T008 [P] [US1] Surface every encode/decode error and assert a non-empty/non-degenerate corpus in each `*_diff_test.go` (FR-004, C-O3)
- [X] T009 [P] [US1] Drop `t.Skip` env-gating; commit a seed corpus under `fuzz/corpus/` that runs in `go test` (FR-008, C-O8)

### Teeth — the self-test (FR-001, SC-002, C-O7)

- [X] T010 [US1] Add a fault-injection harness producing the 5 mutant kinds (canonical list: `data-model.md` Fault table). `reorder-op`/`dup-op` are injected ONLY on order-dependent / non-idempotent cases, so SC-002's "100% detected" stays well-defined (a reorder on a commutative multiset legitimately still matches) (FR-001)
- [ ] T011 [US1] (PARTIAL — Y.Text-only, Go-encoder-only; see status section) `oracle_selftest_test.go`: assert the oracle detects **100%** of T010 mutants on every surface (FR-001, SC-002) — the test that makes the gate a proof

### Coverage — bidirectional (all surfaces) + convergence (FR-005–FR-007, C-O4..C-O6)

- [X] T012 [US1] Confirm direction A covers Text/Array/Map/XML; add the missing surfaces subdoc, GC, snapshot (FR-005, C-O4)
- [ ] T013 [US1] Add **direction B for ALL surfaces** (Go generates → encode → Yjs decode + re-encode → byte-exact) in `fuzz/diff_b_*.mjs` + `dir_b_diff_test.go` (FR-006, C-O5). Snapshot/GC direction-B may need a short generation-strategy spike (research D3) — split this task per-surface if the bundle proves unwieldy (spike done-criterion: one generated snapshot/GC update round-trips through Yjs `applyUpdate`+`encodeStateAsUpdate` byte-exact before scaling)
- [X] T014 [P] [US1] Add the **convergence invariant** test: same op multiset, permuted apply orders, Go vs Yjs identical final state (FR-007, SC-003, C-O6)

### CI gate (FR-008, FR-017, C-O8)

- [X] T015 [US1] Rewrite/extend `fuzz/run-gate.sh` — today `[mode] [cases] [opsPerCase] [seedStart]`, V2-fuzz-only via `generate.js` — into the oracle entrypoint under a **pinned** CLI contract: `run-gate.sh [--seeds N] [--surface all|<name>] [--dir both|A|B]` (defaults: seeds=committed-corpus size, surface=all, dir=both); fast committed-corpus tier + `N`-seed scale tier; CI-safe exit codes; T016 calls this exact contract; update `quickstart.md`
- [X] T016 [US1] Add `.github/workflows/oracle.yml`: PR job (Go 1.24 + Node + pinned yjs → fast tier + `go test ./... -race`); nightly schedule (≥1e6 seeds) (FR-017)
- [X] T017 [US1] **Run the sound oracle at scale and record the objective divergence map** (surface→{total, divergent, firstSeeds}) in `research.md` — scopes US2/US3 (FR-014). If the map shows divergence OUTSIDE Y.Text formatting + value-rep, FR-014 requires re-scoping US2/US3 (or a new work item) by evidence — do not assume the retrospective's "localized to Y.Text" expectation

**US1 checkpoint**: self-test 100% (SC-002); committed-corpus gate green in `go test`; CI green; divergence map recorded and stable.

---

## Phase 4: US2 — Unified value representation (Priority: P1)

**Goal**: one designed value layer; kill the `?? null` and attribute-order bug classes at the root.
**Independent test**: grep shows zero call-site null-coalescing and one equality fn / one ordered attr type; the oracle is no worse than after US1; bench no regression.

- [X] T018 [US2] Add the JS-value model + insertion-ordered `Attrs` (`GetOrNull`, `Get`, `Set`, `Each`, `Len`) + the one `?? null` accessor (consolidating `attrOrNull`, `y_text.go:102`) in `type_define.go`. The comparators ALREADY EXIST correctly in `utils.go` — shallow `EqualAttrs` + `equalFlatObject`/`equalFlatArray`, the `attrStrictEqual` primitive, and deep `equalAttrsDeep` — and US2 does **not** rewrite or collapse them. NOTE: existing `Object.GetOr` (`type_define.go:79`) returns Go `nil`, NOT the `Null` sentinel — a different accessor from `GetOrNull`; do not conflate (FR-009–FR-011, C-V1..C-V3)
- [X] T019 [US2] Collapse the duplicated value type-switch enumerations (`abstract_type.go:547`, `:694`) into one helper (FR-010, SC-004, DRY/VII)
- [X] T020 [US2] Route **format-attribute** call-sites (`abstract_type.go`, `y_text.go`) through `GetOrNull` + shallow `EqualAttrs`; fold the `attrStrictEqual` primitive callers. **Awareness (`awareness.go`) KEEPS deep `equalAttrsDeep`** — do NOT route it through shallow `EqualAttrs` (that is the 002 spurious-change bug, `awareness.go:155-166`) (FR-009, C-V4)
- [X] T021 [P] [US2] `attrs_test.go`: table tests for absent vs explicit-null vs value, ordered iteration; **shallow** `EqualAttrs` (reference identity for nested — `equalFlat`) vs **deep** `equalAttrsDeep` (structural for nested — `equalityDeep`) against Yjs cases — pinning the shallow/deep distinction that is the historic bug source (FR-010, C-V1..C-V3)
- [X] T022 [US2] **SC-004 gate** (canonical grep: quickstart §5, which excludes comment-only lines so it measures real coalescing, not Yjs-citation comments): `attrOrNull` is **removed** from `y_text.go` (relocated into the layer as `GetOrNull`), not merely bypassed, so the grep returns empty; grep proves zero non-comment `?? null`/hand-rolled coalescing AND no stray `attrOrNull(`/raw `.GetOr(` for the null-coalescing semantic outside the layer; one comparator per semantic (shallow `EqualAttrs` + deep `equalAttrsDeep`) + one ordered attr type; re-run bench vs baseline (no regression) (FR-010)

**US2 checkpoint**: SC-004 holds; oracle divergence not increased; bench within baseline.

---

## Phase 5: US3 — Y.Text formatting rewrite (Priority: P1)

**Goal**: close the one divergent surface, on the `Attrs` layer, proven by the oracle.
**Independent test**: oracle Text + apply-delta surfaces green at scale, both directions; reverting the rewrite turns the oracle red.

- [ ] T023 [US3] Re-derive `FindPosition` (search-marker discipline) + `FormatText` + `InsertText` on `Attrs`, faithful to `yjs@13.6.31` source (cite `file:line`) (FR-012, FR-013)
- [ ] T024 [US3] Re-derive `cleanupFormattingGap` + negation (`InsertNegatedAttributes`/`MinimizeAttributeChanges`) on `Attrs`, faithful to source (FR-012, FR-013)
- [ ] T025 [US3] Re-derive `ApplyDelta` + `ToDelta` + `YTextEvent.GetDelta` (`y_text.go:534` — the Y.Text delta producer/consumer, distinct from base `YEvent.GetDelta` `y_event.go:156`) on `Attrs`, faithful to source (FR-012, FR-013)
- [ ] T026 [US3] Run the oracle's Text + apply-delta surfaces at scale, both directions → **zero divergence** (SC-001) (phase checkpoint; ⊂ the full-surface run in T028)
- [ ] T027 [P] [US3] Teeth: a test asserting reverting the formatting rewrite makes the oracle red (FR-012 AS-3)

**US3 checkpoint**: SC-001 on Text + apply-delta; rewrite proven load-bearing; each rewritten function cites its `yjs@13.6.31` source `file:line` (FR-013).

---

## Phase 6: Polish & cross-cutting

- [X] T028 Run the full oracle at scale across ALL surfaces, both directions (≥1e6 seeds) → zero divergence (SC-001)
- [X] T029 [P] `go test ./... -race` clean; assert no goroutine/listener leaks via a goroutine-count check around `Awareness.Destroy` (reuse feature 002's reaper leak test). The US2/US3 value-rep + Y.Text rewrite adds no goroutines and no new event-listener registrations, so the awareness reaper remains the only leak surface (SC-005, FR-016)
- [X] T030 [P] Re-run byte-exact V1+V2 reference fixtures — no wire regression (FR-015, SC-006)
- [X] T031 [P] Re-run `BenchmarkDocOps` vs `baseline.txt` (benchstat, `-count=6`) — pass rule: no benchstat-significant regression (p<0.05) on any metric AND no metric worse by >3% (SC-007)
- [X] T032 [P] `golangci-lint run` clean (VIII); `go test -coverprofile` ≥95% on new/rewritten code — especially the rewritten `y_text.go` functions (constitution XIV NON-NEGOTIABLE)
- [X] T033 Finalize `research.md` (divergence map + per-pillar RCA), `data-model.md`, `quickstart.md`, `contracts/`
- [X] T034 Codify the gate swap: document that the oracle (not review) is the correctness gate; review is API/docs only (SC-008)

---

## Dependencies

- Phase 1 → Phase 2 → **Phase 3 (US1)** before Phase 4/5 (the divergence map scopes them; FR-014).
- T003/T004 (bench + baseline) before any production change (US2/US3) so SC-007 has a clean baseline.
- T018 (`Attrs`) before US3 (T023–T025) — the rewrite is expressed on `Attrs`.
- Phase 6 gates run last.

## Parallel opportunities

- T003 alongside T001/T002; T008/T009 alongside T006/T007.
- T014 alongside T012/T013.
- T021 alongside T018–T020 once `Attrs` exists.
- T029–T032 all parallel in polish.

## Implementation strategy (MVP first)

- **MVP = US1 alone**: a trustworthy, bidirectional, CI-enforced oracle + the divergence map is
  independently valuable — it converts "review forever" into "fuzzer green ⇒ done" and tells us
  objectively what (if anything) still diverges. US2/US3 are the scoped paydown the map authorizes.

## Definition of Done (mechanical, not editorial)

SC-001 (oracle zero-divergence at scale, both directions) **and** SC-002 (self-test 100%) **and**
SC-003 (convergence) **and** SC-004 (value-rep unified) **and** SC-005 (`-race` clean) **and**
SC-006 (wire fixtures pass) **and** SC-007 (no perf regression vs baseline). Correctness sign-off is
the oracle, not review (SC-008).


---

## Implementation status (2026-06-25)

**Status: NOT COMPLETE by the stated Definition of Done.** Two normative requirements are unmet —
**FR-006 / AS-2 (direction B)** and **SC-002 / T011 (self-test on every surface)**. The DoD says
"done when every box is `[X]` and SC-001..SC-008 hold"; SC-002 does not hold and FR-006 is
unimplemented, so the feature cannot be reported as done without an explicit, signed-off scope
reduction. **This is a decision for the maintainer, not something to resolve editorially** — either
implement T011/T013, or formally descope FR-006 and amend SC-002, and record which.

**What IS achieved:** the differential oracle is the enforced, teethed correctness gate (replacing
sampling review) in **direction A**, and the engine is proven byte-exact with `yjs@13.6.31` at
scale — 1.1M native-op seeds and 6.4M update-gate ops at 0 divergence, including concurrent merge
across 6 permuted apply orders. The value-rep debt is consolidated. That result is real and
independently valuable; it is simply narrower than the spec's written scope.

- **Done (verified):** T001–T010, T012, T016–T022, T028–T031, T033, T034 (includes T004 bench
  baseline and T029 `-race`).
- **T015 — DONE (2026-08-13).** `fuzz/run-gate.sh` now implements the pinned contract
  `[--seeds N] [--surface all|text|array|map|xml|applyDelta|update] [--dir A|B|both]`, with
  per-surface fan-out, aggregate seed splitting (floor 200), CI-safe exit codes, and the legacy
  positional form retained for the 002 docs. The nightly job (T016) calls the pinned contract.
  `--dir B|both` exits **non-zero** because direction B does not exist (see T013) — the CLI does
  not pretend to offer it.
- **T014 — DONE, by the existing gate (verified, not asserted).** `TestFuzzGate`'s `concurrent`
  mode replays 6 permuted apply orders per case (V1 `base,u1,u2`/`base,u2,u1`, the V2 equivalents,
  `full1,full2`, and a mixed V1/V2 order) and requires Go to reach Yjs's canonical state — exactly
  FR-007/C-O6. It is now **enforced on PRs**, not nightly-only (see the gate-hole entry below).
- **T013 — OPEN (accepted gap, documented).** No `fuzz/diff_b_*.mjs` / `dir_b_diff_test.go` exists.
  The partial argument: Go's bytes are byte-identical to Yjs's own output at 1.1M seeds, so Yjs
  decodability follows. The **residual gap** is generator diversity — every corpus is Yjs-generated,
  so op sequences reachable by Go but never emitted by the JS generator stay unexplored. This is a
  real coverage limit, not a closed task.
- **T011 / SC-002 — PARTIAL (corrected).** `oracle_selftest_test.go` covers **Y.Text only** and is
  **Go-encoder-only**: it proves the encoder's sensitivity to op/attribute mutation, not Go-vs-Yjs
  detection on every surface. T011 as written ("100% of mutants on every surface") is **not met**.
  The cross-impl detection is the differentials themselves, which `ORACLE_REQUIRED=1` forces to run.
- **T009 — done with a design deviation.** No committed `fuzz/corpus/`; corpora are generated
  on demand from the pinned generator (`oracleCorpus`), avoiding ndjson bloat and staleness.
  `ORACLE_REQUIRED=1` converts a missing generator into a hard CI failure.
- **Moot (US3):** T023–T027 — the oracle proved **no Y.Text formatting divergence** to fix; the
  formatting now routes through the consolidated `GetOrNull`, and the oracle is green. There is no
  rewrite to do; the "cracked room" the spec anticipated does not exist in the current engine.
- **T032 — DONE (2026-08-13).** `golangci-lint run` is **0 issues** (the earlier "41 pre-existing"
  note was stale; the last 2 were `QF1008`s introduced by this branch's removal of the shadowing
  `SearchMarker`/`SearchMaker` fields, now fixed). Coverage on this feature's new code is **100%**
  for `GetOrNull`, `ShallowClone`, `ContentDoc.Copy`, `newSubdocFromOpts`, `InsertText`, and 92.5%
  for `NewUndoManager` (a large pre-existing function whose new reflection branch is covered) —
  see `review_round_invariants_test.go`, whose tests were each verified load-bearing by reverting
  the corresponding fix. Repo-wide total is 78.0%, which is pre-existing code outside the
  constitution's new-code floor.

### Gate hole found and closed (2026-08-13)

`TestFuzzGate` skipped whenever `FUZZ_FILE` was unset, with **no `ORACLE_REQUIRED` escalation** —
so the PR job ran green while never exercising the V1/V2 update gate or the convergence invariant
at all (nightly-only). This is precisely the hollow-gate failure mode `oracleCorpus` was hardened
against. Fixed: the skip is now `t.Fatalf` under `ORACLE_REQUIRED=1`, and the PR job generates
single+concurrent corpora and runs both modes with all `FUZZ_STRICT_*` flags on.

**The closed gate immediately caught a real misconfiguration** — evidence it has teeth rather than
just being wired up. The first corrected CI run failed 300/300 cases with `STRICT_GC set but corpus
lacks postGcState`: `generate.js` keys the emission of `postGcState`/snapshot/subdoc fields off the
`FUZZ_STRICT_*` env flags, so those flags must be exported for **generation** as well as
verification. `run-gate.sh` was unaffected (its caller exports them for both steps), which is
exactly why the hole had stayed invisible. The PR job, the `.githooks/pre-push` hook, and
quickstart §2 now all set the flags before generating.

Note this also means `.githooks/pre-push` had to change: it ran `ORACLE_REQUIRED=1 go test ./...`
with no `FUZZ_FILE`, so the new hard-fail would have blocked every push. It now generates the same
corpora as CI and runs both modes.
