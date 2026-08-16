.PHONY: web-setup web-dev web-api-dev web-check web-build web-run gui-setup gui-check gui-build gui-dev mobile-setup mobile-check mobile-doctor mobile-build mobile-run mobile-device-list test-core test-surface test-all

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

# GUI is an opt-in product build. None of these targets participate in the
# default pi-go build or test graph.
gui-setup:
	$(MAKE) -C surface/gui setup

gui-check:
	$(MAKE) -C surface/gui check

gui-build:
	$(MAKE) -C surface/gui build

gui-dev:
	$(MAKE) -C surface/gui dev

# Mobile is an independent, remote-only Android product build. These targets
# do not participate in the default pi-go build or test graph.
mobile-setup:
	$(MAKE) -C surface/mobile setup

mobile-check:
	$(MAKE) -C surface/mobile check

mobile-doctor:
	$(MAKE) -C surface/mobile doctor

mobile-build:
	$(MAKE) -C surface/mobile build

mobile-run:
	$(MAKE) -C surface/mobile run

mobile-device-list:
	$(MAKE) -C surface/mobile device-list

test-core:
	go test ./internal/... ./cmd/pi-go

test-surface:
	./scripts/webui.sh test

test-all:
	go test ./...
	$(MAKE) web-check
