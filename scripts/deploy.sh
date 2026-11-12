#!/usr/bin/env bash
# Deploys an already-built image to the kind cluster with Helm, making sure the
# cluster runs exactly that image.
#
# Usage: scripts/deploy.sh <service> <image> <cluster>
#   e.g. scripts/deploy.sh hello tiny-bank/hello:dev tiny-bank
#
# Why not just deploy <image> as-is? A fixed tag like :dev looks the same to
# Kubernetes after every rebuild, so it would keep running the old pods. Instead
# we tag the image with a hash of its contents:
#   - same code     -> same hash -> same tag -> nothing restarts
#   - changed code  -> new hash  -> new tag  -> Kubernetes rolls out new pods
#
# The hash comes from the image's layer digests (its actual files), not the image
# ID: the ID also covers build metadata like timestamps, so it changes on every
# build even when nothing else did.
set -euo pipefail

service="${1:?usage: $0 <service> <image> <cluster>}"
image="${2:?usage: $0 <service> <image> <cluster>}"
cluster="${3:?usage: $0 <service> <image> <cluster>}"

# The chart creates a ServiceMonitor, which only exists once the monitoring
# stack has installed its CRDs. Fail early with the fix, not a Helm error.
if ! kubectl --context "kind-${cluster}" get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
  echo "error: the monitoring stack isn't installed on kind-${cluster}. Run: make monitoring" >&2
  exit 1
fi

repo="${image%:*}" # tiny-bank/hello:dev -> tiny-bank/hello
layers="$(docker image inspect -f '{{json .RootFS.Layers}}' "$image")"
hash="$(printf '%s' "$layers" | shasum -a 256)"
tag="build-${hash:0:12}" # 12 hex chars, like a short git hash

docker tag "$image" "${repo}:${tag}"
kind load docker-image "${repo}:${tag}" --name "$cluster"

helm upgrade --install "$service" deploy/charts/service \
  -f "deploy/values/${service}.yaml" \
  --set image.repository="$repo" \
  --set image.tag="$tag" \
  --kube-context "kind-${cluster}" \
  --wait --timeout 2m

echo "deployed ${repo}:${tag}"
