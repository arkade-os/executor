# executord

Alpha software: use at your own risk.

`executord` renews Arkade VTXOs and boards bitcoin while their owner is offline. A wallet registers
a delegation on the addresses of its template inputs; every spend is co-signed by the
[emulator](https://github.com/arkade-os/emulator) only if it satisfies the template's covenant.

The default templates in [`templates/`](templates) are registered at startup: `renew`, `board`
(non-interactive boarding) and `claim` (VHTLC claim stream).

## Run

```sh
make regtest-up   # nigiri, arkd, emulator, postgres
make run          # UI on http://localhost:7081/
make test         # unit tests
make test-e2e     # end-to-end tests on the regtest stack
make regtest-down
```

In production use `ghcr.io/arkade-os/executor` with the variables below, and back up the cosigner
and encryption keys.

- Port 7080 is the public API ([service.proto](api-spec/protobuf/executor/v1/service.proto)).
- Port 7081 is the admin API and UI. It has no authentication, listens on every interface and
  speaks plain HTTP: keep it private, reachable only through a proxy that authenticates its
  clients and terminates TLS.

Artifacts and templates are registered on the admin port (`POST /v1/artifact`,
`POST /v1/template`). The public port only reads them, unless `EXECUTOR_PUBLIC_REGISTRY` is `true`.
On both ports, a REST request other than `GET` or `DELETE` carries
`Content-Type: application/json`.

## Configuration

| Variable | | Default |
|---|---|---|
| `EXECUTOR_ARK_URL` | arkd gRPC address | required |
| `EXECUTOR_EMULATOR_URL` | emulator gRPC address | required |
| `EXECUTOR_ELECTRUM_URL` | Electrum server, `tcp://host:port` or `ssl://host:port` | required |
| `EXECUTOR_DATABASE_URL` | postgres DSN | required |
| `EXECUTOR_COSIGNER_KEY` | private key cosigning arkd's vtxo trees, 32 bytes hex | required |
| `EXECUTOR_ENCRYPTION_KEY` | ECIES key for template secrets, hex | unset |
| `EXECUTOR_PORT` | public API port | `7080` |
| `EXECUTOR_ADMIN_PORT` | admin API and UI port, different from `PORT` | `7081` |
| `EXECUTOR_DEFAULT_TEMPLATES` | register the default templates at startup | `true` |
| `EXECUTOR_PUBLIC_REGISTRY` | let the public port register artifacts and templates | `false` |
| `EXECUTOR_MAX_ARTIFACTS` | cap on registered artifacts; `0` disables | `1000` |
| `EXECUTOR_MAX_TEMPLATES` | cap on registered templates; `0` disables | `1000` |
| `EXECUTOR_MAX_DOCUMENT_BYTES` | size limit of an artifact or template document, a request holding 1 MiB at most; `0` disables | `1048576` |
| `EXECUTOR_PUBLIC_RATE_LIMIT` | requests per second per client address on the public port; `0` disables | `5` |
| `EXECUTOR_MIN_DELEGATION_EXPIRY` | minimum lead of a delegation's expiry | `24h` |
| `EXECUTOR_EXPIRY_INTERVAL` | how often delegations past their expiry are marked | `1m` |
| `EXECUTOR_OFFCHAIN_CONCURRENCY` | offchain attempts in flight at once | `8` |
| `EXECUTOR_ATTEMPT_TIMEOUT` | time to build and sign an offchain attempt | `1m` |
| `EXECUTOR_RETRY_DELAY` | wait before the coins of a failed attempt are retried | `1m` |
| `EXECUTOR_INTENTS_PER_BATCH` | intents arkd takes per batch | `128` |
| `EXECUTOR_INTENT_TIMEOUT` | wait before an intent no batch selected is queued again | `5m` |
| `EXECUTOR_MIN_BATCH_EXPIRY` | shortest batch expiry accepted by executor, a block counting as 10 minutes; `0` disables | `512s` |
| `EXECUTOR_BAN_DURATION` | how long the coins of an intent a batch failed on stay out of the queue | `5m` |
| `EXECUTOR_LANDING_WINDOW` | after a restart or lost batch events, how long an attempt waits to see its coins spent | `5m` |
| `EXECUTOR_RESERVE` | latest a coin enters a batch before its vtxo expires; at least twice `INTENT_TIMEOUT` | `15m` |
| `EXECUTOR_TEMPLATE_MAX_FAILURES` | consecutive failed attempts that disable a template | `10` |
| `EXECUTOR_LOOP_STALL` | how long the loop or arkd's batch events may stay silent before `/healthz` fails | `2m` |
| `EXECUTOR_MAX_DELEGATIONS` | cap on active client delegations; `0` disables | `500000` |
| `EXECUTOR_MAX_STREAM_CHILDREN` | cap on active delegations spawned by streams; `0` disables | `10000` |
| `EXECUTOR_MAX_VARIABLES_BYTES` | size limit of one client delegation's variables; `0` disables | `4096` |
| `EXECUTOR_IDLE_DELEGATION_EXPIRY` | when a client delegation that never held a coin expires; `0` disables | `168h` |
| `EXECUTOR_EVICT_IDLE_AFTER` | at the cap, age from which such a delegation is deleted to make room; `0` disables | `1h` |
| `EXECUTOR_PRUNE_AFTER` | how long after expiry a spawned or never-funded delegation is deleted | `720h` |
| `EXECUTOR_LOG_LEVEL` | logrus level | `4` |

## Behavior

- **Batching.** A coin's deadline is the later of its due time and its expiry minus `RESERVE`.
  Renewals due by the earliest deadline share one batch, split over consecutive batches above
  `INTENTS_PER_BATCH`. A boarding coin is registered as soon as its deposit has the confirmations
  its template asks for. `GET /v1/admin/status` shows the queue, the planned batches and the coins
  forecast to miss their deadline.
- **Batch checks.** Before it cosigns a batch, the daemon checks that arkd's tree pays every
  output it registered, with its assets, in leaves its cosigner key signs from the root down, and
  that the commitment transaction pays the onchain outputs. It does not join a batch whose expiry
  is below `MIN_BATCH_EXPIRY`: the intents are queued again.
- **Caps.** A registration that meets a full cap first makes room among the delegations serving no
  coin, and is refused only when none qualifies. A delegation holding an unspent coin is never
  removed this way.
- **Failing templates.** A template refused `TEMPLATE_MAX_FAILURES` times in a row is disabled; a
  success resets the count. Coins arkd refuses for their state do not count, nor do coins that
  fees or dust leave unspendable, nor coins of a spawned delegation that the template refuses.
- **Lost batch events.** The daemon resubscribes to arkd with a backoff of up to thirty seconds.
- **Electrum limits.** A reply above 64 MiB, or more than 32 MiB of notifications waiting, drops
  the connection: the daemon reconnects and lists its scripts again.

## Streams

A template with a `trigger` is registered by the operator as a stream (admin `RegisterStream`,
`ListStreams`, `CancelStream`), not by clients. For every arkd transaction whose extension holds
the trigger's packet, the daemon captures the trigger's variables, checks that the template locks
the output and spawns a delegation that expires after `expires_after_seconds`.

A default template with a trigger, like `claim`, starts by itself when an encryption key is
configured. For `claim`, the receiver seals its terms (preimage, program to pay, least amount) to
the daemon's encryption key and the template id, and the funder puts that ciphertext in the packet.
The daemon claims only an output that pays that program at least that amount.
