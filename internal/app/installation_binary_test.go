package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise deployment from an unrelated empty directory with only the binary.
// The provider is a local deterministic server; the read and bash tools are real.
func TestBinaryImportsOnceAndFollowsInstalledDocumentationLinks(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("requires building a binary and the Unix production Bash/auth backends")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	deployment := t.TempDir()
	binary := filepath.Join(deployment, "pi-go")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X=github.com/cat3399/pi-go/internal/product.Version=9.8.7", "-o", binary, "./cmd/pi-go")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build deployed binary: %v\n%s", err, output)
	}
	home, cwd := filepath.Join(deployment, "home"), filepath.Join(deployment, "project")
	legacy := filepath.Join(home, ".pi", "agent")
	agentDir := filepath.Join(home, ".pi-go", "agent")
	for _, directory := range []string{legacy, filepath.Join(cwd, ".pi")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const instructions = "Use this installation's local documentation to explain its behavior."
	const customization = "Local deployment notes persist across runtime creation."
	if err := os.WriteFile(filepath.Join(legacy, "AGENTS.md"), []byte(instructions), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "PI_MODEL=stale", "PI_SESSION_ID=stale"}
	run := func(arguments ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, binary, arguments...)
		command.Dir, command.Env = cwd, environment
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("deployed command %v: %v\n%s", arguments, err, output)
		}
		return string(output)
	}
	run("--help")
	if output := run("--version"); output != "9.8.7\n" {
		t.Fatalf("version = %q", output)
	}
	if _, err := os.Stat(filepath.Dir(agentDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("help/version initialized user data")
	}

	var mu sync.Mutex
	var payloads []map[string]any
	linkPattern := regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer imported-fixture-key" {
			t.Error("request did not use the independently imported credential")
		}
		mu.Lock()
		payloads = append(payloads, payload)
		turn := len(payloads)
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		step := (turn - 1) % 3
		if step == 2 {
			item := map[string]any{"type": "message", "id": "answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "installation verified"}}}
			writeProductionSSE(t, writer,
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}})
			return
		}
		input, _ := payload["input"].([]any)
		system := ""
		for _, raw := range input {
			message, _ := raw.(map[string]any)
			if message["role"] == "developer" || message["role"] == "system" {
				system, _ = message["content"].(string)
			}
		}
		if !strings.Contains(system, "operating inside pi-go") || !strings.Contains(system, "tools, skills, and prompt templates (docs/tools.md)") {
			t.Errorf("missing documentation navigation in deployed system prompt: %s", system)
		}
		for _, unwanted := range []string{"pi-go installation:", "Configuration directories:", "- Version:", "- Build:", "- Source directory:", "read the relevant local documentation and source files"} {
			if strings.Contains(system, unwanted) {
				t.Errorf("deployed system prompt still includes %q", unwanted)
			}
		}
		if !strings.Contains(system, instructions) {
			t.Error("deployed system prompt did not load imported user instructions")
		}
		facts := make(map[string]string)
		for _, line := range strings.Split(system, "\n") {
			if key, value, ok := strings.Cut(line, ": "); ok {
				facts[key] = value
			}
		}
		docs := facts["- Additional docs"]
		if !strings.HasPrefix(docs, filepath.Join(home, ".pi-go", "knowledge")+string(filepath.Separator)) {
			t.Errorf("documentation path is not in the deployed installation: %q", docs)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		type toolCall struct {
			id, name string
			args     map[string]any
		}
		calls := []toolCall{{"read-tool-docs", "read", map[string]any{"path": filepath.Join(docs, "tools.md")}}}
		if step == 1 {
			// Discover further reading through the document returned by the real
			// read tool, without a source path or installation facts in the prompt.
			links := make(map[string]string)
			for _, raw := range input {
				item, _ := raw.(map[string]any)
				if item["type"] != "function_call_output" || item["call_id"] != "read-tool-docs" {
					continue
				}
				output, _ := item["output"].(string)
				for _, match := range linkPattern.FindAllStringSubmatch(output, -1) {
					links[filepath.Base(match[1])] = filepath.Join(docs, filepath.FromSlash(match[1]))
				}
			}
			if links["registry.go"] == "" || links["self-knowledge.md"] == "" {
				t.Error("installed tool documentation did not provide source and runtime documentation links")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			calls = []toolCall{
				{"read-runtime-docs", "read", map[string]any{"path": links["self-knowledge.md"]}},
				{"read-tool-source", "read", map[string]any{"path": links["registry.go"]}},
				{"read-session-state", "bash", map[string]any{"command": `printf 'id=%s\nfile=%s\nprovider=%s\nmodel=%s\nreasoning=%s\n' "$PI_SESSION_ID" "$PI_SESSION_FILE" "$PI_PROVIDER" "$PI_MODEL" "$PI_REASONING_LEVEL"`}},
			}
		}
		var items []any
		for index, call := range calls {
			arguments, _ := json.Marshal(call.args)
			item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc-%s", call.id), "call_id": call.id, "name": call.name, "arguments": string(arguments)}
			items = append(items, item)
			writeProductionSSE(t, writer,
				map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "function_call", "id": item["id"], "call_id": item["call_id"], "name": call.name, "arguments": ""}},
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": string(arguments)},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": item["id"], "arguments": string(arguments)},
				map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
		writeProductionSSE(t, writer, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": items}})
	}))
	defer server.Close()
	writeModelsJSON(t, legacy, server.URL+"/v1", nil, nil)
	auth := `{"openai":{"type":"api_key","key":"imported-fixture-key"}}`
	writeAuthJSON(t, legacy, auth)
	for attempt := 0; attempt < 2; attempt++ {
		if output := run("run", "--model", "openai/gpt-4.1", "-p", "Read your local documentation, source, and current session metadata."); !strings.Contains(output, "installation verified") {
			t.Fatalf("run output = %q", output)
		}
		if attempt == 0 {
			original, err := os.ReadFile(filepath.Join(legacy, "auth.json"))
			if err != nil || string(original) != auth {
				t.Fatal("import modified legacy credentials")
			}
			writeAuthJSON(t, legacy, `{"openai":{"type":"api_key","key":"changed-legacy-key"}}`)
			documents, err := filepath.Glob(filepath.Join(home, ".pi-go", "knowledge", "*", "docs", "self-knowledge.md"))
			if err != nil || len(documents) != 1 {
				t.Fatalf("installed documentation = %v, %v", documents, err)
			}
			file, err := os.OpenFile(documents[0], os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatalf("documentation is not directly editable: %v", err)
			}
			_, writeErr := file.WriteString("\n" + customization + "\n")
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 6 {
		t.Fatalf("provider turns = %d, want 6", len(payloads))
	}
	for _, index := range []int{2, 5} {
		outputs := make(map[string]string)
		for _, raw := range payloads[index]["input"].([]any) {
			item := raw.(map[string]any)
			if item["type"] == "function_call_output" {
				outputs[item["call_id"].(string)], _ = item["output"].(string)
			}
		}
		if !strings.Contains(outputs["read-runtime-docs"], "# 版本、运行状态与本地源码") || !strings.Contains(outputs["read-tool-source"], "func NewBuiltInRegistry") {
			t.Fatalf("binary could not read its bundled docs and source: %v", outputs)
		}
		if index == 5 && !strings.Contains(outputs["read-runtime-docs"], customization) {
			t.Fatal("the restarted binary did not read the user's customized documentation")
		}
		metadata := outputs["read-session-state"]
		if !strings.Contains(metadata, "file="+filepath.Join(agentDir, "sessions")) || !strings.Contains(metadata, "provider=openai\nmodel=gpt-4.1\nreasoning=off") || strings.Contains(metadata, "stale") {
			t.Fatalf("live Bash metadata = %q", metadata)
		}
	}
	for _, directory := range []string{agentDir, filepath.Join(cwd, ".pi-go")} {
		if _, err := os.Stat(filepath.Join(directory, ".migration.json")); err != nil {
			t.Fatalf("missing completed import for %s: %v", directory, err)
		}
	}
	if sessions, err := filepath.Glob(filepath.Join(agentDir, "sessions", "*", "*.jsonl")); err != nil || len(sessions) != 2 {
		t.Fatalf("independent durable sessions = %v, %v", sessions, err)
	}
}
