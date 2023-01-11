## Docker Build Versions
DOCKER_BUILD_IMAGE = golang:1.14.6
DOCKER_BASE_IMAGE = alpine:3.12
MATTERWICK_IMAGE ?= mattermost/matterwick:test


GO ?= $(shell command -v go 2> /dev/null)
DEP ?= $(shell command -v dep 2> /dev/null)

PACKAGES=$(shell go list ./...)

TOOLS_BIN_DIR := $(abspath bin)
GO_INSTALL = ./scripts/go_install.sh

GOLINT_VER := master
GOLINT_BIN := golint
GOLINT_GEN := $(TOOLS_BIN_DIR)/$(GOLINT_BIN)


## Checks the code style, tests, builds and bundles the plugin.
.PHONY: all
all: check-style test

## Runs govet and gofmt against all packages.
.PHONY: check-style
check-style: lint vet
	@echo Checking for style guide compliance

.PHONY: build
build:
	@echo Building
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -gcflags all=-trimpath=$(PWD) -asmflags -a -installsuffix cgo -o build/_output/bin/matterwick

.PHONY: build-image
build-image:  ## Build the docker image for matterwick
	@echo Building matterwick Image
	docker build \
	--build-arg DOCKER_BUILD_IMAGE=$(DOCKER_BUILD_IMAGE) \
	--build-arg DOCKER_BASE_IMAGE=$(DOCKER_BASE_IMAGE) \
	. -f Dockerfile -t $(MATTERWICK_IMAGE) \
	--no-cache

## Runs lint against all packages.
.PHONY: lint
lint: $(GOLINT_GEN)
	@echo Running lint
	$(GOLINT_GEN) -set_exit_status $(PACKAGES)
	@echo lint success

## Runs govet against all packages.
.PHONY: vet
vet:
	@echo Running govet
	$(GO) vet ./...
	@echo Govet success

## Runs tests. For local usage, run `make test CONFIG_TEST="-config=config-matterwick.test-local.json"`
test:
	@echo Running Go tests
	$(GO) test $(PACKAGES) $(CONFIG_TEST)
	@echo test success

$(GOLINT_GEN): ## Build golint.
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) golang.org/x/lint/golint $(GOLINT_BIN) $(GOLINT_VER)

# Help documentation à la https://marmelab.com/blog/2016/02/29/auto-documented-makefile.html
help:
	@cat Makefile | grep -v '\.PHONY' |  grep -v '\help:' | grep -B1 -E '^[a-zA-Z_.-]+:.*' | sed -e "s/:.*//" | sed -e "s/^## //" |  grep -v '\-\-' | sed '1!G;h;$$!d' | awk 'NR%2{printf "\033[36m%-30s\033[0m",$$0;next;}1' | sort
