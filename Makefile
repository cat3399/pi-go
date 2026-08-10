.PHONY: web-setup web-dev web-api-dev web-check web-build web-run test-core test-surface test-all

WEB_ARGS ?=

web-setup:
	./scripts/webui.sh setup

web-dev:
	./scripts/webui.sh dev $(WEB_ARGS)

web-api-dev:
	./scripts/webui.sh api-dev $(WEB_ARGS)

web-check:
	./scripts/webui.sh check

web-build:
	./scripts/webui.sh build

web-run:
	./scripts/webui.sh run $(WEB_ARGS)

test-core:
	go test ./internal/... ./cmd/pi-go ./cmd/pi-go-rpc

test-surface:
	./scripts/webui.sh test

test-all:
	go test ./...
	$(MAKE) web-check
