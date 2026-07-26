# calserver developer tasks.
#
# Run inside the nix dev shell (direnv loads it on cd into the repo), or prefix
# any target with `direnv exec . `, e.g. `direnv exec . make test`.

GO    ?= go
ADDR  ?= :8080
REALM ?= SabreDAV
# DSN for the shared dev Postgres started by scripts/dev-db.sh.
DB    ?= postgres://$(USER)@/calserver?host=$(CURDIR)/.dev/pg

# `make` with no target starts the server.
.DEFAULT_GOAL := run

.PHONY: help run all build test vet fmt corpus parity \
        db-start db-stop db-reset \
        caldavtester caldavtester-full clean

run: build db-start ## Build and run the server against the dev DB (default)
	./bin/calserver -addr $(ADDR) -db "$(DB)" -realm $(REALM)

help: ## Show available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}'

all: vet test ## Vet then run the full Go test suite

build: ## Build the server and admin binaries into bin/
	$(GO) build -o bin/calserver ./cmd/calserver
	$(GO) build -o bin/calserver-admin ./cmd/calserver-admin

test: ## Run the full Go test suite
	$(GO) test ./...

vet: ## Run go vet over everything
	$(GO) vet ./...

fmt: ## Format the tree with gofmt
	$(GO) fmt ./...

corpus: ## Replay the sabre-extracted corpus (protocol conformance)
	$(GO) test ./internal/corpus/ -run TestCorpus -v

parity: db-start ## Live co-existence parity test against sabre (needs the dev DB)
	PARITY=1 $(GO) test ./internal/storage/ -run TestParity -v

db-start: ## Start the shared dev Postgres
	scripts/dev-db.sh start

db-stop: ## Stop the dev Postgres
	scripts/dev-db.sh stop

db-reset: ## Drop and reload the dev schema
	scripts/dev-db.sh reset

caldavtester: ## Run the curated CalDAVTester conformance subset
	scripts/caldavtester/run.sh

caldavtester-full: ## Run the ENTIRE CalDAVTester suite, categorized
	scripts/caldavtester/run-full.sh

clean: ## Remove build artifacts
	rm -rf bin
