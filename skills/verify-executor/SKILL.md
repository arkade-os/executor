---
name: verify-executor
description: Verify executor's non-interactive boarding, automatic VTXO renewal, automatic VHTLC claims via streaming templates, intent batching near expiry, multi-input delegation, and operator workflows. Select existing daemon and end-to-end scenarios against the local regtest stack and retain evidence.
---

# Verify executor

Run from the repository root. Read `AGENTS.md`, the current diff, and the relevant entries in
[the feature map](features/README.md). Recheck test names and assertions before relying on them.
Use the existing Go harness to drive REST/gRPC and observe transactions through arkd's indexer.
Organize verification around those six features. Registration, fee limits and recovery are
supporting checks within the relevant feature journeys.

## Select the proof

- `make test`: root and template module checks; repository tests require disposable Postgres.
- `test/daemon`: real Postgres and the daemon, with stand-ins for arkd, Electrum and emulator.
- `test/e2e`: the daemon runs **in-process**, with real regtest dependencies and wallet clients.
  This proves API and transaction behavior; it does not exercise `cmd/executord`, environment
  loading, Docker packaging, or browser interactions.
- `standalone`: the Docker image built from this checkout, run as the Compose `executor` service
  against the real regtest dependencies. This proves packaging, environment loading, startup and
  the public/admin port split. It registers no delegation, so it proves no transaction behavior.

Use the helper below for focused daemon/e2e runs. It assigns a unique disposable database,
captures evidence, and cleans up that database even after a test failure. Do not pass a user's
database to these suites: `test/daemon.ResetDatabase` drops the entire public schema.

Choose the smallest set covering the changed behavior and affected callers. Include rejection
and recovery cases when changing fees, signing, forfeits or claims. Existing assertions may not
cover a new requirement: add a regression scenario to the existing harness when necessary.
Keep normal formatting, lint and package-test requirements from `AGENTS.md`.

## Launch

Prerequisites: Go matching `go.mod`, Docker Compose, Nigiri, Python 3, curl and Bash. Run
`docker ps` and inspect container labels before starting anything. Also check for another
`executord`, `go test`, `e2e.test` or `daemon.test` process using the stack.

The Compose file fixes container names and ports, shares the `nigiri` network and overlaps the
emulator repo's stack. A different Compose project name does **not** isolate it. Do not drive
the shared stack concurrently: tests mine blocks and can change arkd fee settings. The helper's
lock coordinates helper runs only; it cannot detect unrelated tests or another user.

When no conflicting stack exists:

```bash
make regtest-up
```

This starts Nigiri and the Compose dependencies, then initializes, unlocks and funds arkd.
`scripts/regtest-init.sh` can also restart emulator; it is not a read-only readiness probe.
Record which resources existed before launch and which this run created. If startup fails,
collect logs and clean up owned resources before retrying. Do not silently repair unrelated
code or replace another checkout's containers.

For daemon-only checks, `make pg` starts Postgres if the external `nigiri` network already exists.
If that network is absent, use `make regtest-up` above first. The helper requires the local
`executor-pg` container exposing Postgres on localhost:5432; it deliberately ignores any ambient
`EXECUTOR_DATABASE_URL` for its test subprocess.

## Doctor

Run this read-only check before driving the stack and whenever a run looks unhealthy:

```bash
skills/verify-executor/scripts/verify.sh doctor e2e
```

Use `doctor daemon` for Postgres alone. The check records images, container ownership labels,
Postgres readiness, Bitcoin's regtest network, arkd wallet status, signer info, and Electrum's
response. Review the images against `docker-compose.regtest.yml` and the ownership paths against
the intended checkout. A running container is not sufficient: arkd must be initialized, synced
and unlocked. A successful doctor does not prove a feature or sufficient wallet funds; the
selected workflow must still finish.

## Drive

Select the feature from the map. Non-interactive boarding and automatic VTXO renewal share this
baseline scenario, with separate observable outcomes documented in their feature entries:

```bash
skills/verify-executor/scripts/verify.sh e2e '^TestBoardingThenRenewal$'
```

The helper launches the test harness, which migrates a fresh database and starts executor on
free public/admin ports. Do not also run `make run` for these tests. Readiness is established by
successful API interactions in the selected scenario; the test must reach its asserted end state.

Use the exact selectors in the feature map for other journeys. Each helper call runs serially,
with `-count=1`, `-json` and a bounded timeout. `e2e '.*'` runs the full e2e suite;
`daemon '.*'` runs the full daemon suite. A selection matching no tests is a failure.

For a change to configuration, startup, the Dockerfile, the Compose file or what either port
serves, also run the packaged daemon:

```bash
skills/verify-executor/scripts/verify.sh standalone
```

The helper refuses to start when a container named `executor` exists or when port 7080 or 7081 is
taken. It builds the image, starts the container on its own disposable database, requires the
responses listed in [operator verification](features/operator.md), then removes the container.

For UI changes, additionally read the browser journey in that entry. Report either surface as
unverified if it was not exercised.

## Evidence

The helper prints a fresh `/tmp/executor-verification.XXXXXX` directory. Retain:

- `revision.txt`, `status.txt`, `diff.patch`, and `command.txt` to identify the code and action;
- `doctor.log`, `tests.jsonl`, `stderr.log`, and timestamp-bounded container logs;
- for `standalone`, `build.log`, `compose.override.yml`, `responses.txt` and `executor.log` in
  place of `tests.jsonl` and `stderr.log`;
- `database.sql` with resulting delegation, coin and attempt rows, before dropping the database;
- `exit-code.txt` and `cleanup.log` with the outcome and teardown result.

Read the selected test's assertions alongside the captured Go events. Check the actual resulting
amounts, destination scripts, attempts and transaction state, not just API success or a final
screenshot. Preserve request/response bodies and before/after screenshots for any additional
manual/API/browser journey. The existing tests do not log every successful assertion or request;
do not claim that the logs contain a complete wire transcript.

Treat dumps and logs as local artifacts: they may contain test secrets and preimages. Do not
publish them automatically. In the final report give the scenario, pass/fail, evidence path,
observed end state, and uncovered surfaces. A skipped, blocked or unmatched test is not proof.

## Cleanup

The helper relies on Go's test cleanup to stop its in-process daemon and closes its own database
with `dropdb --force` after taking a dump. A `standalone` run also removes the `executor`
container it created and keeps the built image. The helper never stops a container it did not
create. Inspect `cleanup.log` and confirm that `tests.jsonl` (`responses.txt` for `standalone`)
and `database.sql` still exist. If interrupted before a dump succeeds,
retain the logs and report the missing evidence; do not report full success.

Leave a pre-existing shared stack running unless the user authorized stopping it. If this run
owns the entire stack, or the user explicitly authorized resetting it:

```bash
make regtest-down
```

This deletes Compose volumes **and** runs `nigiri stop --delete`. Do not use it as routine cleanup
for a borrowed stack. If only some containers belong to this run, stop/remove only their recorded
IDs. Never kill by process name. Preserve the evidence directory through all cleanup paths.

If the helper was forcibly killed, inspect its recorded database name and lock PID before
removing a stale lock or dropping its database. Do not remove an active run's lock.

## Maintain

Update the relevant feature entry when routes, test names, prerequisites or observable outcomes
change. Run a mapped journey after changing launch/drive/cleanup instructions. A new workflow
that has not been executed remains unverified, even if its commands look plausible.
