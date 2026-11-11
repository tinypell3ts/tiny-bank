# CI runs these same targets, so "works on my machine" == "works in CI".
.DEFAULT_GOAL := help

SERVICE ?= hello
IMAGE   ?= tiny-bank/$(SERVICE):dev
CLUSTER ?= tiny-bank
KUBECTX := kind-$(CLUSTER)
CHART   := deploy/charts/service
VALUES  := deploy/values/$(SERVICE).yaml

.PHONY: help fmt fmt-check vet lint test test-scripts check hooks docker-build docker-smoke \
	cluster-up cluster-down image-load chart-check deploy undeploy

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-13s %s\n", $$1, $$2}'

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

chart-check: ## Lint the chart and validate its output for every service (brew install kubeconform)
	@for v in deploy/values/*.yaml; do \
		echo "--- $$v"; \
		helm lint $(CHART) -f $$v --strict || exit 1; \
		helm template $$(basename $$v .yaml) $(CHART) -f $$v | kubeconform -strict -summary || exit 1; \
	done

deploy: image-load ## Build, load and install or upgrade SERVICE on the kind cluster
	helm upgrade --install $(SERVICE) $(CHART) -f $(VALUES) --kube-context $(KUBECTX) --wait --timeout 2m

undeploy: ## Remove SERVICE from the kind cluster
	helm uninstall $(SERVICE) --kube-context $(KUBECTX)
