# Multi-input delegation

## Sub-features

One template with several inputs, per-input addresses, paired coins, consensus maturity,
one spending transaction, and completion only when that transaction spends every selected coin.

## How to get to it (user POV)

Register a template that declares more than one input. The delegation response lists each input's
name, chain, script and address. `Delegation` has no address. `GET /v1/delegation?address=` still
finds the delegation, and the address may be any of its inputs: an active match wins, otherwise
the newest id. Pay every input. The daemon spends them together once each input has one eligible
coin. A coin at only some inputs waits.

The templates registered at startup stay `renew`, `board` and `claim`. An onchain template is
registered by its document like any other. The daemon watches that template's input addresses and
the emulator signs the spend.

## Driving it with the Go harness

Daemon stand-ins, two inputs each:

```bash
skills/verify-executor/scripts/verify.sh daemon \
  '^(TestTwoVirtualInputsSpendTogether|TestTwoOnchainInputsAreSignedTogether|TestOnchainCoinIsSignedBroadcastThenSucceedsOnceSpent)$'
```

Read `test/daemon/multi_input_test.go`. The virtual journey funds the first input and must not
attempt, then funds the second, submits one virtual transaction over both coins, and succeeds
only after both coins are spent by that transaction. The onchain journey waits until both inputs
are confirmed, broadcasts one signed transaction, keeps input order, leaves the first output at
the first input's amount, and takes the fee from the second input. The third test is the existing
single-input onchain regression.

Repository and domain checks, after `make pg` when Postgres is not already up:

```bash
go test -count=1 -timeout 10m \
  -run '^(TestNewDelegationRegistersEveryInput|TestNewDelegationRefusesTwoSlotsOnTheSameScript|TestNewDelegationRefusesOperatorKeyOnAnOnchainLeaf|TestDelegationGetByAddressFindsAnyInput|TestReadySkipsInfeasibleAndBackedOffTuples|TestReadyWaitsForSequenceAndTxLocktime|TestBuildOnchainTakesTheFeeFromTheNamedInput|TestAttributeFollowsTheSpenderTransaction|TestSettleLeavesADisappearanceSubmitted|TestSettleAdoptsOneObservedSpender|TestSettleFindsAnOnchainSpender|TestRebroadcastSendsOnlyAWitnessTransaction|TestNewTemplateRefusesAllOutputs)$' \
  ./internal/core/domain/delegation/ ./internal/core/domain/execution/ \
  ./internal/core/domain/registry/ ./internal/core/application/execution/ \
  ./internal/infrastructure/postgres/
```

`TestDelegationGetByAddressFindsAnyInput` looks up the second input's address.
`TestNewTemplateRefusesAllOutputs` refuses `all_outputs`. Two slots on one chain with the same
script are refused. An `OPERATOR_KEY` leaf on an onchain template is refused. Relative locktime uses
the coin's confirmed median time (the median time of the block before the coin), and a missing
median time leaves the coin unready. A tuple whose expiry is not after the latest due is skipped
and can be backed off; clearing that backoff makes it eligible again.

## What the daemon requires

A delegation has 1 to 64 inputs and one coin per input, in input order, each matching that
input's script and chain. The same script may be watched on both chains. While an attempt is
unresolved, no other coin of that delegation is selected. The selector keeps the four oldest
eligible coins per input and examines at most 16 tuples. An ineligible, unaffordable or refused
tuple backs off; an outage does not. Fees, when the template has them, come from the named input.
Virtual templates cannot pay fees.

Success is not "the coins disappeared". Every selected coin must be spent by the one transaction
the attempt recorded. A missing spender stays submitted. When every selected coin names the same
spender and the attempt has recorded none, that spender is recorded and the attempt succeeds.
That is how a restart finishes a batch whose commitment was not stored. An onchain coin that left
the unspent set without a spender name is matched to the transaction that spends its outpoint,
and that name is what the restart records. After a restart, a stored
witness transaction is broadcast again; an unsigned or non-witness payload is not. A stream still
matches the trigger input's script, not a particular outpoint, and a template may have only one
trigger.

Attempts stay `submitted`, `succeeded` or `failed`. Prepared, submission-unknown, accepted,
conflicted and conclusively rejected describe the evidence above. They are not stored statuses.

## Gotchas

The daemon emulator signs inputs. It does not run the covenant, so a redirected surplus is not
disproved by these tests. The value check rejects outputs that exceed the inputs. A graceful
`Daemon.Restart` records the in-flight error and does not prove crash rebroadcast; that is the
rebroadcast unit test. `all_outputs`, several trigger rules, and more than 64 inputs are refused.
Several coins in one slot, joins across trigger rules, and reservations shared by more than one
daemon are out of scope. The admin list shows each input address and the detail page shows
per-input funding. Browser use of that UI is a separate check; see [operator](operator.md).
