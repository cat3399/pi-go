.PHONY: help setup check build dev run doctor devices e2e-core e2e-deepseek sync-models

SURFACE ?= terminal
ARGS ?=
VERSION ?= 0.1.0-dev
OUTPUT_DIR ?=

export PI_GO_VERSION = $(VERSION)
export OUTPUT_DIR

help:
	@./scripts/surface.sh help

sync-models:
	@./scripts/sync-models.sh $(ARGS)

setup check build dev run doctor devices:
	@./scripts/surface.sh $@ "$(SURFACE)" $(ARGS)

e2e-core:
	@go test -count=1 ./internal/app ./internal/application ./internal/rpc ./surface/web

e2e-deepseek:
	@if [ -z "$$DEEPSEEK_API_KEY" ]; then \
		echo "DEEPSEEK_API_KEY is required for the live DeepSeek E2E suite" >&2; \
		exit 2; \
	fi
	@go test -v -count=1 -p=1 -timeout=30m -run '^TestLive.*DeepSeek' ./internal/agent ./internal/app
