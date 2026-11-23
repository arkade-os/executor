# Operator API and UI

## Sub-features

Health/status, admin authentication, delegation summaries, template management, cancellation
controls, graceful shutdown and browser UI.

## How to get to it (user POV)

The operator visits the private admin UI on port 7081. Health is at `/healthz`, metrics at
`/metrics`, and API detail at `/v1/admin/status`, `/v1/admin/health` and `/v1/admin/delegations`.
Public port 7080 must not expose these admin operations.

## Driving it with the Go harness

```bash
skills/verify-executor/scripts/verify.sh daemon \
  '^(TestStatusAnswersWhileArkdIsUnreachable|TestHealthzFollowsArkd|TestListDelegationsByNextDueWithItsDetail|TestUIIsServedBehindAuth|TestAdminAuthentication)$'
```

Read `test/daemon/operator_test.go` and `admin_test.go`. Require correct health transitions,
accessible status during an arkd outage, delegation ordering/detail and authentication behavior.
The external services here are stand-ins. `TestUIIsServedBehindAuth` verifies HTTP serving and
auth; it does not execute the UI's JavaScript.

For template management and startup defaults:

```bash
skills/verify-executor/scripts/verify.sh daemon '^TestDefaultTemplates$'
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestTemplateRegistry|TestTemplateCannotNameTheCosignerKey)$'
```

Defaults must register once and preserve an operator's disabled-template choice across restart.
The registry journey verifies idempotent registration, invalid-template rejection, refusal of
delegations against a disabled template and refusal to delete a referenced template. A template
naming the executor's cosigner key must be rejected for delegation.

For the limits on what the public port may register and keep:

```bash
skills/verify-executor/scripts/verify.sh daemon \
  '^(TestPublicPortRegistersDocumentsOnlyWhenOpened|TestRegisterDelegationCapsItsVariables|TestRegisterDelegationMakesRoomAtTheCap|TestSpawnedDelegationMakesRoomOnceItsCoinIsSpent)$'
```

The public port must answer `POST /v1/artifact` and `POST /v1/template` with 403 unless
`EXECUTOR_PUBLIC_REGISTRY` is on, while the admin port serves them. At a full cap, a client
delegation that never held a coin must be deleted and a spawned one whose coin is spent must
expire to make room; one holding an unspent coin must stay active.

For delegation controls and graceful shutdown:

```bash
skills/verify-executor/scripts/verify.sh e2e \
  '^(TestCancelStopsRenewals|TestStopWaitsForTheBatch)$'
```

Use `POST /v1/admin/delegation/{id}/cancel` or `/resume`. Cancellation must persist against wallet
re-registration, and resumption must preserve the delegation ID and restart renewal. Shutdown
must wait for the in-flight batch to land. See [automatic renewal](auto-renewal.md) for the
transaction outcomes and recovery checks; these commands exercise the API and in-process daemon.

For standalone configuration or packaging, use the `executor` Compose profile only after
checking port ownership. Assign that container its own disposable database via a temporary
Compose override; the default `executor` database may belong to an existing daemon. The
`make regtest-run` target builds and launches this profile and runs stack initialization. Check
`/healthz` and `/v1/admin/status` on localhost:7081, plus `/v1/info` on localhost:7080, and capture
responses and container logs. This is a separate proof from the in-process tests.

For a browser change, inspect the current `internal/interface/grpc/web/` controls and use an
available browser harness to exercise the actual changed interaction. Capture the action and
its resulting API state as well as screenshots. Use labels and DOM selectors from the current
UI. If no browser harness is available, report browser behavior as unverified.

## Gotchas

Keep the admin listener private. The Compose profile publishes 7081 on loopback and disables
admin authentication for local regtest. Authentication tests use their own configured users.
`make run` only supplies the database and log level; from a clean environment it still needs the
required connection, cosigner and admin-auth variables listed in `internal/config/config.go`.
Do not infer standalone launch readiness from the abbreviated README command alone.
