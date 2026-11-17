# Automatic VHTLC claims via streaming templates

## Sub-features

Automatic discovery and child delegations, receiver payment, template-bound secrets and sealed
payment terms.

## How to get to it (user POV)

Fund a matching VHTLC with the sealed secret packet in the transaction. The configured claim
stream discovers it and creates the claim delegation without a separate wallet registration.

## Driving it with the Go harness

```bash
skills/verify-executor/scripts/verify.sh e2e '^TestStreamClaim$'
```

Read `test/e2e/swap_test.go`. The `claim` stream must discover the funding transaction and spawn
its own child delegation without a separate wallet registration. It must pay the receiver
10,000 sats, expose the expected preimage and record one succeeded attempt.

For sealing or covenant changes:

```bash
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestStreamClaimRefusesASecretSealedToAnotherTemplate|TestStreamClaimEnforcesTheSealedTerms)$'
```

A secret sealed to a different template leaves funding unspent and pays nothing; a valid control
must still succeed. Sealed amount and receiver violations likewise leave their funding unspent.

## Gotchas

The harness configures an encryption key. A standalone executor without `EXECUTOR_ENCRYPTION_KEY`
cannot reproduce that setup. `TestStreamClaimEnforcesTheSealedTerms` registers the
`offline_receive.json` fixture and a stream explicitly; it does not prove that fixture is a
default startup template. Use the packet/sealing helpers rather than fabricated preimages or
directly inserted child rows. Keep test secrets in local evidence only.
