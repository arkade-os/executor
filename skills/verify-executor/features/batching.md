# Intent batching near expiry

## Sub-features

Wait until close to the earliest VTXO deadline, combine eligible renewal intents in one arkd
session, register only one batch's capacity at a time, split larger groups and report late coins.

## How to get to it (user POV)

Register several delegations that execute through intents, such as several `renew` delegations,
and fund their addresses with VTXOs whose renewal windows overlap. Leave the owners offline.
Executor should wait near the earliest deadline and put the eligible intents in the same arkd
batch when they fit, rather than renew each delegation as soon as it becomes eligible.

Interpret "close to expiry" using `internal/core/domain/execution/plan.go` and
`internal/config/config.go`:

- A template's schedule determines eligibility (`Due`). For `renew`, this is the VTXO expiry
  minus its `renewal_window`.
- A delegation entry becomes due when its last included coin does. Its deadline is the earliest
  included coin's expiry minus `EXECUTOR_RESERVE`, clamped to be no earlier than the entry's due
  time. The default reserve is 15 minutes. Do not expect executor to wait until raw expiry.
- The earliest deadline drives the renewal group. Other renewal entries due by that group time
  can join, even if their own expiries are later. Entries due after it belong to a later group.
- The planner targets settlement at the group time and registration one arkd session duration
  earlier. Capacity overflow moves earlier chunks into preceding sessions, earliest deadlines
  first. `EXECUTOR_INTENTS_PER_BATCH` controls the executor's planned capacity; ensure it fits
  arkd's configured capacity when checking a real stack.

The deadline is a scheduling target, not a guarantee that arkd will select an intent. When a
deadline is already unreachable, executor should use the next feasible session and report late
coins instead of waiting indefinitely.

## Driving it with the Go harness

After the skill's Launch and Doctor steps, run the timing and single-session proof:

```bash
skills/verify-executor/scripts/verify.sh daemon \
  '^TestRenewalsWaitForTheEarliestDeadlineThenShareARound$'
```

Read `test/daemon/batch_test.go`. This scenario uses four separate renewal delegations, staggered
due times, deadlines roughly six to seven seconds after setup, and one-second arkd sessions.
It must prove all of the following:

1. No intent is registered during the initial four seconds, even while coins become eligible.
2. All four intents register together, no earlier than 4.5 seconds after setup, with exactly
   the funded outpoints.
3. All four attempts succeed in one batch and share the same commitment transaction ID.

The daemon harness controls arkd's sessions and pauses selection while inspecting registrations.
The planner's exact target times are checked separately without wall-clock sleeps:

```bash
go test -count=1 -run '^TestPlan' ./internal/core/domain/execution/
```

In `plan_test.go`, inspect `TestPlanHoldsRenewalsInOneGroupAtTheEarliestDeadline`,
`TestPlanSplitsTheGroupByCapacity`,
`TestPlanOfAFutureRenewalDueBeforeTheEarliestDeadlineJoinsItsGroup` and
`TestPlanOfFutureRenewalsFormsAGroupPerFarApartDeadline`. Require registration one session before
the target, inclusion of eligible later-expiring renewals and separate groups for entries whose
due times do not overlap. Capture this command's output alongside the helper's evidence.

For capacity and lateness checks:

```bash
skills/verify-executor/scripts/verify.sh daemon \
  '^(TestOneBatchesWorthIsRegistered|TestGroupOverTheBatchCapacitySplitsIntoBatches|TestLateCoinsAreCounted)$'
```

Seven intents with capacity three must register the earliest three first, forecast groups of
3/3/1, and complete in three batches. Five intents with capacity two must wait before registering,
split across at least three batches and each land before its deadline. An overdue coin must
appear in `Late`; the existing lateness scenario also checks that its batch carries an eligible
later-expiring renewal.

For a real-stack capacity check:

```bash
skills/verify-executor/scripts/verify.sh e2e '^TestManyDelegations$'
```

This funds 128 boarding/renewal pairs and requires the renewed VTXOs to reference at most eight
distinct commitment transactions. It proves batching with real dependencies, but does not assert
that all 128 fit in one session or that registration occurs near expiry.

For each run, retain the input outpoints, delegation/template IDs, eligibility and expiry times,
reserve, session duration, configured capacities, registration observations and succeeded attempt
commitment IDs. Use the test fixtures/assertions with the helper's logs and database dump; inspect
`GET /v1/admin/status` for the queued/registered counts, batch forecast and late count. A forecast
alone is insufficient: the actual successful attempts must demonstrate the expected grouping.

## Gotchas

- Multiple renewal delegations are separate template instances and intents. The current timing
  scenario uses one template definition with different variables. It does not prove grouping
  across distinct template IDs. For that claim, extend the harness with compatible proof
  templates and verify shared commitment IDs; report that case unverified until it is exercised.
- The exact single-session timing proof uses stand-ins for external services. The real-stack
  scenario has the weaker eight-batch bound. Report each level accurately; neither alone proves
  strict near-expiry, single-session execution against a real arkd.
- Grouping can renew later-expiring coins before their own deadlines to share the earliest
  eligible batch. Do not demand that every coin separately waits until its own latest deadline.
- A group larger than capacity needs several sessions. If deadlines leave insufficient room,
  assert the late count rather than weakening the capacity or timing assertions.
- Boardings are scheduled promptly at their due time. Offchain stream claims bypass this intent
  batching path. Do not delay either to make them share a renewal batch.
