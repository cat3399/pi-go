.PHONY: setup check frontend-build build dev

BIN_DIR ?= bin
VERSION ?= 0.1.0-dev
GOEXE := $(shell go env GOEXE)

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
	mkdir -p "$(BIN_DIR)"
	go build -tags production -trimpath -ldflags "-X=main.version=$(VERSION)" -o "$(BIN_DIR)/pi-go-gui$(GOEXE)" .

dev:
	npm --prefix frontend run build:dev
	go run . --devtools
