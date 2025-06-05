# Binary name
BINARY_NAME=rpcgateway

# Build flags
LDFLAGS=-s -w
VERSION=0.1.0

# Git information - with fallbacks
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Build flags for optimal performance
BUILD_FLAGS=-v -trimpath -buildmode=pie -tags=netgo -gcflags="-l=4" -asmflags="-trimpath" -race=false
VERSION_FLAGS=-X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: all build clean test lint

all: clean build

build:
	@echo "Building $(BINARY_NAME) version $(VERSION)"
	@echo "Git commit: $(GIT_COMMIT)"
	@echo "Build time: $(BUILD_TIME)"
	GOAMD64=v3 go build $(BUILD_FLAGS) -ldflags="$(LDFLAGS) $(VERSION_FLAGS)" -o $(BINARY_NAME) main.go

clean:
	rm -f $(BINARY_NAME)
	go clean

test:
	go test -v -race ./...

lint:
	golangci-lint run

.DEFAULT_GOAL := build 