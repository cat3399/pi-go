// Command cataloggen imports and verifies the scoped upstream pi-ai catalog.
//
// The checked-in JSON files are the oracle. A normal go generate validates
// them and rewrites catalog_oracle_generated.go deterministically. The
// generated oracle records the exact npm package version. Maintainers can
// unpack that package and import its dist/providers/data directory with:
//
//	go run ./internal/model/cataloggen -source /path/to/package/dist/providers/data -update-oracle
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type target struct {
	File, Provider    string
	APIs              []string
	AllowEmptyBaseURL bool
}

var targets = []target{
	{File: "anthropic.json", Provider: "anthropic", APIs: []string{"anthropic-messages"}},
	{File: "azure-openai-responses.json", Provider: "azure-openai-responses", APIs: []string{"azure-openai-responses"}, AllowEmptyBaseURL: true},
	{File: "cerebras.json", Provider: "cerebras", APIs: []string{"openai-completions"}},
	{File: "deepseek.json", Provider: "deepseek", APIs: []string{"openai-completions"}},
	{File: "groq.json", Provider: "groq", APIs: []string{"openai-completions"}},
	{File: "openai-codex.json", Provider: "openai-codex", APIs: []string{"openai-codex-responses"}},
	{File: "openai.json", Provider: "openai", APIs: []string{"openai-responses"}},
	{File: "together.json", Provider: "together", APIs: []string{"openai-completions"}},
	{File: "xai.json", Provider: "xai", APIs: []string{"openai-completions", "openai-responses"}},
}

const upstreamCatalogPackage = "@earendil-works/pi-ai@0.84.2"

type modelIdentity struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	API      string `json:"api"`
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	Cost     any    `json:"cost"`
	Input    any    `json:"input"`
	Context  uint64 `json:"contextWindow"`
	Max      uint64 `json:"maxTokens"`
}

type oracle struct {
	File, Provider, SHA256 string
	APIs                   []string
	Count                  int
	IDs                    []string
}

func main() {
	var source, oracleDir, output string
	var update bool
	modelDir := "internal/model"
	if _, err := os.Stat("catalog_generated.go"); err == nil {
		modelDir = "."
	}
	flag.StringVar(&source, "source", "", "optional upstream providers/data directory")
	flag.StringVar(&oracleDir, "oracle-dir", filepath.Join(modelDir, "catalogdata"), "checked-in oracle directory")
	flag.StringVar(&output, "output", filepath.Join(modelDir, "catalog_oracle_generated.go"), "generated Go output")
	flag.BoolVar(&update, "update-oracle", false, "replace the checked-in oracle from -source")
	flag.Parse()
	if update && source == "" {
		fatal("-update-oracle requires -source")
	}
	if update {
		if err := os.MkdirAll(oracleDir, 0o755); err != nil {
			fatal("create oracle directory: %v", err)
		}
	}

	oracles := make([]oracle, 0, len(targets))
	allIDs := make([]string, 0)
	for _, target := range targets {
		path := filepath.Join(oracleDir, target.File)
		if source != "" {
			path = filepath.Join(source, target.File)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			fatal("read %s: %v", path, err)
		}
		canonical, ids, err := validateAndCanonicalize(raw, target)
		if err != nil {
			fatal("validate %s: %v", path, err)
		}
		if update {
			if err := writeAtomic(filepath.Join(oracleDir, target.File), canonical, 0o644); err != nil {
				fatal("write oracle %s: %v", target.File, err)
			}
		}
		digest := sha256.Sum256(canonical)
		oracles = append(oracles, oracle{
			File: target.File, Provider: target.Provider, APIs: target.APIs,
			SHA256: hex.EncodeToString(digest[:]), Count: len(ids), IDs: ids,
		})
		for _, id := range ids {
			allIDs = append(allIDs, target.Provider+"/"+id)
		}
	}
	sort.Strings(allIDs)
	generated, err := render(oracles, allIDs)
	if err != nil {
		fatal("render generated catalog oracle: %v", err)
	}
	if err := writeAtomic(output, generated, 0o644); err != nil {
		fatal("write generated catalog oracle: %v", err)
	}
}

func validateAndCanonicalize(raw []byte, target target) ([]byte, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var groups map[string]map[string]json.RawMessage
	if err := decoder.Decode(&groups); err != nil || groups == nil {
		return nil, nil, fmt.Errorf("catalog root must be an object: %w", err)
	}
	if len(groups) != len(target.APIs) {
		return nil, nil, fmt.Errorf("catalog APIs = %v, want exactly %v", sortedKeys(groups), target.APIs)
	}
	ids := make([]string, 0)
	seenIDs := make(map[string]struct{})
	for _, api := range target.APIs {
		models := groups[api]
		if models == nil {
			return nil, nil, fmt.Errorf("catalog is missing API %q", api)
		}
		for id, rawModel := range models {
			var identity modelIdentity
			if err := json.Unmarshal(rawModel, &identity); err != nil {
				return nil, nil, fmt.Errorf("model %q is invalid: %w", id, err)
			}
			if strings.TrimSpace(id) == "" || identity.ID != id || identity.Provider != target.Provider || identity.API != api {
				return nil, nil, fmt.Errorf("model %q has inconsistent identity", id)
			}
			if identity.Name == "" || (!target.AllowEmptyBaseURL && identity.BaseURL == "") || identity.Cost == nil || identity.Input == nil || identity.Context == 0 || identity.Max == 0 {
				return nil, nil, fmt.Errorf("model %q is missing required catalog metadata", id)
			}
			if _, duplicate := seenIDs[id]; duplicate {
				return nil, nil, fmt.Errorf("model %q occurs in multiple API groups", id)
			}
			seenIDs[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	canonical, err := json.MarshalIndent(groups, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	canonical = append(canonical, '\n')
	return canonical, ids, nil
}

func sortedKeys(groups map[string]map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func render(oracles []oracle, allIDs []string) ([]byte, error) {
	var out strings.Builder
	out.WriteString("// Code generated by go generate ./internal/model; DO NOT EDIT.\n")
	out.WriteString("package model\n\n")
	fmt.Fprintf(&out, "const generatedCatalogSource = %q\n\n", upstreamCatalogPackage)
	out.WriteString("var generatedCatalogOracle = map[string]catalogOracle{\n")
	for _, value := range oracles {
		fmt.Fprintf(&out, "\t%q: {Provider: %q, APIs: %#v, SHA256: %q, Count: %d},\n", value.File, value.Provider, value.APIs, value.SHA256, value.Count)
	}
	out.WriteString("}\n\n")
	out.WriteString("var generatedCatalogModelIDs = []string{\n")
	for _, id := range allIDs {
		fmt.Fprintf(&out, "\t%q,\n", id)
	}
	out.WriteString("}\n")
	return format.Source([]byte(out.String()))
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".cataloggen-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() {
		if _, err := os.Stat(name); err == nil {
			_ = os.Rename(name, filepath.Join(os.TempDir(), filepath.Base(name)))
		}
	}()
	if err := temporary.Chmod(mode); err == nil {
		_, err = temporary.Write(data)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func fatal(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "cataloggen: "+format+"\n", arguments...)
	os.Exit(1)
}
