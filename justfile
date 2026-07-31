search_api := "apps/search-api"
web_gui := "apps/web-gui"

[private]
default: build

# buf.yaml and buf.gen.yaml live at the repository root because generation
# writes into two different applications; running from anywhere else would
# need relative escapes in every out path.
# sqlc.yaml stays in the search API instead: its schema and query paths are
# module-relative, and only that module has any.

generate:
    buf generate
    cd {{ search_api }} && sqlc generate

install:
    cd {{ web_gui }} && pnpm install

build:
    cd {{ search_api }} && go build ./...
    cd {{ web_gui }} && pnpm build

test:
    cd {{ search_api }} && go test ./...
    cd {{ web_gui }} && pnpm test

lint:
    buf lint
    cd {{ search_api }} && go vet ./...
    cd {{ search_api }} && golangci-lint run
    cd {{ web_gui }} && pnpm typecheck

fmt:
    buf format --write
    cd {{ search_api }} && golangci-lint fmt

tidy:
    cd {{ search_api }} && go mod tidy

# Needs a committed baseline: fails until api/ exists on main.
proto-breaking:
    buf breaking --against '.git#branch=main'
