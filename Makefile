TEST ?= $$(go list ./...)
APP_NAME = tofu-api
BUILD_DIR = $(PWD)/dist
CONFIG ?=
SHELL := /bin/bash

all: tidy fmt lint security test build

setup:
	@command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@command -v gosec >/dev/null 2>&1 || go install github.com/securego/gosec/v2/cmd/gosec@latest
	@command -v goreleaser >/dev/null 2>&1 || go install github.com/goreleaser/goreleaser/v2@latest

clean:
	rm -rf $(BUILD_DIR) cover.out

tidy:
	go mod tidy

fmt:
	gofmt -w $$(find . -name '*.go')

lint:
	golangci-lint run

security:
	gosec -quiet ./...

test:
	go test -race -timeout 120s -coverprofile=cover.out -cover $(TEST)
	go tool cover -func=cover.out

build:
	goreleaser build --snapshot --clean

# Binaries, archives, and the RPM and DEB packages in dist/, without
# publishing: a release is the CI's, from main.
snapshot:
	goreleaser release --snapshot --clean

run: build
	$(if $(CONFIG),,$(error make run CONFIG=<provider>.yaml))
	INFRAPI_CONFIG_DOTENV_FILE=app.env $(BUILD_DIR)/$(APP_NAME)_linux_amd64_v1/$(APP_NAME) serve -config $(CONFIG)

.PHONY: all setup clean tidy fmt lint security test build snapshot run
