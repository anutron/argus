.PHONY: build vet test test-watch test-cover test-pkg lint-pr

build:
	go build ./...

vet:
	go vet ./...

# Run golangci-lint the same way CI does — only flag issues introduced by
# this branch's diff vs origin/master. Use before pushing to catch lint
# failures locally instead of via a CI round-trip.
lint-pr:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Install: brew install golangci-lint OR go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	@git fetch origin master >/dev/null 2>&1 || true
	golangci-lint run --new-from-rev=origin/master ./...

test:
	go test -race -count=1 ./...

test-watch:
	@command -v gotestsum >/dev/null 2>&1 || { echo "Install gotestsum: go install gotest.tools/gotestsum@latest"; exit 1; }
	gotestsum --watch ./...

test-cover:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

test-pkg:
	@test -n "$(PKG)" || { echo "Usage: make test-pkg PKG=./internal/db/"; exit 1; }
	go test -race -count=1 -v $(PKG)
