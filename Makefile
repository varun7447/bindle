# Bindle: one entry point for local development. `make help` lists targets.
#
# Layout:
#   backend/   Go services (ingest, api, processor, alerts, migrate)
#   console/   Next.js console
#   sdks/      Python and TypeScript SDKs
#   deploy/    local (docker compose), aws (CDK), otel-collector
#   examples/  agents to try Bindle with

SHELL := /bin/bash
GO ?= go
COMPOSE := docker compose -f deploy/local/compose.yaml
ENV_FILE := deploy/local/.env

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---- local stack ------------------------------------------------------------

$(ENV_FILE):
	./deploy/local/dev-env.sh

.PHONY: up
up: $(ENV_FILE) ## Start the local stack (OpenSearch, PostgreSQL, ingest, API, alerts)
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop the local stack (data is kept)
	$(COMPOSE) down

.PHONY: logs
logs: ## Follow the local stack's logs
	$(COMPOSE) logs -f --tail=50

.PHONY: console
console: ## Run the console at http://localhost:3000 (needs `make up`)
	cd console && npm install --no-audit --no-fund && \
	  BINDLE_API_URL=http://127.0.0.1:8080 BINDLE_PUBLIC_INGEST_URL=http://localhost:4318 npm run dev

.PHONY: demo
demo: $(ENV_FILE) ## Send demo runs (one with a runaway loop) to the local stack
	set -a; . $(ENV_FILE); set +a; python3 examples/sdk/demo_agent.py && python3 examples/sdk/demo_agent.py --loop

# ---- tests ------------------------------------------------------------------

.PHONY: test
test: test-backend test-python test-typescript test-console ## Run every test suite

.PHONY: test-backend
test-backend: ## Go vet + tests (integration tests run when the local stack is up)
	cd backend && $(GO) vet ./... && $(GO) test ./...

.PHONY: test-python
test-python: ## Python SDK tests
	cd sdks/python && python3 -m pytest -q

.PHONY: test-typescript
test-typescript: ## TypeScript SDK typecheck + tests
	cd sdks/typescript && npm install --no-audit --no-fund && npx tsc --noEmit && npm test

.PHONY: test-console
test-console: ## Console typecheck + tests + production build
	cd console && npm install --no-audit --no-fund && npm run typecheck && npm test && npm run build

# ---- AWS --------------------------------------------------------------------

.PHONY: synth
synth: ## Synthesize the CDK stack (no deploy)
	cd deploy/aws && npm install --no-audit --no-fund && npx cdk synth -c insecureDevCidr=203.0.113.0/24 > /dev/null && echo "synth ok"
