BINARY  := kiln
PKG     := github.com/daiwa-zou/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X '$(PKG)/internal/observability.Version=$(VERSION)'

.PHONY: all build test test-verbose lint fmt tidy migrate dev clean

all: fmt test build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kiln

test:
	go test ./...

test-verbose:
	go test -v -race ./...

lint:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

migrate: build
	./bin/$(BINARY) admin migrate

dev: build
	./bin/$(BINARY) serve --with-worker

clean:
	rm -rf bin dist
