.PHONY: setup check doctor build run device-list

BIN_DIR ?= bin
VERSION ?= 0.1.0-dev
export PI_GO_VERSION = $(VERSION)

setup:
	task setup

check:
	task check

doctor:
	task doctor

build:
	task build BIN_DIR="$(BIN_DIR)"

run:
	task run BIN_DIR="$(BIN_DIR)"

device-list:
	task device:list
