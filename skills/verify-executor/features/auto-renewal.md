# Automatic VTXO renewal

## Sub-features

Renewal while the owner is offline, asset preservation, batching, owner fee limits and recovery.

## How to get to it (user POV)

Register a `renew` delegation for the owner's variables and maximum fee, then receive VTXOs at
the address of its `funds` input, directly or through
[non-interactive boarding](non-interactive-boarding.md).
Executor renews the VTXOs when due without the owner participating in each batch.

## Driving it with the Go harness

```bash
skills/verify-executor/scripts/verify.sh e2e '^TestBoardingThenRenewal$'
```

Read `test/e2e/boarding_test.go`. After boarding, the same delegation must discover a renewed
VTXO at the same script. The admin summary must report one coin, 20,000 sats and a nonzero next
due value. `landed` correlates succeeded attempts with indexer commitment transaction IDs.

For fee and asset changes:

```bash
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestAssetRenewal|TestIntentFees|TestFeeDropUnblocksRenewal)$'
```

Asset identifiers and amounts must survive renewal. In `test/e2e/fees_test.go`, a permitted
100-sat fee yields `delegateAmount - 100`; a zero-cap delegation records refusal and leaves its
input unspent. Clearing the fee must unblock that renewal and preserve the full delegated amount.

For scheduling, cancellation or recovery changes:

```bash
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestCancelStopsRenewals|TestRestart|TestRestartMidRenewal)$'
```

Read `test/e2e/lifecycle_test.go` and `restart_test.go`. Cancellation leaves the coin unspent
during the observation window; wallet registration cannot override it. Resumption retains the
delegation ID and renews. Restart must resume work from persisted delegations. Recovery after
forfeits must record the renewal once and leave one unspent output of the full 20,000 sats.

For timing and capacity changes, follow [intent batching near expiry](batching.md). It verifies
that eligible renewals wait near the earliest deadline and share a session, with separate checks
for groups that exceed batch capacity.

## Gotchas

Fee tests change shared arkd policy and clear it in cleanup. Require exclusive use of the stack.
They restore their test baseline, not arbitrary previous settings; capture and restore a borrowed
stack's original policy or use an independently owned stack. Refusal alone is insufficient:
check the input remains unspent. Cancellation assertions cover a bounded interval, and crash
tests inject failures at the arkd client boundary rather than killing an OS process.
