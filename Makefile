SHELL := /bin/bash

BINARY_NAME := detent
BINARY_PATH := tmp/$(BINARY_NAME)
CMD_PACKAGE := ./cmd/detent
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
DEV_VERSION ?= dev
COVERPROFILE := tmp/coverage.out
COVERPROFILE_RAW := tmp/coverage.raw.out
COVERAGE_THRESHOLD := 70.0
MODERNIZE_FIX_FLAGS ?= -newexpr=false
TEMPL ?= go run github.com/a-h/templ/cmd/templ@v0.3.1001
TAILWIND_INPUT ?= static/css/input.css
TAILWIND_OUTPUT ?= static/css/output.css
SQLC_CONFIG ?= sqlc/sqlc.yaml
MIGRATIONS_DIR ?= internal/store/migrations
GOOSE_DRIVER ?= sqlite3
DATABASE_URL ?= tmp/detent.db
NILAWAY_VERSION ?= v0.0.0-20260612163715-2d8907f431ca
NILAWAY ?= go run go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)
NILAWAY_INCLUDE_PKGS ?= github.com/digitaldrywood/detent

.PHONY: dev generate css css-watch build test test-race test-cover lint vet check modernize-check nilaway-audit release-snapshot sqlc db-migrate setup clean help

dev:
	@mkdir -p tmp
	@if [ -f tmp/air-combined.log ]; then \
		mv tmp/air-combined.log tmp/air-combined-$$(date +%Y%m%d-%H%M%S).log; \
	fi
	@ls -t tmp/air-combined-*.log 2>/dev/null | tail -n +6 | xargs rm -f 2>/dev/null || true
	@ENV=dev LOG_LEVEL=debug DETENT_AIR_VERSION=$(DEV_VERSION) air 2>&1 | tee tmp/air-combined.log

generate:
	@go generate ./...
	@if [ -n "$$(git ls-files --others --exclude-standard -- '*.templ'; git ls-files -- '*.templ')" ]; then \
		$(TEMPL) generate; \
	else \
		echo "No templ files found; skipping templ generate."; \
	fi
	@$(MAKE) sqlc
	@$(MAKE) css

css:
	@if [ -f "$(TAILWIND_INPUT)" ]; then \
		if [ -f package-lock.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm ci; elif [ -f package.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm install; fi; \
		mkdir -p "$$(dirname "$(TAILWIND_OUTPUT)")"; \
		node_modules/.bin/tailwindcss -i "$(TAILWIND_INPUT)" -o "$(TAILWIND_OUTPUT)" --minify; \
	else \
		echo "No Tailwind input at $(TAILWIND_INPUT); skipping CSS build."; \
	fi

css-watch:
	@if [ -f "$(TAILWIND_INPUT)" ]; then \
		if [ -f package-lock.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm ci; elif [ -f package.json ] && [ ! -x node_modules/.bin/tailwindcss ]; then npm install; fi; \
		mkdir -p "$$(dirname "$(TAILWIND_OUTPUT)")"; \
		node_modules/.bin/tailwindcss -i "$(TAILWIND_INPUT)" -o "$(TAILWIND_OUTPUT)" --watch; \
	else \
		echo "No Tailwind input at $(TAILWIND_INPUT); skipping CSS watch."; \
	fi

build: generate
	@mkdir -p tmp
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_PATH) $(CMD_PACKAGE)

test:
	go test ./...

test-race:
	go test -race ./...

test-cover:
	@mkdir -p tmp
	go test -coverprofile=$(COVERPROFILE_RAW) ./...
	@awk 'NR == 1 || ($$1 !~ /_templ\.go:/ && $$1 !~ /\/internal\/store\/sqlc\// && $$1 !~ /\/internal\/database\/sqlc\//)' "$(COVERPROFILE_RAW)" > "$(COVERPROFILE)"
	@coverage="$$(go tool cover -func=$(COVERPROFILE) | awk '/^total:/ { gsub(/%/, "", $$3); print $$3 }')"; \
	awk -v coverage="$$coverage" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN { \
		if (coverage + 0 < threshold + 0) { \
			printf "coverage %.1f%% is below %.1f%%\n", coverage, threshold; \
			exit 1; \
		} \
		printf "coverage %.1f%% meets %.1f%% threshold\n", coverage, threshold; \
	}'

lint:
	golangci-lint run --timeout=5m

vet:
	go vet ./...

nilaway-audit:
	$(NILAWAY) -include-pkgs=$(NILAWAY_INCLUDE_PKGS) ./...

check: build lint vet nilaway-audit test-race test-cover
	@echo "All checks passed."

modernize-check:
	go fix -diff $(MODERNIZE_FIX_FLAGS) ./...

release-snapshot:
	goreleaser release --snapshot --clean

sqlc:
	@if [ -f "$(SQLC_CONFIG)" ]; then \
		sqlc generate -f "$(SQLC_CONFIG)"; \
	else \
		echo "No sqlc config at $(SQLC_CONFIG); skipping sqlc generate."; \
	fi

db-migrate:
	@if [ -d "$(MIGRATIONS_DIR)" ]; then \
		mkdir -p "$$(dirname "$(DATABASE_URL)")"; \
		goose -dir "$(MIGRATIONS_DIR)" "$(GOOSE_DRIVER)" "$(DATABASE_URL)" up; \
	else \
		echo "No migrations directory at $(MIGRATIONS_DIR); skipping database migration."; \
	fi

setup:
	go install github.com/air-verse/air@latest
	go install github.com/a-h/templ/cmd/templ@v0.3.1001
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6
	@if [ -f package.json ]; then npm install; fi

clean:
	rm -rf tmp

help:
	@echo "Available targets:"
	@echo "  dev          Run Air with dev logging and combined log rotation"
	@echo "  generate     Run go generate, templ, sqlc, and Tailwind"
	@echo "  css          Build Tailwind CSS"
	@echo "  css-watch    Watch and rebuild Tailwind CSS"
	@echo "  build        Build $(BINARY_NAME)"
	@echo "  test         Run Go tests"
	@echo "  test-race    Run Go tests with the race detector"
	@echo "  test-cover   Run Go coverage with a $(COVERAGE_THRESHOLD)% minimum"
	@echo "  lint         Run golangci-lint"
	@echo "  check        Run the local validation gate, including NilAway"
	@echo "  modernize-check  Run the Go modernizer diff check"
	@echo "  nilaway-audit  Run the local NilAway audit"
	@echo "  release-snapshot  Build local GoReleaser snapshot archives"
	@echo "  sqlc         Generate sqlc output"
	@echo "  db-migrate   Run goose migrations"
	@echo "  setup        Install development tools"
