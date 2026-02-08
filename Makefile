.PHONY: all build test e2e e2e-test clean install help

# Variables
BINARY_NAME=semerge
BUILD_DIR=bin
GO=go
GOFLAGS=-v
E2E_DIR=e2e/runner

# Default target
all: build

## build: Build the semerge binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/$(BINARY_NAME)

## install: Install semerge to $GOPATH/bin
install:
	@echo "Installing $(BINARY_NAME)..."
	$(GO) install $(GOFLAGS) ./cmd/$(BINARY_NAME)

## test: Run unit tests
test:
	@echo "Running unit tests..."
	$(GO) test -v ./...

## e2e-test: Run E2E tests
# Usage:
#   make e2e-test              # Run all tests
#   make e2e-test TEST=go      # Run all Go tests
#   make e2e-test TEST=go/rename_struct  # Run specific test
#   DEBUG=1 make e2e-test TEST=go/rename_struct  # Run with debug logging
e2e-test:
	@echo "Running E2E tests: $(TEST)..."
	@if [ -n "$(TEST)" ]; then \
		cd $(E2E_DIR) && $(GO) test -v -run "TestE2E/$(TEST).*"; \
	else \
		cd $(E2E_DIR) && $(GO) test -v; \
	fi

## clean: Clean build artifacts
clean:
	@echo "Cleaning..."
	$(GO) clean ./...
	rm -rf $(BUILD_DIR)

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build                    Build the semerge binary"
	@echo "  install                  Install semerge to \$$GOPATH/bin"
	@echo "  test                     Run unit tests"
	@echo "  e2e-test                 Run All E2E tests"
	@echo "                             TEST=go/rename_struct  - Run specific test"
	@echo "                             TEST=go                - Run all Go tests"
	@echo "                             DEBUG=1               - Enable debug logging"
	@echo "  clean                    Clean build artifacts"
	@echo "  help                     Show this help message"

# List all targets
.DEFAULT_GOAL := help
