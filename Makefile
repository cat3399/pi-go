.PHONY: help setup check build dev run doctor devices

SURFACE ?= terminal
ARGS ?=

help:
	@./scripts/surface.sh help

setup check build dev run doctor devices:
	@./scripts/surface.sh $@ "$(SURFACE)" $(ARGS)
