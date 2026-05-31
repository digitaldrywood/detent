SHELL := /bin/bash

BINARY_NAME := symphony
BINARY_PATH := tmp/$(BINARY_NAME)
CMD_PACKAGE := ./cmd/symphony
COVERPROFILE := tmp/coverage.out
COVERPROFILE_RAW := tmp/coverage.raw.out
COVERAGE_THRESHOLD := 70.0
TEMPL ?= go run github.com/a-h/templ/cmd/templ@v0.3.1001
TAILWIND_INPUT ?= static/css/input.css
TAILWIND_OUTPUT ?= static/css/output.css
SQLC_CONFIG ?= sqlc/sqlc.yaml
MIGRATIONS_DIR ?= internal/database/migrations
GOOSE_DRIVER ?= sqlite3
DATABASE_URL ?= tmp/symphony.db

.PHONY: dev generate css css-watch build test test-race test-cover lint vet check sqlc db-migrate setup clean help

dev:
	@mkdir -p tmp
	@if [ -f tmp/air-combined.log ]; then \
		mv tmp/air-combined.log tmp/air-combined-$$(date +%Y%m%d-%H%M%S).log; \
	fi
	@ls -t tmp/air-combined-*.log 2>/dev/null | tail -n +6 | xargs rm -f 2>/dev/null || true
	@air 2>&1 | tee tmp/air-combined.log

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
	go build -o $(BINARY_PATH) $(CMD_PACKAGE)

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

check: build lint vet test-race test-cover
	@echo "All checks passed."

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
	@echo "  dev          Run Air with combined log rotation"
	@echo "  generate     Run go generate, templ, sqlc, and Tailwind"
	@echo "  css          Build Tailwind CSS"
	@echo "  css-watch    Watch and rebuild Tailwind CSS"
	@echo "  build        Build $(BINARY_NAME)"
	@echo "  test         Run Go tests"
	@echo "  test-race    Run Go tests with the race detector"
	@echo "  test-cover   Run Go coverage with a $(COVERAGE_THRESHOLD)% minimum"
	@echo "  lint         Run golangci-lint"
	@echo "  check        Run the local validation gate"
	@echo "  sqlc         Generate sqlc output"
	@echo "  db-migrate   Run goose migrations"
	@echo "  setup        Install development tools"
