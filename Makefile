# CI runs these same targets, so "works on my machine" == "works in CI".
.DEFAULT_GOAL := help

SERVICE ?= hello
IMAGE   ?= tiny-bank/$(SERVICE):dev
CLUSTER ?= tiny-bank
KUBECTX := kind-$(CLUSTER)
CHART   := deploy/charts/service

# Monitoring stack: Prometheus Operator, Prometheus, Grafana, kube-state-metrics.
MONITORING_CHART         := oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
MONITORING_CHART_VERSION := 93.2.2

# CloudNativePG operator: runs a Postgres cluster for each service that declares one.
CNPG_CHART         := oci://ghcr.io/cloudnative-pg/charts/cloudnative-pg
CNPG_CHART_VERSION := 0.29.1

# kubeconform only knows built-in Kubernetes types. Schemas for custom ones
# (like ServiceMonitor) come from the CRDs catalog, pinned to one commit so a
# change upstream can't break or loosen our checks.
CRD_SCHEMAS := https://raw.githubusercontent.com/datreeio/CRDs-catalog/89c736bdde38b5fecd0f8436ec5f3aa3679a3b86/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json

.PHONY: help fmt fmt-check vet lint test test-scripts check hooks docker-build docker-smoke \
	cluster-up cluster-down image-load chart-check deploy undeploy monitoring monitoring-down dashboards dashboards-check cnpg cnpg-down dev dev-down

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-16s %s\n", $$1, $$2}'

fmt: ## Format all Go code in place
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint (brew install golangci-lint)
	golangci-lint run ./...

test: ## Run unit tests with the race detector and coverage
	go test -race -cover ./...

test-scripts: ## Test the shell scripts in scripts/
	scripts/check-branch-name_test.sh

check: fmt-check vet lint test test-scripts ## Everything CI runs

hooks: ## Use the repo's git hooks (branch-name check on push)
	git config core.hooksPath scripts/hooks

docker-build: ## Build the image for SERVICE (default hello) as IMAGE
	docker build --build-arg PKG=./cmd/$(SERVICE) -t $(IMAGE) .

docker-smoke: docker-build ## Run IMAGE, check /healthz, then check it stops cleanly on SIGTERM
	scripts/docker-smoke.sh $(IMAGE)

cluster-up: ## Create the local kind cluster (does nothing if it already exists)
	@kind get clusters 2>/dev/null | grep -qx '$(CLUSTER)' || kind create cluster --name $(CLUSTER) --config deploy/kind/cluster.yaml --wait 2m
	@kubectl cluster-info --context $(KUBECTX)

cluster-down: ## Delete the local kind cluster and everything in it
	kind delete cluster --name $(CLUSTER)

image-load: docker-build ## Build IMAGE and copy it into the kind cluster's node
	kind load docker-image $(IMAGE) --name $(CLUSTER)

# Every service's values, plus deploy/charts/service/ci/*.yaml, which turn on
# chart features no service uses yet (like a database) so they're checked too.
chart-check: ## Lint the chart and validate its output for every service (brew install kubeconform)
	@for v in deploy/values/*.yaml $(CHART)/ci/*.yaml; do \
		echo "--- $$v"; \
		helm lint $(CHART) -f $$v --strict || exit 1; \
		helm template $$(basename $$v .yaml) $(CHART) -f $$v | kubeconform -strict -summary \
			-schema-location default -schema-location '$(CRD_SCHEMAS)' || exit 1; \
	done

deploy: docker-build ## Build IMAGE and deploy exactly that build of SERVICE to the kind cluster
	scripts/deploy.sh $(SERVICE) $(IMAGE) $(CLUSTER)

undeploy: ## Remove SERVICE from the kind cluster
	helm uninstall $(SERVICE) --kube-context $(KUBECTX)

monitoring: ## Install or upgrade Prometheus + Grafana in the "monitoring" namespace (make dev does this too)
	helm upgrade --install monitoring $(MONITORING_CHART) --version $(MONITORING_CHART_VERSION) \
		--kube-context $(KUBECTX) --namespace monitoring --create-namespace \
		-f deploy/monitoring/values.yaml --wait --timeout 5m
	@$(MAKE) --no-print-directory dashboards

# Grafana's sidecar loads any ConfigMap labelled grafana_dashboard=1, so our
# dashboards are plain JSON files in git, not something clicked together in the UI.
dashboards: ## Load deploy/monitoring/dashboards/*.json into Grafana (part of make monitoring)
	kubectl --context $(KUBECTX) -n monitoring create configmap tiny-bank-dashboards \
		--from-file=deploy/monitoring/dashboards/ --dry-run=client -o yaml \
		| kubectl --context $(KUBECTX) label --local -f - grafana_dashboard=1 -o yaml \
		| kubectl --context $(KUBECTX) apply -f -

dashboards-check: ## Check every dashboard is valid JSON with a uid, a title and panels
	@for f in deploy/monitoring/dashboards/*.json; do \
		jq -e '(.uid | type == "string") and (.title | type == "string") and (.panels | length > 0)' $$f >/dev/null \
			|| { echo "invalid dashboard: $$f"; exit 1; }; \
	done; echo "ok: dashboards"

monitoring-down: ## Remove the monitoring stack (its CRDs stay, as Helm never deletes CRDs)
	helm uninstall monitoring --kube-context $(KUBECTX) --namespace monitoring

cnpg: ## Install or upgrade the CloudNativePG operator in "cnpg-system" (needs make monitoring; make dev does both)
	helm upgrade --install cnpg $(CNPG_CHART) --version $(CNPG_CHART_VERSION) \
		--kube-context $(KUBECTX) --namespace cnpg-system --create-namespace \
		-f deploy/cnpg/values.yaml --wait --timeout 5m

cnpg-down: ## Remove the CloudNativePG operator (its CRDs stay; delete databases first)
	helm uninstall cnpg --kube-context $(KUBECTX) --namespace cnpg-system

dev: cluster-up ## Start the Tilt dev loop: build, deploy and redeploy on save (dashboard: localhost:10350)
	tilt up --context $(KUBECTX)

dev-down: ## Remove everything Tilt deployed (the cluster stays up)
	tilt down --context $(KUBECTX)
