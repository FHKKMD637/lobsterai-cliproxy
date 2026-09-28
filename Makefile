# Build the LobsterAI CPA plugin as a dynamic library.
#
#   make            # build for the host platform
#   make linux      # cross-compile for linux/amd64 (needs a linux cross toolchain)
#   make install    # build and copy into the CPA plugins directory
#
# CPA_DIR can be overridden: make install CPA_DIR=/path/to/CLIProxyAPI

CPA_DIR ?= /path/to/CLIProxyAPI

GOOS_LOCAL   := $(shell go env GOOS)
GOARCH_LOCAL := $(shell go env GOARCH)
EXT_darwin   := dylib
EXT_linux    := so
EXT_windows  := dll

# -trimpath keeps local filesystem paths out of the shipped binary.
GOFLAGS ?= -trimpath

.PHONY: all build linux install test clean

all: build

build:
	CGO_ENABLED=1 go build $(GOFLAGS) -buildmode=c-shared -o lobsterai.$(EXT_$(GOOS_LOCAL)) .
	@echo "built lobsterai.$(EXT_$(GOOS_LOCAL))"

linux:
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=x86_64-linux-gnu-gcc \
		go build $(GOFLAGS) -buildmode=c-shared -o lobsterai.so .
	@echo "built lobsterai.so (linux/amd64)"

install: build
	mkdir -p $(CPA_DIR)/plugins/$(GOOS_LOCAL)/$(GOARCH_LOCAL)
	cp lobsterai.$(EXT_$(GOOS_LOCAL)) $(CPA_DIR)/plugins/$(GOOS_LOCAL)/$(GOARCH_LOCAL)/
	@echo "installed to $(CPA_DIR)/plugins/$(GOOS_LOCAL)/$(GOARCH_LOCAL)/"

test:
	go vet ./...
	go test ./...

clean:
	rm -f lobsterai.dylib lobsterai.so lobsterai.dll lobsterai.h
