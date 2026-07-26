# calserver developer tasks.
#
# Run inside the nix dev shell (direnv loads it on cd into the repo), or prefix
# any target with `direnv exec . `, e.g. `direnv exec . make test`.

GO    ?= go
ADDR  ?= :8080
REALM ?= SabreDAV
# DSN for the shared dev Postgres started by scripts/dev-db.sh.
DB    ?= postgres://$(USER)@/calserver?host=$(CURDIR)/.dev/pg

# Admin panel (calserver-admin) dev settings.
ADMIN_ADDR ?= 127.0.0.1:8081
ADMIN_AUTH ?= .dev/admin-auth.htpasswd
ADMIN_USER ?= root
ADMIN_PASS ?= root

# `make` with no target starts the DB and both servers.
.DEFAULT_GOAL := dev

.PHONY: help dev run all build test vet fmt corpus parity \
        db-start db-stop db-reset \
        caldavtester caldavtester-full clean

dev: build db-start $(ADMIN_AUTH) ## Start the dev DB and both servers: CalDAV/CardDAV + admin panel (default)
	@echo "calserver (CalDAV/CardDAV) → http://$(ADDR)"
	@echo "calserver-admin (panel)    → http://$(ADMIN_ADDR)  (login $(ADMIN_USER) / $(ADMIN_PASS))"
	@echo "Ctrl-C stops both servers."
	@trap 'kill 0' EXIT INT TERM; \
	 ./bin/calserver -addr $(ADDR) -db "$(DB)" -realm $(REALM) & \
	 ./bin/calserver-admin -addr $(ADMIN_ADDR) -db "$(DB)" -realm $(REALM) \
	   -admin-auth $(ADMIN_AUTH) & \
	 wait

# Generate a bcrypt htpasswd for the admin panel on first run (dev creds only).
$(ADMIN_AUTH):
	@mkdir -p $(dir $@)
	htpasswd -cbB $@ $(ADMIN_USER) $(ADMIN_PASS)

run: build db-start ## Build and run only the CalDAV/CardDAV server against the dev DB
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
