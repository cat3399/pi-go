.PHONY: setup check frontend-build build dev

BIN_DIR ?= bin

setup:
	npm --prefix ../ui ci
	npm --prefix frontend ci

check:
	npm --prefix ../ui run check
	npm --prefix frontend run check
	CGO_ENABLED=0 go test -tags server ./...

frontend-build:
	npm --prefix ../ui run check
	npm --prefix frontend run build

build: frontend-build
	mkdir -p $(BIN_DIR)
	go build -tags production -trimpath -o $(BIN_DIR)/pi-go-gui .

dev:
	npm --prefix frontend run build:dev
	go run . --devtools
