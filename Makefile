.PHONY: build test lint lint-fix fmt tidy clean docker up down

BINARY := forgesync
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X git.erwanleboucher.dev/eleboucher/forgesync/internal/version.Version=$(VERSION)"

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -trimpath -o bin/$(BINARY) ./cmd/forgesync

test:
	go test -race -count=1 -timeout 60s ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

fmt:
	golangci-lint fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/

docker:
	docker build --build-arg VERSION=$(VERSION) -t forgesync:$(VERSION) .

up:
	docker compose up --build -d

down:
	docker compose down
