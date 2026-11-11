# CI runs these same targets, so "works on my machine" == "works in CI".
.DEFAULT_GOAL := help

.PHONY: help fmt fmt-check vet lint test check hooks

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'

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

check: fmt-check vet lint test ## Everything CI runs

hooks: ## Use the repo's git hooks (branch-name check on push)
	git config core.hooksPath scripts/hooks
