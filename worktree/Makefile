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
SQLC_CONFIG ?= sqlc/sqlc.yaml
MIGRATIONS_DIR ?= internal/store/migrations
GOOSE_DRIVER ?= sqlite3
GOOSE_VERSION := v3.26.0
DATABASE_URL ?= tmp/detent.db
NILAWAY_VERSION ?= v0.0.0-20260612163715-2d8907f431ca
NILAWAY ?= go run go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)
NILAWAY_INCLUDE_PKGS ?= github.com/digitaldrywood/detent
NILAWAY_PACKAGES ?= ./...
GOVULNCHECK_VERSION ?= v1.6.0
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
# Upstream issue: https://github.com/securego/gosec/issues/1712
GOSEC_VERSION ?= v2.28.0
GOSEC_BINARY ?= tmp/gosec-$(GOSEC_VERSION)-deterministic
GOSEC_PATCH ?= scripts/gosec-v2.28.0-deterministic.patch
GOSEC_DETERMINISM_RUNS ?= 8
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
# G115: existing conversions are bounded or intentionally preserve platform/API widths.
# G301: shared runtime, service, and artifact directories intentionally require traversal access.
# G304: Detent intentionally reads operator-selected config, workflow, and workspace paths.
# G306: flagged files are non-secret configs, manifests, screenshots, and generated artifacts.
GOSEC_EXCLUDES ?= G115,G301,G304,G306,G703,G704
GOSEC_EXCLUDE_DIRS ?= .detent
GOSEC_EXCLUDE_DIR_FLAGS := $(addprefix -exclude-dir=,$(GOSEC_EXCLUDE_DIRS))
.PHONY: dev generate check-migrations check-generated css css-watch app app-dev app-test check-app build test test-fast test-race coverage-check test-cover soak visual-e2e lint vet gosec-build security-gosec-determinism check check-fast check-land check-release modernize-check nilaway-audit nilaway-changed source-metadata release-snapshot sqlc db-create db-migrate setup clean help

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
	python3 scripts/check-runner-enrollment-docs.py

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
		(cd "$(APP_DIR)" && npm run typecheck && npm run design:tokens:check && npm run design:catalog:check && npx vitest run --maxWorkers=$(TEST_PROCS) && npm run build); \
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
	@if [ -f "$(APP_DIR)/package.json" ]; then \
		if [ ! -d "$(APP_DIR)/node_modules" ]; then (cd "$(APP_DIR)" && npm ci); fi; \
		(cd "$(APP_DIR)" && npm run typecheck && npm test); \
	else \
		echo "No conversation client at $(APP_DIR); skipping app tests."; \
	fi

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
	bash scripts/test-workspace.sh
	@packages="$$(go list ./...)" && \
	packages="$$(printf '%s\n' "$$packages" | awk '$$0 != "github.com/digitaldrywood/detent/internal/workspace" && $$0 != "github.com/digitaldrywood/detent/internal/web"')" && \
	$(GO_TEST) $$packages
	# Measure the 500ms request budget without competing package tests or builds.
	$(GO_TEST) ./internal/web

test-fast: generate-docs
	$(GO_TEST) -short -timeout=60s ./...

# Hub server tests split into slices balanced by measured time; slice 4 catches every other name.
HUB_SLICE_1 := -run '^Test([BDEFILQTUV]|Normalize|Native[A-H])'
HUB_SLICE_2 := -run '^Test[HMP]'
HUB_SLICE_3 := -run '^Test[CGS]'
HUB_SLICE_4 := -skip '^Test[ABCDEFGHILMNPQSTUV]'
HUB_SLICE_5 := -run '^Test(A|Native[I-Z])'

.PHONY: test-landing-hub
test-landing-hub:
	@set -e; source scripts/check-evidence.sh; \
	$(foreach slice,$(sort $(filter HUB_SLICE_%,$(.VARIABLES))),check_with_evidence unit-full-hub-$(patsubst HUB_SLICE_%,%,$(slice)) env -u DETENT_API_TOKEN python3 scripts/test-landing-hub.py $(patsubst HUB_SLICE_%,%,$(slice)) $(TEST_PROCS) $(TEST_TIMEOUT) $($(slice));)

# RACE_SHARD=i/n races one balanced slice of the packages outside the hub server; RACE_SHARD=hub-N races hub server slice N.
RACE_SHARD ?=
RACE_HUB_SLICE := $(patsubst hub-%,%,$(filter hub-%,$(RACE_SHARD)))

test-race: generate-docs
	@if [ -z "$(RACE_SHARD)" ]; then \
		$(GO_TEST) -short -race -timeout=30m ./...; \
	elif [ -n "$(RACE_HUB_SLICE)" ]; then \
		$(GO_TEST) -short -race -timeout=30m ./internal/hubserver $(HUB_SLICE_$(RACE_HUB_SLICE)); \
	else \
		packages="$$(bash scripts/race-packages.sh $(RACE_SHARD))"; \
		$(GO_TEST) -short -race -timeout=30m $$packages; \
	fi

# COVER_SHARD=i/n runs one balanced slice; shard 1 also owns workspace and web.
# COVER_SHARD=hub-N covers hub server slice N; COVER_SKIP_HUB=1 leaves the hub server to those shards.
COVER_SHARD ?= 1/1
COVER_SKIP_HUB ?=
COVER_HUB_SLICE := $(patsubst hub-%,%,$(filter hub-%,$(COVER_SHARD)))

ifneq ($(COVER_HUB_SLICE),)
test-cover:
	@mkdir -p tmp
	$(GO_TEST) -coverprofile=$(COVERPROFILE_RAW) ./internal/hubserver $(HUB_SLICE_$(COVER_HUB_SLICE))
	@$(MAKE) coverage-check
else
test-cover:
	@mkdir -p tmp
	@rm -f tmp/workspace-cover.raw.out tmp/rest-cover.raw.out tmp/web-cover.raw.out tmp/hub-{1,2,3,4,5}-cover.raw.out tmp/hub-cover.raw.out tmp/packages-cover.raw.out
	@if [ "$(firstword $(subst /, ,$(COVER_SHARD)))" = 1 ]; then \
		bash scripts/test-workspace.sh -coverprofile tmp/workspace-cover.raw.out -output tmp/workspace-cover-evidence; \
	fi
	@set -e; packages="$$(COVER_SKIP_HUB=$(COVER_SKIP_HUB) bash scripts/cover-packages.sh $(COVER_SHARD))"; \
	hub=; remaining=; profiles=; \
	for package in $$packages; do \
		case "$$package" in \
			github.com/digitaldrywood/detent/internal/hubserver) hub=1 ;; \
			*) remaining="$$remaining $$package" ;; \
		esac; \
	done; \
	if [ -n "$$hub" ]; then \
		$(GO_TEST) -coverprofile=tmp/hub-1-cover.raw.out ./internal/hubserver $(HUB_SLICE_1); \
		$(GO_TEST) -coverprofile=tmp/hub-2-cover.raw.out ./internal/hubserver $(HUB_SLICE_2); \
		$(GO_TEST) -coverprofile=tmp/hub-3-cover.raw.out ./internal/hubserver $(HUB_SLICE_3); \
		$(GO_TEST) -coverprofile=tmp/hub-4-cover.raw.out ./internal/hubserver $(HUB_SLICE_4); \
		$(GO_TEST) -coverprofile=tmp/hub-5-cover.raw.out ./internal/hubserver $(HUB_SLICE_5); \
		go run ./tools/covermerge -union tmp/hub-1-cover.raw.out tmp/hub-2-cover.raw.out tmp/hub-3-cover.raw.out tmp/hub-4-cover.raw.out tmp/hub-5-cover.raw.out > tmp/hub-cover.raw.out; \
		profiles=tmp/hub-cover.raw.out; \
	fi; \
	if [ -n "$$remaining" ]; then \
		$(GO_TEST) -coverprofile=tmp/packages-cover.raw.out $$remaining; \
		profiles="$$profiles tmp/packages-cover.raw.out"; \
	fi; \
	set -- $$profiles; \
	if [ "$$#" -eq 1 ]; then cp "$$1" tmp/rest-cover.raw.out; \
	else go run ./tools/covermerge "$$@" > tmp/rest-cover.raw.out; fi
	# Keep the web timing check isolated and coverage profiles disjoint.
	@if [ "$(firstword $(subst /, ,$(COVER_SHARD)))" = 1 ]; then \
		$(GO_TEST) -coverprofile=tmp/web-cover.raw.out ./internal/web && \
		go run ./tools/covermerge tmp/workspace-cover.raw.out tmp/rest-cover.raw.out tmp/web-cover.raw.out > $(COVERPROFILE_RAW); \
	else \
		cp tmp/rest-cover.raw.out $(COVERPROFILE_RAW); \
	fi
	@$(MAKE) coverage-check
endif

coverage-check:
	@awk 'NR == 1 || ($$1 !~ /_templ\.go:/ && $$1 !~ /\/internal\/store\/sqlc\// && $$1 !~ /\/internal\/database\/sqlc\//)' "$(COVERPROFILE_RAW)" > "$(COVERPROFILE)"
	@go tool cover -func=$(COVERPROFILE) | awk '/^total:/ { printf "coverage %s of statements (evidence only, no floor)\n", $$3 }'

soak:
	DETENT_RUN_SOAK_TESTS=1 $(GO_TEST) ./internal/orchestrator -run '^(TestSoak|TestDispatchParityAdversarialFixtures)' -count=1

visual-e2e: build
	@if [ ! -x node_modules/.bin/playwright ]; then npm ci; fi
	DETENT_BINARY="$(CURDIR)/$(BINARY_PATH)" node_modules/.bin/playwright test

lint: $(GOLANGCI_LINT)
	GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" "$(GOLANGCI_LINT)" run --allow-parallel-runners --concurrency=$(TEST_PROCS) --timeout=15m

$(GOLANGCI_LINT):
	@bash scripts/runner-setup.sh go-tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint $(GOLANGCI_LINT_VERSION) "$(GOLANGCI_LINT)" "$(GOLANGCI_LINT_TOOLCHAIN)"

vet:
	go vet -p $(TEST_PROCS) ./...

gosec-build:
	./scripts/build-gosec.sh "$(GOSEC_VERSION)" "$(GOSEC_PATCH)" "$(GOSEC_BINARY)"

security-gosec-determinism: gosec-build
	./scripts/check-gosec-determinism.sh "$(GOSEC_BINARY)" "$(GOSEC_DETERMINISM_RUNS)"

security: security-gosec-determinism
	$(GOVULNCHECK) ./...
	$(GOSEC_BINARY) -quiet -exclude-generated -severity=medium -confidence=medium -exclude=$(GOSEC_EXCLUDES) $(GOSEC_EXCLUDE_DIR_FLAGS) ./...

nilaway-audit:
	$(NILAWAY) -include-pkgs=$(NILAWAY_INCLUDE_PKGS) ./...

nilaway-changed:
	python3 scripts/nilaway-changed.py $(NILAWAY_VERSION) $(NILAWAY_INCLUDE_PKGS) $(NILAWAY_PACKAGES)

check: check-invariants check-migrations check-generated check-app build lint vet nilaway-audit test-race test-cover
	@echo "All checks passed."

check-land:
	NILAWAY_VERSION="$(NILAWAY_VERSION)" NILAWAY_INCLUDE_PKGS="$(NILAWAY_INCLUDE_PKGS)" bash scripts/check-land.sh "$(CHECK_LAND_BASE)" "$(APP_DIR)" "$(TEST_PROCS)"

.PHONY: check-release
check-release:
	bash scripts/check-release.sh "$(TEST_PROCS)"

.PHONY: check-landing
check-landing:
	bash scripts/check-landing.sh "$(CHECK_LAND_BASE)" "$(TEST_PROCS)" "$(GOLANGCI_LINT)"

check-fast: check-invariants check-migrations check-generated check-app lint vet test-fast
	go build ./...
	@echo "Fast checks passed."

.PHONY: check-invariants
check-invariants:
	go run ./tools/invariantcheck

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
	@echo "  build        Build $(BINARY_NAME)"
	@echo "  test         Run Go tests"
	@echo "  test-race    Run the short unit suite under the race detector"
	@echo "  test-cover   Run Go tests with a coverage profile as evidence (no floor)"
	@echo "  soak         Run opt-in orchestrator incident and adversarial soak tests"
	@echo "  visual-e2e   Run Playwright browser journeys"
	@echo "  lint         Run golangci-lint"
	@echo "  security-gosec-determinism  Verify deterministic G705 caller traversal"
	@echo "  security     Run govulncheck and gosec security scans"
	@echo "  check-land   Run the pre-landing lint, vet, build and short-test gate"
	@echo "  test-fast    Run the whole-repository short unit suite"
	@echo "  check        Run optional full diagnostics, including NilAway"
	@echo "  modernize-check  Run the Go modernizer diff check"
	@echo "  nilaway-audit  Run the local NilAway audit"
	@echo "  release-snapshot  Build local GoReleaser snapshot archives"
	@echo "  sqlc         Generate sqlc output"
	@echo "  db-create    Create a UTC timestamp SQL migration (NAME=description)"
	@echo "  db-migrate   Run goose migrations, including later-landed timestamps"
	@echo "  setup        Install development tools"
