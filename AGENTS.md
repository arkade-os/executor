# AGENTS.md

executord renews Arkade VTXOs and boards bitcoin while their owner is offline. A wallet registers
a delegation on the addresses of its template inputs; every spend is co-signed by the
[emulator](https://github.com/arkade-os/emulator) only if it satisfies the template's covenant. The
default templates in [`templates/`](templates) (`renew`, `board` for non-interactive boarding, and
the VHTLC claim stream `claim`) are registered at startup. An onchain template is registered by
its document like any other: the daemon watches its input addresses and the emulator signs the
spend. Port
7080 is the public API, 7081 the admin API and UI: keep it private. Treat signing, forfeit, sweep
and fee code as code that moves money. When your Ark knowledge disagrees with the repo, trust the
code but if you think the code is buggy and conflicts with repo intentions then ask a human. For
terms like VTXO, batch, intent, forfeit or boarding, see the
[Arkade glossary](https://docs.arkadeos.com/learn/glossary).

## Repository layout

```
cmd/                         executord entry point
api-spec/                    protobuf and OpenAPI specs, generated code in protobuf/gen and
                             openapi/swagger
internal/config/             env-based configuration (EXECUTOR_* variables)
internal/core/domain/        models and sentinel errors, imports nothing from internal/ outside
                             domain/
internal/core/application/   business logic (delegation, execution, registry, health), imports only
                             domain and ports from internal/, anything else from pkg/ or
                             external deps
internal/core/ports/         interfaces the application uses (chain, indexer, signer...)
internal/infrastructure/     implementations of ports, one folder per dependency (arkd, emulator,
                             electrum, postgres)
internal/interface/grpc/     gRPC handlers, also served as REST, plus the admin web UI in web/
pkg/template/                the template engine (artifacts, covenants, ECIES sealing), own go.mod
templates/                   the default templates, embedded and registered at startup
test/                        daemon, e2e and scale suites against the regtest stack
scripts/                     regtest-init.sh
```

## Commands

Build, test, lint and codegen commands are Makefile targets; the `##` comment above each target
says what it does. The Run section of `README.md` covers local setup and the e2e flow
(`make regtest-up`, `make run`, `make test-e2e`). For a quick loop run one test with
`go test -run '^TestName$' ./path/to/pkg/` — most packages need no postgres; the postgres tests
need `make pg` first (CI sets `EXECUTOR_DATABASE_URL` instead).

For workflow verification, read [`skills/verify-executor/SKILL.md`](skills/verify-executor/SKILL.md).

## Linting

`make lint` runs golangci-lint with `.golangci.yml` in both the root module and `pkg/template`;
`make lint-fix` fixes what it can. CI runs the same checks — `make fmt` must leave no diff and the
linter runs on both modules — so run them before you commit.

- Two modules are linted: the root and `pkg/template`. `api-spec` has its own `go.mod` and is not
  linted; follow the same rules there by hand.
- The `gofmt`, `goimports` and `golines` formatters are enabled, with lines capped at 100 chars.
  Wrap comments, strings and test code yourself. Wrap long calls and signatures yourself before
  running the linter. Fill each line up to the limit, instead of putting one argument per line as
  JS formatters do. `golines` also splits one argument per line, so don't leave the wrapping to it.
- Generated code is excluded from lint: `api-spec/protobuf/gen` and
  `internal/infrastructure/postgres/sqlc`.

## Code layout

Order a non-test Go file top to bottom like this.

1. Types, interfaces, constants and vars.
2. The factory function (`NewService`, `Open`...).
3. Exported methods.
4. Unexported methods.
5. Util functions and structs.

`internal/infrastructure/postgres/postgres.go` is a good reference.

- Match the style of the file you edit. Don't reformat or rewrite code the change doesn't need.
- Errors that reach clients map to a gRPC code through `toStatus` in
  `internal/interface/grpc/handlers/artifact.go`: domain and application code return sentinel
  errors (`Err...`, e.g. `delegation.ErrDelegationNotFound`) and `toStatus` translates them. Reuse
  an existing error and code when one fits, and add a new sentinel error plus a `toStatus` case
  only when none does. Don't send a bare `fmt.Errorf` through the API: an unmapped error becomes
  `codes.Internal` with a generic message while the real one is logged.
- Return errors. Don't add a `panic` to non-test code.
- Write no comment unless the [Comments](#comments) rules keep it.
- Never hand-edit generated code in `api-spec/protobuf/gen`, `api-spec/openapi/swagger` or
  `internal/infrastructure/postgres/sqlc`. Edit the source and regenerate (`make proto`,
  `make sqlc`).

## Comments

Write no comment by default: names, types and structure carry the meaning. When code needs prose to
be understood, rename, extract or retype it until it does not. The rules cover every comment a
change adds or edits, tests included. Leave the comments of code the change doesn't touch.

A comment stays only when it is one of these.

- A license or legal header.
- A doc comment that defines the contract of an exported identifier.
- Behavior that is not obvious and that we cannot reshape, because an external dependency or a
  protocol forces it: arkd, the emulator, electrum, bitcoin consensus or policy. A surprise in our
  own code is not one: reshape the code.
- A link to a specification (a BIP, an RFC) for a constraint the code cannot express. No ticket
  numbers, PR links or change history.
- A `//nolint` directive, only where the rule is wrong or purely stylistic. Where the rule protects
  correctness or safety, fix the code.

When in doubt, delete the comment.

- Don't narrate what the code does, restate a name or a list in prose, add banners or leave
  commented-out code.
- `IMPORTANT`, `do not remove`, `fine for now` or a long justification don't earn a comment its
  place. Check the claim against the code. Don't shorten such a comment: delete it.
- A comment that excuses a workaround marks code to fix. Fix the cause when it is in the scope of
  the change, else report it.
- A comment that states a constraint (`do not remove`, `keep in sync with`) is better enforced. Ask
  a human before you encode it as a type, a test or a lint rule, then delete it. If it cannot be
  enforced, delete it and report the constraint.

## Unit tests

A `_test.go` file reads top to bottom like this.

1. Package-level vars, constants and types the tests share.
2. The test functions.
3. Helpers and helper structs.

Tests live in the same package as the code, not in a `_test` package.

Inside a test, follow these rules.

- Every `TestXxx` should test exactly one function and `Xxx` should be its name.
- Group cases under named subtests. When possible (most of the time), define fixtures as a list of
  unnamed structs with name, data and expected result or error. `TestBlamed` in
  `internal/infrastructure/arkd/batch_test.go` is a good reference.
- Use `testify/require`. Reuse an existing mock before you write a new one.
- Always use `t.Context()` as context. Use `context.Background()` only in rare, specific cases.
- Use `t.Cleanup` instead of `defer`. Use `defer` only in rare, specific cases.
- A bug fix ships with a test that fails without the fix and passes with it.
- Don't write tests that only restate the implementation. Delete test files that cover nothing new.
- Only report test results you actually ran.
- The postgres tests need `make pg` for a throwaway database. Run them through `make test`, which
  CI runs against a postgres service with `EXECUTOR_DATABASE_URL` set.

## Making a change

- Never assume. Check a claim against the code before you make it.
- Before you add a file, read two neighbours in the same folder and copy their structure. A new
  port implementation should look like the existing ones under `internal/infrastructure/`.
- Read every caller of a function before you change it. Fix a bug in the shared code every caller
  uses, not only in the caller the issue mentions.
- Keep the diff to the task. Unrelated fixes go in their own PR.
- Don't add, remove or upgrade dependencies unless the task needs it.
- Fix the cause of a lint or test failure. Don't disable the linter or weaken the test.
- Protobuf changes must not break clients. Run `make proto-lint`, and regenerate with `make proto`.
- Create db migrations with `make FILE=<name> migration`, so both `up.sql` and `down.sql` exist.
- Read `internal/config/config.go` before changing settings; the Configuration table in `README.md`
  documents the main `EXECUTOR_*` variables.

## Commits and PRs

- Before you commit, run the tests of the packages you touched (`make test` covers the root module
  and `pkg/template`). Run `go mod tidy` (`make deps`) in any module whose `go.mod` you changed —
  root, `pkg/template` or `api-spec`.
- CI fails on a `make fmt` diff. It also runs the linter on both modules, `make test` against
  postgres, the daemon and e2e suites on a regtest stack, and a Trivy scan.
- Sign every commit.
- Keep the commit subject to one short sentence. Recent history uses prefixes like `fix:`, `test:`
  or `docs:` when they help.
- Push with `--force-with-lease`, never `--force`.
- PRs are merged into `master`, and `master` must stay green.
- You cannot work directly on `master`.
