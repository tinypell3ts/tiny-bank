# Tilt dev loop for tiny-bank: `make dev` (or `tilt up`) builds every service,
# deploys it to the local kind cluster with the shared Helm chart, port-forwards it,
# and rebuilds and redeploys whenever you save a file. The dashboard is at
# http://localhost:10350. `tilt down` removes everything Tilt deployed.
#
# Tiltfiles are written in Starlark, a small Python-like language.

# Safety: only ever deploy to the local kind cluster, never whatever cluster
# kubectl happens to point at.
CONTEXT = 'kind-tiny-bank'
if k8s_context() != CONTEXT:
    fail('Tilt only runs against %s, but kubectl is on %s. Run `make cluster-up` first.' % (CONTEXT, k8s_context()))
allow_k8s_contexts(CONTEXT)

def go_service(name, port):
    """Build cmd/<name>, deploy it with the shared chart, and forward localhost:<port>."""
    # Tilt tags each build with a content hash and loads it into kind itself,
    # so a code change always rolls out a new pod (like `make deploy`).
    # .dockerignore keeps docs and other non-Go edits from triggering rebuilds.
    docker_build(
        'tiny-bank/' + name,
        '.',
        build_args={'PKG': './cmd/' + name},
    )
    k8s_yaml(helm(
        'deploy/charts/service',
        name=name,
        values=['deploy/values/%s.yaml' % name],
    ))
    # Wait for the monitoring stack: the chart's ServiceMonitor needs its CRD.
    k8s_resource(name, port_forwards='%d:8080' % port, resource_deps=['monitoring'], labels=['services'])

# One line per service. Each gets its own localhost port.
go_service('hello', port=8080)

# --- Monitoring: Prometheus + Grafana (kube-prometheus-stack) ---
# Installed with plain Helm through `make monitoring`, so it works the same with
# or without Tilt, and the chart's CRDs are handled by Helm. It reruns only when
# the values file changes. `tilt down` leaves it running (it's slow to install);
# `make monitoring-down` removes it.
local_resource(
    'monitoring',
    cmd='make monitoring',
    deps=['deploy/monitoring/values.yaml'],
    labels=['monitoring'],
)

def forward(name, service, port, target):
    """Forward localhost:<port> to a service in the monitoring namespace."""
    local_resource(
        name,
        serve_cmd='kubectl --context %s -n monitoring port-forward svc/%s %d:%d' % (CONTEXT, service, port, target),
        resource_deps=['monitoring'],
        links=[link('http://localhost:%d' % port, name)],
        labels=['monitoring'],
    )

forward('grafana', 'monitoring-grafana', 3000, 80)
forward('prometheus', 'monitoring-prometheus', 9090, 9090)
