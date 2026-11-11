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

# Wait up to ~5s for the server to come up.
for _ in $(seq 1 50); do
  curl -fsS "${base}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

curl -fsS "${base}/healthz" >/dev/null || fail "/healthz did not return 200"
curl -fsS "${base}/readyz" >/dev/null || fail "/readyz did not return 200"

user="$(docker inspect -f '{{.Config.User}}' "$id")"
[[ "$user" != "" && "$user" != "root" && "$user" != "0" ]] || fail "image runs as root (user=${user:-<unset>})"

# docker stop sends SIGTERM, then SIGKILL after 10s. A graceful shutdown exits 0 well before that.
docker stop "$id" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$id")"
[[ "$code" == "0" ]] || fail "exit code after SIGTERM was $code, want 0"

docker logs "$id" 2>&1 | grep -q '"msg":"server stopped"' || fail "no 'server stopped' log line"

echo "ok: $image starts, is healthy, runs as $user and shuts down cleanly"
