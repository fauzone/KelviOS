.PHONY: build run test vet clean webui

BINARY_NAME=kelvios-core
BIN_DIR=bin
BUILD_DIR=$(BIN_DIR)
VERSION?=dev
LDFLAGS=-s -w -X main.version=$(VERSION)
GOFLAGS=CGO_ENABLED=0

build:
	@mkdir -p $(BUILD_DIR)
	$(GOFLAGS) go build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/kelvios-core

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

test:
	$(GOFLAGS) go test -v ./...

vet:
	$(GOFLAGS) go vet ./...

webui:
	cd webapp && npm run build && cd .. && bash scripts/sync-webui.sh

clean:
	rm -rf $(BUILD_DIR)

tidy:
	go mod tidy

check: vet test

.DEFAULT_GOAL := build