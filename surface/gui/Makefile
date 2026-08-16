.PHONY: setup check check-native frontend-build build dev

setup:
	npm --prefix ../ui ci
	npm --prefix frontend ci

check:
	npm --prefix ../ui run check
	npm --prefix frontend run check
	CGO_ENABLED=0 go test -tags server ./...

check-native:
	go test ./...

frontend-build:
	npm --prefix ../ui run check
	npm --prefix frontend run build

build: frontend-build
	mkdir -p bin
	go build -tags production -trimpath -o bin/pi-go-gui .

dev:
	npm --prefix frontend run build:dev
	go run . --devtools
