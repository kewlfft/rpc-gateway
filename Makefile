# Binary name
BINARY_NAME=rpcgateway
VERSION=0.5.0

# Git information - with fallbacks
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Optimized build flags - maximum performance, no test dependencies
LDFLAGS=-s -w -extldflags '-static'
BUILD_FLAGS=-trimpath -gcflags="-l=4 -B -C -m=2" -asmflags="-trimpath" -tags=!test -race=false
VERSION_FLAGS=-X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: all build clean test lint

all: clean build

build:
	@echo "Building optimized $(BINARY_NAME) version $(VERSION) (maximum performance, no test deps)"
	@echo "Git commit: $(GIT_COMMIT)"
	@echo "Build time: $(BUILD_TIME)"
	CGO_ENABLED=0 GOAMD64=v3 go build $(BUILD_FLAGS) -ldflags="$(LDFLAGS) $(VERSION_FLAGS)" -o $(BINARY_NAME) main.go

clean:
	rm -f $(BINARY_NAME)
	go clean

test:
	go test -v -race ./...

lint:
	golangci-lint run

.DEFAULT_GOAL := build 