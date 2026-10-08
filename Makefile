GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fmt lint test test/race
build:
	mkdir -p dist
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/mailvault ./cmd/mailvault
fmt:
	gofmt -w .
lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	golangci-lint run
test:
	$(GO) test -count=1 ./...
test/race:
	$(GO) test -race -count=1 ./...
