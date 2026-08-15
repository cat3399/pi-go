package parity_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type ledger struct {
	SchemaVersion int `yaml:"schemaVersion"`
	Upstream      struct {
		Commit          string   `yaml:"commit"`
		ProductionChain []string `yaml:"productionChain"`
	} `yaml:"upstream"`
	Scope struct {
		Included []string `yaml:"included"`
		Deferred []struct {
			Name         string `yaml:"name"`
			Reason       string `yaml:"reason"`
			UpstreamPath string `yaml:"upstreamPath"`
		} `yaml:"deferred"`
	} `yaml:"scope"`
	StatusDefinitions map[string]string  `yaml:"statusDefinitions"`
	Capabilities      []ledgerCapability `yaml:"capabilities"`
}

type ledgerCapability struct {
	ID       string `yaml:"id"`
	Category string `yaml:"category"`
	Status   string `yaml:"status"`
	Upstream struct {
		Path   string `yaml:"path"`
		Symbol string `yaml:"symbol"`
	} `yaml:"upstream"`
	Go *struct {
		Path   string `yaml:"path"`
		Symbol string `yaml:"symbol"`
	} `yaml:"go"`
	TargetGoBoundary string `yaml:"targetGoBoundary"`
	SurfaceExposure  struct {
		Application string `yaml:"application"`
		JSONL       string `yaml:"jsonl"`
		HTTP        string `yaml:"http"`
	} `yaml:"surfaceExposure"`
	Evidence []struct {
		Kind   string `yaml:"kind"`
		Path   string `yaml:"path"`
		Symbol string `yaml:"symbol"`
	} `yaml:"evidence"`
	IntentionalGoDifference string `yaml:"intentionalGoDifference"`
	RemainingDifference     string `yaml:"remainingDifference"`
}

type workflowCorpus struct {
	UpstreamCommit string `json:"upstreamCommit"`
}

func TestCoreParityLedgerIsConcreteAndCurrent(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "parity", "core-a116523.yaml"))
	if err != nil {
		t.Fatalf("read parity ledger: %v", err)
	}
	var document ledger
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode parity ledger: %v", err)
	}
	if document.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", document.SchemaVersion)
	}
	if len(document.Upstream.ProductionChain) != 5 || len(document.Scope.Included) == 0 || len(document.Scope.Deferred) == 0 {
		t.Fatalf("ledger scope is incomplete: chain=%v included=%d deferred=%d", document.Upstream.ProductionChain, len(document.Scope.Included), len(document.Scope.Deferred))
	}
	assertCorpusCommit(t, root, document.Upstream.Commit)

	allowedStatus := map[string]bool{"oracle_verified": true, "source_aligned": true, "remaining": true}
	allowedExposure := map[string]bool{"full": true, "partial": true, "deferred": true, "not_applicable": true}
	allowedEvidenceKind := map[string]bool{"go_source": true, "go_test": true, "upstream_oracle": true, "upstream_source": true}
	seen := make(map[string]struct{}, len(document.Capabilities))
	if len(document.Capabilities) < 30 {
		t.Fatalf("capabilities = %d, want a production-chain ledger rather than a sample", len(document.Capabilities))
	}
	for index, capability := range document.Capabilities {
		label := capability.ID
		if strings.TrimSpace(label) == "" {
			t.Fatalf("capabilities[%d] has an empty id", index)
		}
		if _, duplicate := seen[label]; duplicate {
			t.Fatalf("duplicate capability id %q", label)
		}
		seen[label] = struct{}{}
		if !allowedStatus[capability.Status] || strings.TrimSpace(capability.Category) == "" {
			t.Fatalf("%s has invalid category/status %q/%q", label, capability.Category, capability.Status)
		}
		if definition := document.StatusDefinitions[capability.Status]; strings.TrimSpace(definition) == "" {
			t.Fatalf("%s uses undocumented status %q", label, capability.Status)
		}
		if strings.TrimSpace(capability.Upstream.Path) == "" || strings.TrimSpace(capability.Upstream.Symbol) == "" {
			t.Fatalf("%s has no concrete upstream source", label)
		}
		for surface, value := range map[string]string{
			"application": capability.SurfaceExposure.Application,
			"jsonl":       capability.SurfaceExposure.JSONL,
			"http":        capability.SurfaceExposure.HTTP,
		} {
			if !allowedExposure[value] {
				t.Fatalf("%s has invalid %s exposure %q", label, surface, value)
			}
		}
		if strings.TrimSpace(capability.IntentionalGoDifference) == "" || strings.TrimSpace(capability.RemainingDifference) == "" {
			t.Fatalf("%s must state both intentional and remaining differences", label)
		}
		if capability.Status == "remaining" {
			if capability.Go != nil || strings.TrimSpace(capability.TargetGoBoundary) == "" || capability.RemainingDifference == "none" {
				t.Fatalf("%s must name a missing target boundary and a real remaining difference", label)
			}
		} else {
			if capability.Go == nil {
				t.Fatalf("%s must name its Go implementation", label)
			}
			assertFileContains(t, root, capability.Go.Path, capability.Go.Symbol, label+" Go implementation")
		}
		if len(capability.Evidence) == 0 {
			t.Fatalf("%s has no evidence", label)
		}
		hasUpstreamOracle := false
		for _, evidence := range capability.Evidence {
			if !allowedEvidenceKind[evidence.Kind] {
				t.Fatalf("%s has invalid evidence kind %q", label, evidence.Kind)
			}
			if strings.HasPrefix(evidence.Path, "packages/") {
				continue
			}
			if evidence.Kind == "upstream_oracle" {
				hasUpstreamOracle = true
			}
			needle := evidence.Symbol
			if evidence.Kind == "go_test" || evidence.Kind == "upstream_oracle" {
				needle = "func " + evidence.Symbol + "("
			}
			assertFileContains(t, root, evidence.Path, needle, label+" evidence")
		}
		if capability.Status == "oracle_verified" && !hasUpstreamOracle {
			t.Fatalf("%s is oracle_verified but names no executable upstream oracle", label)
		}
	}
	for _, deferred := range document.Scope.Deferred {
		if strings.TrimSpace(deferred.Name) == "" || strings.TrimSpace(deferred.Reason) == "" || strings.TrimSpace(deferred.UpstreamPath) == "" {
			t.Fatalf("deferred scope entry is not concrete: %#v", deferred)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve parity test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func assertCorpusCommit(t *testing.T, root, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "internal", "agent", "testdata", "upstream_workflow_corpus.json"))
	if err != nil {
		t.Fatalf("read workflow corpus: %v", err)
	}
	var corpus workflowCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("decode workflow corpus: %v", err)
	}
	if corpus.UpstreamCommit != want || len(want) != 40 {
		t.Fatalf("ledger upstream commit %q does not match workflow corpus %q", want, corpus.UpstreamCommit)
	}
}

func assertFileContains(t *testing.T, root, path, symbol, description string) {
	t.Helper()
	if strings.TrimSpace(path) == "" || strings.TrimSpace(symbol) == "" {
		t.Errorf("%s has an empty path or symbol", description)
		return
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Errorf("read %s %s: %v", description, path, err)
		return
	}
	if !strings.Contains(string(data), symbol) {
		t.Errorf("%s %s does not contain %q", description, path, symbol)
	}
}
