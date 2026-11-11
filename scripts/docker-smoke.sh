#!/usr/bin/env bash
# Smoke-tests a service image: it starts, answers /healthz and /readyz, runs as
# non-root, and exits 0 on SIGTERM (what Kubernetes sends to stop a pod).
#
# Usage: scripts/docker-smoke.sh <image>
set -euo pipefail

image="${1:?usage: $0 <image>}"

# Publish container port 8080 on a random free localhost port.
id="$(docker run -d -p 127.0.0.1::8080 "$image")"
trap 'docker rm -f "$id" >/dev/null 2>&1 || true' EXIT
port="$(docker port "$id" 8080/tcp | head -n1 | sed 's/.*://')"
base="http://127.0.0.1:${port}"

fail() {
  echo "FAIL: $*" >&2
  docker logs "$id" >&2 || true
  exit 1
}

# Every request gets a hard timeout, so a server that accepts connections but never
# answers fails fast instead of hanging CI.
probe() { curl -fsS --max-time 1 "${base}$1" >/dev/null 2>&1; }

# Wait for startup, with an overall deadline (SECONDS is bash's built-in timer).
deadline=$((SECONDS + 10))
until probe /healthz; do
  ((SECONDS < deadline)) || fail "/healthz not healthy within 10s"
  sleep 0.2
done
probe /readyz || fail "/readyz did not return 200"

# USER can be "name", "uid", "name:group" or "uid:gid". Only the part before ':' decides
# whether we run as root, so "root:nonroot" or "0:65532" must still fail.
user="$(docker inspect -f '{{.Config.User}}' "$id")"
uid="${user%%:*}"
[[ -n "$uid" && "$uid" != "root" && "$uid" != "0" ]] || fail "image runs as root (user=${user:-<unset>})"

# docker stop sends SIGTERM, then SIGKILL after 10s. A graceful shutdown exits 0 well before that.
docker stop "$id" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$id")"
[[ "$code" == "0" ]] || fail "exit code after SIGTERM was $code, want 0"

docker logs "$id" 2>&1 | grep -q '"msg":"server stopped"' || fail "no 'server stopped' log line"

echo "ok: $image starts, is healthy, runs as $user and shuts down cleanly"
