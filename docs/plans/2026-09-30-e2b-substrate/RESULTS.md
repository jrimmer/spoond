# RESULTS — conformance runs

Each section records one run's performance numbers against the unit's
budgets. Full pass/fail lists live in the run's JSON under
`conformance/results/` on the host (git-ignored).

## 2026-09-30 — U08 staging (E2B substrate, host)

Run against `spoond-backend-staging` (https://spoond.example.com:18890) from
`/root/src/spoond` on `feat/e2b-substrate` after U08, with
`CONFORMANCE_SUBSTRATE=e2b`, `CONFORMANCE_GUEST_SERVICE=10.0.0.11:18891`,
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

## 2026-10-01 — U12 precondition: FULL suite, staging (E2B)

All 27 tests PASS (L1–L6, S1–S4, D1–D3, N1–N6, I1–I3 incl. Docker-in-Docker 21.3 s,
R1–R3 with CONFORMANCE_DESTRUCTIVE=1) in 202.6 s against `spoond-backend-staging`
(tip 39636b3). Results JSON: `conformance/results/20261001T005006-e2b-full.json` (host).

| Metric | Value | Budget | Result |
|---|---|---|---|
| S4 `create_p50_ms` | 60 | — | OK |
| S4 `create_p95_ms` | 73 | 2000 | OK |
| `create_ms` max (scylla) | 332 | 5000 | OK |
| `clone_ms` | 541 | 5000 | OK |
| `fork8_ms` | 1046 | 10000 | OK |
| `suspend_ms` | 44 | 15000 | OK |
| `resume_ms` | 65 | 3000 | OK |
| `restart_total_ms` | 6028 | 120000 | OK |

I3 passed (not a known limitation after all). POOL_SIZE for production (per U12 step 4,
the value recorded for S4): **0**.
