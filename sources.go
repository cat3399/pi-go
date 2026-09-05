// Package pigo contains the source and documentation shipped with the Go core.
// Embedding the compilation inputs directly keeps ordinary go build, development
// builds and release builds on the same source snapshot without a generated copy.
package pigo

import "embed"

// Sources contains maintained source inputs, including fixtures and build
// instructions. Dependency installations, credentials and generated UI output
// are deliberately outside the selected trees. Separate Go modules contribute
// their own source filesystem when assembling an embedded core.
//
//go:embed *.go README.md docs internal cmd scripts .github/workflows Makefile go.mod go.sum
//go:embed surface/tui surface/web/*.go surface/ui/src surface/ui/*.json surface/ui/LICENSE surface/ui/THIRD_PARTY_NOTICES.md
//go:embed surface/web/_frontend/app surface/web/_frontend/components surface/web/_frontend/public surface/web/_frontend/*.json surface/web/_frontend/*.ts surface/web/_frontend/*.mjs
//go:embed surface/web/_frontend/README.md surface/web/_frontend/LICENSE
var Sources embed.FS
