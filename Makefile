GO ?= go
BUF ?= buf
PNPM ?= pnpm
GOLANGCI_LINT ?= golangci-lint
SQLC ?= sqlc

SEARCH_API := apps/search-api
WEB_GUI := apps/web-gui

.DEFAULT_GOAL := build

# buf.yaml and buf.gen.yaml live at the repository root because generation
# writes into two different applications; running from anywhere else would
# need relative escapes in every out path.
# sqlc.yaml stays in the search API instead: its schema and query paths are
# module-relative, and only that module has any.
.PHONY: generate
generate:
	$(BUF) generate
	cd $(SEARCH_API) && $(SQLC) generate

.PHONY: install
install:
	cd $(WEB_GUI) && $(PNPM) install

.PHONY: build
build:
	cd $(SEARCH_API) && $(GO) build ./...
	cd $(WEB_GUI) && $(PNPM) build

.PHONY: test
test:
	cd $(SEARCH_API) && $(GO) test ./...
	cd $(WEB_GUI) && $(PNPM) test

.PHONY: lint
lint:
	$(BUF) lint
	cd $(SEARCH_API) && $(GO) vet ./...
	cd $(SEARCH_API) && $(GOLANGCI_LINT) run
	cd $(WEB_GUI) && $(PNPM) typecheck

.PHONY: fmt
fmt:
	$(BUF) format --write
	cd $(SEARCH_API) && $(GOLANGCI_LINT) fmt

.PHONY: tidy
tidy:
	cd $(SEARCH_API) && $(GO) mod tidy

# Needs a committed baseline: fails until api/ exists on the main branch.
.PHONY: proto-breaking
proto-breaking:
	$(BUF) breaking --against '.git#branch=main'
