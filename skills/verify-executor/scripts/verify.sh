#!/usr/bin/env bash
# Run the existing harness with an owned database and evidence that survives teardown.
set -euo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$root"

doctor() {
  local layer=$1
  go version
  docker inspect --format \
    '{{.Name}} {{.Config.Image}} {{.State.Status}} {{index .Config.Labels "com.docker.compose.project.working_dir"}}' \
    executor-pg
  docker exec executor-pg pg_isready -U postgres -d postgres
  python3 - <<'PY'
import socket
with socket.create_connection(("127.0.0.1", 5432), timeout=5):
    print("Postgres host port reachable")
PY
  if [[ "$layer" == daemon ]]; then return; fi
  docker inspect --format \
    '{{.Name}} {{.Config.Image}} {{.State.Status}} {{index .Config.Labels "com.docker.compose.project.working_dir"}}' \
    bitcoin arkd arkd-wallet emulator fulcrum
  nigiri rpc getblockchaininfo | python3 -c '
import json, re, sys
# Nigiri may colorize bitcoin-cli output even when stdout is a pipe.
info = json.loads(re.sub(r"\x1b\[[0-9;]*m", "", sys.stdin.read()))
assert info["chain"] == "regtest", "Bitcoin is not regtest"
print("Bitcoin network:", info["chain"], "height:", info["blocks"])
'
  python3 - <<'PY'
import json
import socket
import urllib.request

for port in (7070, 7073):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/info", timeout=10) as r:
        info = json.load(r)
    assert info.get("signerPubkey"), f"No signer on port {port}"
    if port == 7070:
        assert info.get("network") == "regtest", "Arkd is not regtest"
    print(f"Signer info on {port}:", json.dumps(info))
with urllib.request.urlopen(
    "http://127.0.0.1:7071/v1/admin/wallet/status", timeout=10
) as r:
    status = json.load(r)
assert all(status.get(k) is True for k in ("initialized", "unlocked", "synced")), status
print("Arkd wallet:", json.dumps(status))
with socket.create_connection(("127.0.0.1", 50001), timeout=10) as conn:
    conn.sendall(b'{"id":1,"method":"server.version","params":[]}\n')
    with conn.makefile("rb") as stream:
        reply = json.loads(stream.readline(65536))
    assert reply.get("result") and not reply.get("error"), reply
    print("Electrum:", json.dumps(reply))
PY
}

standalone() {
  if docker inspect executor >/dev/null 2>&1; then
    echo "A container named executor already exists; this run does not replace it." >&2
    return 1
  fi
  python3 - <<'PY'
import socket
for port in (7080, 7081):
    with socket.socket() as probe:
        assert probe.connect_ex(("127.0.0.1", port)) != 0, f"Port {port} is already in use"
PY
  cat >"$evidence/compose.override.yml" <<EOF
services:
  executor:
    environment:
      EXECUTOR_DATABASE_URL: postgres://postgres@pg:5432/$database?sslmode=disable
EOF
  local compose=(
    docker compose -f docker-compose.regtest.yml -f "$evidence/compose.override.yml"
    --profile executor
  )
  printf '%q ' "${compose[@]}" up -d --build --no-deps executor >"$evidence/command.txt"
  printf '\n' >>"$evidence/command.txt"
  container=true
  "${compose[@]}" up -d --build --no-deps executor >"$evidence/build.log" 2>&1

  for _ in $(seq 60); do
    if curl -fsS -m 2 -o /dev/null http://127.0.0.1:7081/healthz 2>/dev/null; then break; fi
    sleep 1
  done
  python3 - >"$evidence/responses.txt" <<'PY'
import json
import urllib.error
import urllib.request

def call(port, path, method="GET"):
    request = urllib.request.Request(f"http://127.0.0.1:{port}{path}", method=method)
    if method == "POST":
        request.data = b"{}"
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=10) as r:
            status, body = r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        status, body = e.code, e.read().decode()
    print(f"{method} :{port}{path} -> {status}\n{body}\n")
    return status, body

def expect(port, path, want, method="GET"):
    status, body = call(port, path, method)
    assert status == want, f"{method} :{port}{path} answered {status}, want {want}: {body}"
    return body

health = json.loads(expect(7081, "/healthz", 200))
assert health["healthy"] is True, health
assert set(health["checks"]) == {"db", "arkd", "electrum", "emulator", "loop"}, health
assert json.loads(expect(7081, "/v1/admin/health", 200))["healthy"] is True
assert "tipHeight" in json.loads(expect(7081, "/v1/admin/status", 200))
expect(7081, "/v1/admin/delegations", 200)
expect(7081, "/metrics", 200)
assert "<html" in expect(7081, "/", 200), "The admin port does not serve the UI"

info = json.loads(expect(7080, "/v1/info", 200))
assert info["network"] == "regtest" and info["batchCosignerPubkey"] and info["cosignerPubkey"], info
templates = json.loads(expect(7080, "/v1/templates", 200))["templates"]
assert len(templates) == 3, f"Want the 3 default templates, got {len(templates)}"

for path in ("/", "/healthz", "/metrics", "/v1/admin/status", "/v1/admin/delegations"):
    expect(7080, path, 404)
for path in ("/v1/template", "/v1/artifact"):
    expect(7080, path, 403, "POST")
    expect(7081, path, 400, "POST")
PY
  docker logs executor >"$evidence/executor.log" 2>&1
  if grep -qE '"level":"(fatal|panic)"' "$evidence/executor.log"; then
    echo "The daemon logged a fatal error; read $evidence/executor.log" >&2
    return 1
  fi
  local restarts
  restarts=$(docker inspect --format '{{.RestartCount}}' executor)
  if [[ "$restarts" != 0 ]]; then
    echo "The container restarted $restarts times" >&2
    return 1
  fi
  echo "Passed: image build, startup, health, default templates, public and admin port split"
}

usage() {
  echo "Usage: $0 doctor [e2e|daemon] | {e2e|daemon} '<test regex>' | standalone" >&2
  exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
if [[ "$1" == doctor ]]; then
  layer=${2:-e2e}
  [[ "$layer" == e2e || "$layer" == daemon ]] || usage
  doctor "$layer"
  exit
fi
if [[ "$1" == standalone ]]; then
  [[ $# == 1 ]] || usage
else
  [[ $# == 2 && ( "$1" == e2e || "$1" == daemon ) ]] || usage
fi
layer=$1
selector=${2:-}
evidence=$(mktemp -d /tmp/executor-verification.XXXXXX)
lock=/tmp/executor-verification.lock
locked=false
created=false
container=false
database="executor_verify_$(date +%s)_$$"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "Evidence: $evidence"
: >"$evidence/cleanup.log"

cleanup() {
  local result=$?
  trap - EXIT INT TERM
  set +e
  if [[ "$container" == true ]]; then
    docker logs executor >"$evidence/executor.log" 2>&1
    if docker rm -f executor >>"$evidence/cleanup.log" 2>&1; then
      echo "Removed owned container: executor" >>"$evidence/cleanup.log"
    else
      echo "Failed to remove owned container: executor" >>"$evidence/cleanup.log"
      result=1
    fi
  fi
  if [[ "$created" == true ]]; then
    docker exec executor-pg pg_dump -U postgres "$database" \
      >"$evidence/database.sql" 2>>"$evidence/cleanup.log"
    if [[ $? != 0 ]]; then result=1; fi
    docker exec executor-pg dropdb -U postgres --force "$database" \
      >>"$evidence/cleanup.log" 2>&1
    if [[ $? != 0 ]]; then
      echo "Failed to drop owned database: $database" >>"$evidence/cleanup.log"
      result=1
    else
      echo "Dropped owned database: $database" >>"$evidence/cleanup.log"
    fi
  fi
  if [[ "$layer" != daemon && "$locked" == true ]]; then
    for container in arkd emulator fulcrum; do
      docker logs --since "$started" "$container" \
        >"$evidence/$container.log" 2>&1
    done
  fi
  if [[ "$locked" == true ]]; then
    rm "$lock/pid"
    rmdir "$lock"
  fi
  echo "$result" >"$evidence/exit-code.txt"
  echo "Result: $result; evidence retained at $evidence"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if ! mkdir "$lock"; then
  echo "Another verification run holds $lock; inspect its PID before proceeding." >&2
  exit 1
fi
locked=true
echo "$$" >"$lock/pid"
git rev-parse HEAD >"$evidence/revision.txt"
git status --short >"$evidence/status.txt"
git diff HEAD >"$evidence/diff.patch"
printf '%s\n' "$database" >"$evidence/database-name.txt"
doctor "$layer" >"$evidence/doctor.log" 2>&1

# createdb must succeed before cleanup is allowed to drop this database.
docker exec executor-pg createdb -U postgres "$database"
created=true
if [[ "$layer" == standalone ]]; then
  standalone
  exit
fi
export EXECUTOR_DATABASE_URL="postgres://postgres@localhost:5432/$database?sslmode=disable"
args=(go test -json -count=1)
if [[ "$layer" == e2e ]]; then
  args+=(-tags e2e -timeout 40m)
else
  args+=(-timeout 15m)
fi
args+=(-run "$selector" "./test/$layer/...")
printf 'EXECUTOR_DATABASE_URL=%q ' "$EXECUTOR_DATABASE_URL" >"$evidence/command.txt"
printf '%q ' "${args[@]}" >>"$evidence/command.txt"
printf '\n' >>"$evidence/command.txt"
"${args[@]}" >"$evidence/tests.jsonl" 2>"$evidence/stderr.log"

# go test returns success even when -run matches nothing.
python3 - "$evidence/tests.jsonl" <<'PY'
import json
import sys
with open(sys.argv[1]) as stream:
    events = [json.loads(line) for line in stream if line.strip()]
passed = [e["Test"] for e in events if e.get("Test") and e["Action"] == "pass"]
skipped = [e["Test"] for e in events if e.get("Test") and e["Action"] == "skip"]
assert passed, "No test passed; check the selector and build tags"
assert not skipped, f"Skipped tests are not proof: {skipped}"
print("Passed:", ", ".join(passed))
PY
