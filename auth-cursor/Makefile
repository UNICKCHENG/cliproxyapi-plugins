GO ?= go
NPM ?= npm

# VERSION is compiled into the library and keys the sidecar bootstrap directory.
VERSION ?= 0.0.0-dev

GOOS := $(shell $(GO) env GOOS)
GOARCH := $(shell $(GO) env GOARCH)

ifeq ($(GOOS),windows)
PLUGIN_EXT := dll
else ifeq ($(GOOS),darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

# PLUGIN_ID is the distribution id. The host derives the plugin id, and therefore the
# plugins.configs key, from the library file name, so this must match registry.json.
PLUGIN_ID := auth-cursor
DIST_DIR := $(CURDIR)/dist
PLUGIN_LIB := $(DIST_DIR)/$(PLUGIN_ID).$(PLUGIN_EXT)
ARCHIVE := $(DIST_DIR)/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip

# INSTALL_DIR points at a CLIProxyAPI checkout's plugins tree for local development.
INSTALL_DIR ?=
INSTALL_PLUGIN_DIR := $(INSTALL_DIR)/$(GOOS)/$(GOARCH)
INSTALL_SIDECAR_DIR := $(INSTALL_PLUGIN_DIR)/$(PLUGIN_ID)-sidecar

LDFLAGS := -X main.pluginVersion=$(VERSION)

.PHONY: build release install sidecar-deps test fmt clean

## build compiles the C-ABI shared library the host loads with dlopen.
build: | $(DIST_DIR)
	cd go && CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o "$(PLUGIN_LIB)" .
	rm -f "$(DIST_DIR)/$(PLUGIN_ID).h"

## release packages the library for the plugin store: the archive root must hold the
## dynamic library and nothing else, because the host extracts only that file.
release: build
	cd "$(DIST_DIR)" && rm -f "$(notdir $(ARCHIVE))" && zip -q -X "$(notdir $(ARCHIVE))" "$(PLUGIN_ID).$(PLUGIN_EXT)"
	@echo "$(ARCHIVE)"

## install copies the library and a ready-to-run sidecar into a CLIProxyAPI checkout so a
## development host skips the home-directory bootstrap.
install: build sidecar-deps
	@test -n "$(INSTALL_DIR)" || { echo "set INSTALL_DIR=<CLIProxyAPI>/plugins"; exit 1; }
	mkdir -p "$(INSTALL_PLUGIN_DIR)" "$(INSTALL_SIDECAR_DIR)"
	cp "$(PLUGIN_LIB)" "$(INSTALL_PLUGIN_DIR)/"
	cp go/sidecar/index.mjs go/sidecar/http-bridge.mjs go/sidecar/package.json "$(INSTALL_SIDECAR_DIR)/"
	cp -R go/sidecar/node_modules "$(INSTALL_SIDECAR_DIR)/"

## sidecar-deps installs the sidecar dependencies in the source tree for local runs.
sidecar-deps:
	cd go/sidecar && $(NPM) install --omit=dev --no-audit --no-fund

test:
	cd go && $(GO) test ./...

fmt:
	cd go && gofmt -w .

$(DIST_DIR):
	mkdir -p "$(DIST_DIR)"

clean:
	rm -rf "$(DIST_DIR)"
