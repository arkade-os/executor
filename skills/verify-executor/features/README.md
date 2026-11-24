# Executor feature map

Read only the entries relevant to the change. Commands run from the repository root after the
skill's Launch and Doctor steps. The helper supplies a disposable database and retains evidence.

| Feature | Primary proof | Read when changing |
| --- | --- | --- |
| [Non-interactive boarding](non-interactive-boarding.md) | Onchain funding becomes a VTXO while the owner is offline | Registration, chain discovery, boarding, recovery |
| [Automatic VTXO renewal](auto-renewal.md) | Delegated VTXOs renew within the owner's terms | Scheduling, batching, assets, fees, recovery |
| [Automatic VHTLC claims via streaming templates](auto-claim-vhtlc.md) | Funding triggers a child delegation and receiver payment | Streams, sealing, covenant enforcement |
| [Intent batching near expiry](batching.md) | Eligible renewals wait near the earliest deadline and share a batch | Scheduling, reserve, session timing, intent capacity |
| [Multi-input delegation](multi-input-delegation.md) | Every input of one template spends in one transaction | Registration, tuple selection, maturity, completion |
| [Operator](operator.md) | Health/API checks, separate browser journey | Admin API/UI, configuration, packaging |

Registration, fee limits, cancellation and recovery are supporting checks in these six features.
The boarding and renewal entries share `TestBoardingThenRenewal`; run it once when covering both
and report the boarding and renewal outcomes separately.

Coverage is per journey and surface. Passing one registration route does not verify boarding,
gRPC parity, or the browser; passing daemon stand-ins does not verify external integration; the
`standalone` smoke in the operator entry does not verify a delegation journey.
The templates registered at startup are `renew`, `board` and `claim`. An onchain template
registered by its document is watched and signed the same way; the daemon onchain tests use
stand-ins for arkd, Electrum and the emulator.
