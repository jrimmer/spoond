# RESULTS — conformance runs

Each section records one run's performance numbers against the unit's
budgets. Full pass/fail lists live in the run's JSON under
`conformance/results/` on vm2 (git-ignored).

## 2026-09-30 — U08 staging (E2B substrate, vm2)

Run against `spoond-backend-staging` (https://vm2.lacy.casa:18890) from
`/root/src/spoond` on `feat/e2b-substrate` after U08, with
`CONFORMANCE_SUBSTRATE=e2b`, `CONFORMANCE_GUEST_SERVICE=10.1.0.11:18891`,
`POOL_SIZE=0`. All 14 selected tests pass (L1–L6, S1–S4, D1–D2, I1–I2)
in 454.5 s.

Budgets (U08 §Done when, budget create_p95_ms ≤ 2000):

| Metric | Value | Budget | Result |
|---|---|---|---|
| S4 `create_p50_ms` | 61 | — | OK |
| S4 `create_p95_ms` | 69 | 2000 | OK |

Context (not budgeted):

- L1 create_ms per image: dev-base 67, elixir-base 69, elixir-release
  77, go-base 75, llm-review 69, py-base 67, scylla 90.
- S1 suspend 537 ms, resume 57 ms.
- S2 clone (running sandbox): 120.8 s.
- S3 fork to 8: 121.3 s.
