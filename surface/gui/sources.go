package main

import "embed"

// The GUI is an independent Go module; its source inputs accompany the core's
// source bundle without including generated frontend output or dependencies.
//
//go:embed *.go README.md THIRD_PARTY_NOTICES.md Makefile go.mod go.sum frontend/src frontend/*.json frontend/*.ts frontend/index.html
var guiSources embed.FS
