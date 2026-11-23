# executord

Alpha software: use at your own risk.

`executord` renews Arkade VTXOs and boards bitcoin while their
owner is offline. A wallet registers a delegation on the addresses of its
template inputs; every spend is co-signed by the
[emulator](https://github.com/arkade-os/emulator) only if it satisfies the
template's covenant.

The default templates in [`templates/`](templates) (`renew`, `board` for non-interactive boarding, and the VHTLC claim stream `claim`) are registered at startup.

## Run

```sh
make regtest-up   # nigiri, arkd, emulator, postgres
make run          # UI on http://localhost:7081/
make test         # unit tests
make test-e2e     # end-to-end tests on the regtest stack
make regtest-down
```

In production use `ghcr.io/arkade-os/executor` with the variables below.
Port 7080 is the public API
([service.proto](api-spec/protobuf/executor/v1/service.proto)), 7081 the
admin API and UI: keep it private. Back up the cosigner and encryption keys.

Artifacts and templates are registered on the admin port (`POST /v1/artifact`,
`POST /v1/template`): the public port answers them with 403 unless
`EXECUTOR_PUBLIC_REGISTRY` is `true`. Reading them stays public.

## Configuration

| Variable | | Default |
|---|---|---|
| `EXECUTOR_ARK_URL` | arkd gRPC address | required |
| `EXECUTOR_EMULATOR_URL` | emulator gRPC address | required |
| `EXECUTOR_ELECTRUM_URL` | Electrum server, `tcp://host:port` or `ssl://host:port` | required |
| `EXECUTOR_DATABASE_URL` | postgres DSN | required |
| `EXECUTOR_COSIGNER_KEY` | private key cosigning arkd's vtxo trees, 32 bytes hex | required |
| `EXECUTOR_ADMIN_AUTH` | `file` or `disabled` | required |
| `EXECUTOR_ADMIN_USERS_FILE` | `user:bcrypt` lines | with `file` |
| `EXECUTOR_ENCRYPTION_KEY` | ECIES key for template secrets, hex; a secret is sealed to the key and the id of its template | unset |
| `EXECUTOR_DEFAULT_TEMPLATES` | register the default templates at startup | `true` |
| `EXECUTOR_MIN_DELEGATION_EXPIRY` | minimum lead of a delegation's expiry | `24h` |
| `EXECUTOR_EXPIRY_INTERVAL` | how often delegations past their expiry are marked | `1m` |
| `EXECUTOR_OFFCHAIN_CONCURRENCY` | offchain attempts in flight at once | `8` |
| `EXECUTOR_ATTEMPT_TIMEOUT` | building and signing an offchain attempt, before arkd is called | `1m` |
| `EXECUTOR_RETRY_DELAY` | before the coins of a failed attempt are tried again | `1m` |
| `EXECUTOR_INTENTS_PER_BATCH` | intents arkd takes per batch: at most that many are kept registered | `128` |
| `EXECUTOR_INTENT_TIMEOUT` | an intent no batch selected by then is deleted from arkd and queued again | `5m` |
| `EXECUTOR_BAN_DURATION` | how long the coins of an intent a batch failed on keep out of the queue | `5m` |
| `EXECUTOR_LANDING_WINDOW` | after a restart or the loss of arkd's batch events, how long an attempt whose forfeits may have reached arkd waits to see its coins spent before it is failed | `5m` |
| `EXECUTOR_RESERVE` | latest a coin enters a batch, before its vtxo expires | `15m` |
| `EXECUTOR_TEMPLATE_MAX_FAILURES` | consecutive failed attempts that disable a template | `10` |
| `EXECUTOR_LOOP_STALL` | how long the loop may stay silent past a due time, or arkd's batch events stay lost, before `/healthz` reports it | `2m` |
| `EXECUTOR_PUBLIC_RATE_LIMIT` | requests per second per client address on the public port, loopback exempt; `0` disables | `5` |
| `EXECUTOR_PUBLIC_REGISTRY` | let the public port register artifacts and templates, as the admin port does | `false` |
| `EXECUTOR_MAX_DELEGATIONS` | active delegations cap | `500000` |
| `EXECUTOR_MAX_STREAM_CHILDREN` | active spawned delegations cap, apart from `MAX_DELEGATIONS` | `10000` |
| `EXECUTOR_MAX_VARIABLES_BYTES` | what the variables of one client delegation weigh together at most; `0` disables | `4096` |
| `EXECUTOR_IDLE_DELEGATION_EXPIRY` | how long after registration a client delegation that never held a coin expires; `0` disables | `168h` |
| `EXECUTOR_EVICT_IDLE_AFTER` | at `MAX_DELEGATIONS`, how long after registration a client delegation that never held a coin is deleted to make room; `0` disables | `1h` |
| `EXECUTOR_PRUNE_AFTER` | how long after expiry a delegation a stream spawned, or a client one that never held a coin, is deleted | `720h` |
| `EXECUTOR_LOG_LEVEL` | logrus level | `4` |

The delegations a stream spawns count only toward `MAX_STREAM_CHILDREN`, the others toward `MAX_DELEGATIONS`.
A registration that meets a full cap first makes room among the delegations serving no coin, a
hundred at most: the oldest client ones that never held a coin are deleted, or spawned ones whose
coins have been spent, or never showed, for one `EXPIRY_INTERVAL` are expired. It is refused only
when none qualifies. A delegation holding an unspent coin is never removed this way.

Renewals are grouped. A coin's deadline is the later of its due time and its
expiry minus `RESERVE`. The earliest deadline forces a group, which carries
every renewal due by then, so they share one batch; the entries of a group are
registered one batch before it. A group over `INTENTS_PER_BATCH` splits into
consecutive batches ending at its time, the earliest deadlines first. A boarding coin
is registered at once. The reserve must be at least twice `INTENT_TIMEOUT`: one
intent that times out and a retry. The admin `GET /v1/admin/status` shows the
queue, what is registered, the coins forecast to miss their deadline and the
batches the plan expects.

When arkd ends the batch event stream, the daemon subscribes again after a second, then twice as
long after each refusal, up to thirty seconds. The attempts of the batch in flight wait
`LANDING_WINDOW` to see their coins spent, as after a restart. The `loop` check of `/healthz` fails
once the events are lost for `LOOP_STALL`.

A template whose attempts arkd, the emulator or the template itself
refuse `TEMPLATE_MAX_FAILURES` times in a row is disabled; a succeeded attempt
resets the count. A coin arkd refuses for its state (spent, swept, banned,
already in an intent, an amount outside its limits) does not count, nor does a
coin of a spawned delegation that the template itself refuses: a stranger funds
it and chooses what the stream captures. The
templates registered at startup are `renew`, `board` and `claim`. An onchain template is registered by its document like any other: the
daemon watches its input addresses and the emulator signs the spend.

Other limits and intervals are in [`internal/config/config.go`](internal/config/config.go).

## Streams

A template with a `trigger` is not registered by clients: the operator registers it as a stream (admin `RegisterStream`, `ListStreams`, `CancelStream`; a default with a trigger starts by itself when an encryption key is configured, and stays cancelled once the operator cancels it). The daemon reads arkd's transaction stream and, for every transaction whose extension holds the trigger's packet, captures the trigger's variables, checks the template locks the output, and spawns a delegation of the template that expires after `expires_after_seconds`. When several delegations share a script, the coins go to the one a stream spawned, otherwise to the oldest.

The `claim` default does not take a bare preimage. The receiver seals its terms to the daemon's
encryption key and the template id: the 32-byte preimage, the 32-byte program to pay, then the
least amount as a script number. The funder, who holds that ciphertext before funding, puts it in
the packet; the daemon claims only an output that pays that program at least that amount, so a
decoy of the same hash paying someone else, or less, leaves the preimage sealed.

