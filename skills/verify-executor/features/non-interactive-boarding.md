# Non-interactive boarding

## Sub-features

Wallet registration, boarding while the owner is offline, multiple boarding inputs and recovery.

## How to get to it (user POV)

A wallet lists `GET /v1/templates`, selects `board`, and submits `POST /v1/delegation` with its
template ID and owner variables. Send regtest bitcoin to the boarding input's address
(`inputs[0].address`; a delegation has no address of its own) and
leave executor to board it while the owner is offline. The harness also registers a renewal
delegation for the resulting VTXOs; see [automatic renewal](auto-renewal.md).

## Driving it with the Go harness

```bash
skills/verify-executor/scripts/verify.sh e2e '^TestBoardingThenRenewal$'
```

Read `test/e2e/boarding_test.go` and `landed` in `helpers_test.go`. The scenario funds 20,000 sats,
checks that the boarding input address matches the owner's derived address, then observes a boarded
VTXO worth 20,000 sats at the renewal destination. `landed` correlates succeeded attempts with
indexer commitment transaction IDs. The later renewal assertions belong to the renewal feature.

For registration, multiple-input or boarding-recovery changes:

```bash
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestWalletFlowOverREST|TestSeveralCoinsAtOneAddress|TestRestartMidBatch)$'
```

The REST journey in `test/e2e/template_test.go` verifies registration and lookup, invalid-owner
400 responses, missing-template 404 responses, and exclusion of admin routes from the public
port. The multiple-input journey must board separate outputs of 10,000, 10,000 and 30,000 sats.
The recovery journey in `test/e2e/restart_test.go` must record the boarding once and preserve
20,000 sats after a failure injected after forfeits reached arkd.

## Gotchas

These tests faucet and mine real regtest transactions. They need a funded, unlocked arkd wallet
and working Electrum/indexer synchronization. Wait for the existing eventual assertions; do not
replace them with a fixed sleep or accept only a registered intent as successful boarding.
Construct owner variables with the existing wallet helpers. Recovery uses an injected client
failure and an in-process restart; it does not simulate an OS kill or container crash.
