# yk.gy developer commands. Deploys to Development / Production are done by
# GitHub Actions only; there is intentionally no deploy target here.

SHELL := /bin/bash
.DEFAULT_GOAL := help

LOCAL_URL ?= http://127.0.0.1:7676
VICEROY   ?= $(shell command -v viceroy 2>/dev/null || echo "$(HOME)/Library/Application Support/fastly/viceroy")

# Guard: never inherit a service id from the shell for local commands.
unexport FASTLY_SERVICE_ID

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- local
.PHONY: dev
dev: ## Start the local server with auto-rebuild (http://localhost:7676)
	fastly compute serve --watch

.PHONY: serve
serve: ## Start the local server without file watching
	fastly compute serve

.PHONY: build
build: ## Build the Wasm package (pkg/yk-gy.tar.gz)
	fastly compute build

# ---------------------------------------------------------------- quality
.PHONY: test
test: ## Unit tests (native Go)
	go test ./...

.PHONY: test-race
test-race: ## Unit tests with the race detector and coverage
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: test-integration
test-integration: ## Integration tests inside Viceroy (real KV Store semantics)
	cd test/integration && GOOS=wasip1 GOARCH=wasm go test -count=1 -exec "'$(VICEROY)' run -C fastly.toml" .

.PHONY: fuzz
fuzz: ## Fuzz the URL validator for 30s
	go test -run='^$$' -fuzz=FuzzNormalize -fuzztime=30s ./internal/validation/

.PHONY: smoke
smoke: ## Smoke-test the running local server (writes to LOCAL data only)
	scripts/smoke-test.sh $(LOCAL_URL) --env local --write --deep

.PHONY: lint
lint: ## gofmt + go vet (+ staticcheck / govulncheck when installed)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	GOOS=wasip1 GOARCH=wasm go vet ./...
	@if command -v staticcheck >/dev/null; then staticcheck ./...; else echo "(staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest)"; fi
	@if command -v govulncheck >/dev/null; then govulncheck ./...; else echo "(govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@latest)"; fi

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w .

.PHONY: check
check: lint test test-integration build ## Run what CI runs, locally

# ---------------------------------------------------------------- workflow
.PHONY: status
status: ## Branch, environment health and latest CI/CD runs
	@scripts/status.sh

.PHONY: feature
feature: ## Create a feature branch from dev: make feature NAME=my-change
	@test -n "$(NAME)" || { echo "usage: make feature NAME=short-name"; exit 1; }
	git fetch origin && git switch dev && git pull --ff-only origin dev && git switch -c feature/$(NAME)

.PHONY: hotfix
hotfix: ## Create a hotfix branch from main: make hotfix NAME=fix-name
	@test -n "$(NAME)" || { echo "usage: make hotfix NAME=short-name"; exit 1; }
	git fetch origin && git switch main && git pull --ff-only origin main && git switch -c hotfix/$(NAME)

.PHONY: switch-dev
switch-dev: ## Switch to an up-to-date dev branch
	git fetch origin && git switch dev && git pull --ff-only origin dev

.PHONY: pr-dev
pr-dev: ## Push the current feature/* branch and open a PR to dev
	git push -u origin HEAD && gh pr create --base dev --fill

.PHONY: pr-main
pr-main: ## Open the release PR dev -> main
	gh pr create --base main --head dev --title "Release $$(date +%Y-%m-%d)" --body "Release dev to production. The production deploy runs only after explicit approval (see README)."

.PHONY: deploy-prod
deploy-prod: ## Print how to request a production deploy (runs in GitHub Actions)
	@echo "Production deploys run in GitHub Actions only. For the current main HEAD:"
	@echo "  gh workflow run deploy-prod.yml --ref main -f confirm=deploy-production"

.PHONY: rollback
rollback: ## Print how to roll back (runs in GitHub Actions)
	@echo "  gh workflow run rollback.yml --ref main -f environment=development"
	@echo "  gh workflow run rollback.yml --ref main -f environment=production -f confirm=rollback-production [-f target_version=N]"

.PHONY: clean
clean: ## Remove build output (bin/, pkg/, coverage.out)
	@for p in bin/main.wasm pkg/yk-gy.tar.gz coverage.out; do [ -f "$$p" ] && unlink "$$p" || true; done
