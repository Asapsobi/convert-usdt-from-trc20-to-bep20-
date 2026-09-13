.PHONY: build test test-integration migrate-up migrate-down lint

build:
	go build ./...

# Unit tests only. Integration-tagged files (build tag "integration") are
# excluded automatically -- they need a real Postgres instance.
test:
	go test ./...

# Requires RELAYD_TEST_DATABASE_URL and LEDGER_TEST_DATABASE_URL (the
# latter for internal/orchestrate's own real-ledgerd ship-gate test,
# internal/testledger builds and runs ledger/cmd/ledgerd as a
# subprocess), e.g.:
#   export RELAYD_TEST_DATABASE_URL=postgres://relayd:relayd@localhost:5440/relayd_test
#   export LEDGER_TEST_DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger_test
#
# -p 1 (serialize package test binaries) is required here, unlike every
# sibling service's own otherwise-identical target: this module's own
# internal/orchestrate integration test creates real C1 orders against a
# SHARED ledger_test database, and Go's default per-package test
# parallelism would let it race against a different concurrently-running
# package's own writes to that same external database -- found directly,
# not theoretical: without -p 1 this exact test intermittently failed
# against leftover/concurrent rows from other packages' own test runs.
test-integration:
	go test -tags=integration -p 1 ./...

# Requires RELAYD_DATABASE_URL. Uses our own cmd/migrate rather than
# goose's CLI binary, for the same reason as every sibling service: it
# would drag a driver for every database goose supports into go.sum
# unnecessarily.
migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

# Falls back to gofmt + go vet when golangci-lint isn't installed, rather
# than hard-failing the target on a missing tool.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found, falling back to gofmt + go vet"; \
		test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1); \
		go vet ./...; \
	fi
