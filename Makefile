SHELL := /bin/bash

BINARY_NAME := detent
BINARY_PATH := tmp/$(BINARY_NAME)
CMD_PACKAGE := ./cmd/detent
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ifndef DATE
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
endif
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
DEV_VERSION ?= dev
COVERPROFILE := tmp/coverage.out
COVERPROFILE_RAW := tmp/coverage.raw.out
MODERNIZE_FIX_FLAGS ?= -newexpr=false
AIR_VERSION := v1.64.0
TEMPL_VERSION := v0.3.1001
TEMPL ?= go run github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)
TAILWIND_INPUT ?= static/css/input.css
TAILWIND_OUTPUT ?= static/css/output.css
SQLC_VERSION := v1.31.1
SQLC := go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
APP_DIR ?= web/conversation
CHECK_LAND_BASE ?= origin/develop
SQLC_CONFIG ?= internal/hubserver/postgres/sqlc.yaml
MIGRATIONS_DIR ?= internal/hubserver/postgres/migrations
GOOSE_DRIVER ?= postgres
GOOSE_VERSION := v3.26.0
DATABASE_URL ?=
GOVULNCHECK_VERSION ?= v1.10.0
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
# Upstream issue: https://github.com/securego/gosec/issues/1712
GOSEC_VERSION ?= v2.30.0
GOSEC_BINARY ?= tmp/gosec-$(GOSEC_VERSION)-deterministic
GOSEC_PATCH ?= scripts/gosec-v2.30.0-deterministic.patch
TEST_PROCS ?= 4
GOMAXPROCS ?= $(TEST_PROCS)
export GOMAXPROCS
TEST_TIMEOUT ?= 10m
GO_TEST := env -u DETENT_API_TOKEN go test -p $(TEST_PROCS) -timeout=$(TEST_TIMEOUT)
GOLANGCI_LINT_VERSION_FILE := .golangci-version
GOLANGCI_LINT_VERSION := $(shell cat $(GOLANGCI_LINT_VERSION_FILE))
GOLANGCI_LINT_TOOLCHAIN := $(shell awk '/^toolchain / { print $$2 }' go.mod)
GOLANGCI_LINT_DIR := $(CURDIR)/tmp/tools/golangci-lint/$(GOLANGCI_LINT_VERSION)/$(GOLANGCI_LINT_TOOLCHAIN)
GOLANGCI_LINT := $(GOLANGCI_LINT_DIR)/golangci-lint
HEFT_VERSION := $(shell awk '/digitaldrywood\/heft$$/ { found = 1 } found && $$1 == "version:" { print $$2; exit }' .custom-gcl.yml 2>/dev/null)
GOLANGCI_LINT_CUSTOM_DIR := $(CURDIR)/tmp/tools/golangci-lint-custom/$(GOLANGCI_LINT_VERSION)-heft-$(HEFT_VERSION)/$(GOLANGCI_LINT_TOOLCHAIN)
GOLANGCI_LINT_CUSTOM := $(GOLANGCI_LINT_CUSTOM_DIR)/golangci-lint-custom
GOLANGCI_LINT_RUN := $(if $(wildcard .custom-gcl.yml),$(GOLANGCI_LINT_CUSTOM),$(GOLANGCI_LINT))
HEFT := GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" go run github.com/digitaldrywood/heft/cmd/heft@$(HEFT_VERSION)
# G115: existing conversions are bounded or intentionally preserve platform/API widths.
# G301: shared runtime, service, and artifact directories intentionally require traversal access.
# G304: Detent intentionally reads operator-selected config, workflow, and workspace paths.
# G306: flagged files are non-secret configs, manifests, screenshots, and generated artifacts.
GOSEC_EXCLUDES ?= G115,G301,G304,G306,G703,G704
GOSEC_EXCLUDE_DIRS ?= .detent
GOSEC_EXCLUDE_DIR_FLAGS := $(addprefix -exclude-dir=,$(GOSEC_EXCLUDE_DIRS))
.PHONY: dev generate check-migrations check-generated css css-watch app app-dev app-test app-test-run app-contract board-contract-run check-app build test test-fast test-race coverage-check test-cover soak visual-e2e lint vet gosec-build security check check-landing check-release modernize-check source-metadata release-snapshot sqlc db-create db-migrate setup clean help

dev:
	@mkdir -p tmp
	@if [ -f tmp/air-combined.log ]; then \
		mv tmp/air-combined.log tmp/air-combined-$$(date +%Y%m%d-%H%M%S).log; \
	fi
	@ls -t tmp/air-combined-*.log 2>/dev/null | tail -n +6 | xargs rm -f 2>/dev/null || true
	@ENV=dev LOG_LEVEL=debug DETENT_AIR_VERSION=$(DEV_VERSION) air 2>&1 | tee tmp/air-combined.log

generate: assets
	@go generate ./...
	@if [ -n "$$(git ls-files --others --exclude-standard -- '*.templ'; git ls-files -- '*.templ')" ]; then \
		$(TEMPL) generate; \
	else \
		echo "No templ files found; skipping templ generate."; \
	fi
	@$(MAKE) sqlc

.PHONY: assets generate-docs release-notes
assets: app css release-notes

generate-docs:
	go run ./internal/config/cmd/configdoc -root .

check-migrations:
	go run ./tools/migrationcheck

check-generated: generate-docs
	$(SQLC) diff -f "$(SQLC_CONFIG)"

css:
	@if [ -f "$(TAILWIND_INPUT)" ]; then \
		if [ -f package-lock.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm ci; elif [ -f package.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm install; fi; \
		mkdir -p "$$(dirname "$(TAILWIND_OUTPUT)")"; \
		node_modules/.bin/tailwindcss -i "$(TAILWIND_INPUT)" -o "$(TAILWIND_OUTPUT)" --minify; \
	else \
		echo "No Tailwind input at $(TAILWIND_INPUT); skipping CSS build."; \
	fi

app:
	@if [ -f "$(APP_DIR)/package.json" ]; then \
		(cd "$(APP_DIR)" && npm ci && npm run build); \
	else \
		echo "No conversation client at $(APP_DIR); skipping app build."; \
	fi

app-dev:
	@if [ -f "$(APP_DIR)/package.json" ]; then \
		if [ ! -d "$(APP_DIR)/node_modules" ]; then (cd "$(APP_DIR)" && npm ci); fi; \
		(cd "$(APP_DIR)" && npm run dev); \
	else \
		echo "No conversation client at $(APP_DIR); skipping app dev server."; \
	fi

check-app:
	@set -e; if [ -f "$(APP_DIR)/package.json" ]; then \
		if [ ! -d "$(APP_DIR)/node_modules" ]; then (cd "$(APP_DIR)" && npm ci); fi; \
		(cd "$(APP_DIR)" && npm run typecheck && npm run design:tokens:check && npm run design:catalog:check); \
		$(MAKE) app-test-run; \
		grep -q "MIT" static/app/conversation/app.js || { \
			echo "static/app/conversation/app.js is missing the MIT attribution banner."; \
			exit 1; \
		}; \
		grep -q "^react@" static/app/conversation/THIRD_PARTY_LICENSES.txt || { \
			echo "static/app/conversation/THIRD_PARTY_LICENSES.txt is missing bundled dependency licenses."; \
			exit 1; \
		}; \
	else \
		echo "No conversation client at $(APP_DIR); skipping client checks."; \
	fi

app-test:
	@set -e; if [ -f "$(APP_DIR)/package.json" ]; then \
		if [ ! -d "$(APP_DIR)/node_modules" ]; then (cd "$(APP_DIR)" && npm ci); fi; \
		(cd "$(APP_DIR)" && npm run typecheck); \
		$(MAKE) app-test-run; \
	else \
		echo "No conversation client at $(APP_DIR); skipping app tests."; \
	fi

app-test-run: app
	@$(MAKE) board-contract-run

app-contract:
	@set -e; if [ -f "$(APP_DIR)/package.json" ]; then \
		if [ ! -d "$(APP_DIR)/node_modules" ]; then (cd "$(APP_DIR)" && npm ci); fi; \
		$(MAKE) board-contract-run APP_TEST_ARGS=src/contracts/fixtures.test.ts; \
	else \
		echo "No conversation client at $(APP_DIR); skipping the board contract check."; \
	fi

board-contract-run:
	@set -e; mkdir -p "$(CURDIR)/tmp"; \
		fixture_root=$$(mktemp -d "$(CURDIR)/tmp/board-contract.XXXXXX"); \
		mkdir "$$fixture_root/runtime"; \
		export TMPDIR="$$fixture_root/runtime" TMP="$$fixture_root/runtime" TEMP="$$fixture_root/runtime" \
			DETENT_TEST_BOARD_CONTRACT_DIR="$$fixture_root" DETENT_TEST_POSTGRES=binary \
			GOTOOLCHAIN="$$(awk '$$1 == "toolchain" {print $$2}' go.mod)"; \
		env -u DETENT_API_TOKEN go test -p $(TEST_PROCS) -parallel=$(TEST_PROCS) -timeout=120s -v ./internal/hubserver -run '^(TestBoardRequestPreservesPageContract|TestBoardViewCardsMatchLaneCards)$$'; \
		(cd "$(APP_DIR)" && npm test -- --maxWorkers=$(TEST_PROCS) $(APP_TEST_ARGS))

css-watch:
	@if [ -f "$(TAILWIND_INPUT)" ]; then \
		if [ -f package-lock.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm ci; elif [ -f package.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm install; fi; \
		mkdir -p "$$(dirname "$(TAILWIND_OUTPUT)")"; \
		node_modules/.bin/tailwindcss -i "$(TAILWIND_INPUT)" -o "$(TAILWIND_OUTPUT)" --watch; \
	else \
		echo "No Tailwind input at $(TAILWIND_INPUT); skipping CSS watch."; \
	fi

build: assets generate-docs
	@mkdir -p tmp
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_PATH) $(CMD_PACKAGE)

test: generate-docs
	$(GO_TEST) ./internal/workspace/...
	@packages="$$(go list ./...)" && \
	packages="$$(printf '%s\n' "$$packages" | awk '$$0 != "github.com/digitaldrywood/detent/internal/workspace" && index($$0, "github.com/digitaldrywood/detent/internal/workspace/") != 1 && $$0 != "github.com/digitaldrywood/detent/internal/web"')" && \
	$(GO_TEST) $$packages
	# Measure the 500ms request budget without competing package tests or builds.
	$(GO_TEST) ./internal/web

test-fast: generate-docs
	$(GO_TEST) -short -timeout=10m ./...

test-race: generate-docs
	$(GO_TEST) -short -race -timeout=30m ./...

test-cover:
	@mkdir -p tmp
	$(GO_TEST) -coverprofile=$(COVERPROFILE_RAW) ./...
	@$(MAKE) coverage-check

coverage-check:
	@awk 'NR == 1 || ($$1 !~ /_templ\.go:/ && $$1 !~ /\/internal\/store\/sqlc\// && $$1 !~ /\/internal\/database\/sqlc\//)' "$(COVERPROFILE_RAW)" > "$(COVERPROFILE)"
	@go tool cover -func=$(COVERPROFILE) | awk '/^total:/ { printf "coverage %s of statements (evidence only, no floor)\n", $$3 }'

soak:
	DETENT_RUN_SOAK_TESTS=1 $(GO_TEST) ./internal/orchestrator -run '^(TestSoak|TestDispatchParityAdversarialFixtures)' -count=1

visual-e2e: build
	@if [ ! -x node_modules/.bin/playwright ]; then npm ci; fi
	node_modules/.bin/playwright test

lint: $(GOLANGCI_LINT_RUN)
	GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" "$(GOLANGCI_LINT_RUN)" run --allow-parallel-runners --concurrency=$(TEST_PROCS) --timeout=15m
ifneq ($(wildcard .custom-gcl.yml),)
	$(HEFT) check ./...
endif

$(GOLANGCI_LINT_CUSTOM): .custom-gcl.yml $(GOLANGCI_LINT_VERSION_FILE) scripts/runner-setup.sh
	@grep -q '^version: $(GOLANGCI_LINT_VERSION)$$' .custom-gcl.yml || { echo ".custom-gcl.yml version must match $(GOLANGCI_LINT_VERSION_FILE) ($(GOLANGCI_LINT_VERSION))"; exit 1; }
	@GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" bash scripts/runner-setup.sh golangci-lint-custom "$(GOLANGCI_LINT_VERSION)" "$(HEFT_VERSION)" "$(GOLANGCI_LINT_CUSTOM)"

$(GOLANGCI_LINT):
	@bash scripts/runner-setup.sh go-tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint $(GOLANGCI_LINT_VERSION) "$(GOLANGCI_LINT)" "$(GOLANGCI_LINT_TOOLCHAIN)"

vet:
	go vet -p $(TEST_PROCS) ./...

gosec-build:
	./scripts/build-gosec.sh "$(GOSEC_VERSION)" "$(GOSEC_PATCH)" "$(GOSEC_BINARY)"

security: gosec-build
	$(GOVULNCHECK) ./...
	$(GOSEC_BINARY) -quiet -exclude-generated -severity=medium -confidence=medium -exclude=$(GOSEC_EXCLUDES) $(GOSEC_EXCLUDE_DIR_FLAGS) ./...

check: check-migrations check-generated lint vet test-fast
	go build ./...
	@echo "Checks passed."

check-landing:
	bash scripts/check-landing.sh "$(CHECK_LAND_BASE)" "$(TEST_PROCS)" "$(GOLANGCI_LINT_RUN)"

check-release:
	bash scripts/check-release.sh "$(TEST_PROCS)"

modernize-check:
	go fix -diff $(MODERNIZE_FIX_FLAGS) ./...

release-notes:
	go run ./tools/releasenotes -version "$(VERSION)" -commit "$(COMMIT)" -date "$(DATE)"

source-metadata: release-notes
	@mkdir -p tmp/source-metadata
	@printf '%s\n' '$(LDFLAGS)' > tmp/source-metadata/BUILD_LDFLAGS

release-snapshot:
	goreleaser release --snapshot --clean

sqlc:
	@if [ -f "$(SQLC_CONFIG)" ]; then \
		$(SQLC) generate -f "$(SQLC_CONFIG)"; \
	else \
		echo "No sqlc config at $(SQLC_CONFIG); skipping sqlc generate."; \
	fi

db-create:
	@test -n "$(NAME)" || { echo "Use make db-create NAME=description [MIGRATIONS_DIR=directory]"; exit 1; }
	TZ=UTC goose -dir "$(MIGRATIONS_DIR)" create "$(NAME)" sql

db-migrate:
	@if [ -d "$(MIGRATIONS_DIR)" ]; then \
		mkdir -p "$$(dirname "$(DATABASE_URL)")"; \
		goose -allow-missing -dir "$(MIGRATIONS_DIR)" "$(GOOSE_DRIVER)" "$(DATABASE_URL)" up; \
	else \
		echo "No migrations directory at $(MIGRATIONS_DIR); skipping database migration."; \
	fi

setup:
	@bash scripts/runner-setup.sh go-tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint $(GOLANGCI_LINT_VERSION) "$(GOLANGCI_LINT)" "$(GOLANGCI_LINT_TOOLCHAIN)"
	@bash scripts/runner-setup.sh go-tool github.com/air-verse/air $(AIR_VERSION) air
	@bash scripts/runner-setup.sh go-tool github.com/a-h/templ/cmd/templ $(TEMPL_VERSION) templ
	@bash scripts/runner-setup.sh go-tool github.com/sqlc-dev/sqlc/cmd/sqlc $(SQLC_VERSION) sqlc
	@bash scripts/runner-setup.sh go-tool github.com/pressly/goose/v3/cmd/goose $(GOOSE_VERSION) goose
	@bash scripts/runner-setup.sh node-deps .
	@bash scripts/runner-setup.sh node-deps "$(APP_DIR)"

clean:
	rm -rf tmp

help:
	@echo "Available targets:"
	@echo "  dev          Run Air with dev logging and combined log rotation"
	@echo "  generate     Run go generate, templ, sqlc, Tailwind, and the conversation client"
	@echo "  generate-docs  Generate config references"
	@echo "  assets       Build dashboard CSS and the conversation client"
	@echo "  css          Build Tailwind CSS"
	@echo "  css-watch    Watch and rebuild Tailwind CSS"
	@echo "  app          Build the conversation client into static/app/conversation"
	@echo "  app-dev      Run the conversation client dev server"
	@echo "  app-test     Typecheck and test the conversation client"
	@echo "  check-app    Client typecheck, design token and catalog checks, tests, build and attribution"
	@echo "  app-contract Decode Go board handler payloads with the client contract"
	@echo "  build        Build $(BINARY_NAME)"
	@echo "  test         Run Go tests"
	@echo "  test-race    Run the short unit suite under the race detector"
	@echo "  test-cover   Run Go tests with a coverage profile as evidence (no floor)"
	@echo "  soak         Run opt-in orchestrator incident and adversarial soak tests"
	@echo "  visual-e2e   Run Playwright browser journeys"
	@echo "  lint         Run golangci-lint"
	@echo "  security     Run govulncheck and gosec security scans"
	@echo "  check        Run local migrations, generated, lint, vet, short tests, and build"
	@echo "  check-landing  Run diff-scoped landing build, short tests, and lint"
	@echo "  check-release  Run full release validation suite"
	@echo "  test-fast    Run the whole-repository short unit suite"
	@echo "  modernize-check  Run the Go modernizer diff check"
	@echo "  release-snapshot  Build local GoReleaser snapshot archives"
	@echo "  sqlc         Generate sqlc output"
	@echo "  db-create    Create a UTC timestamp SQL migration (NAME=description)"
	@echo "  db-migrate   Run goose migrations, including later-landed timestamps"
	@echo "  setup        Install development tools"
