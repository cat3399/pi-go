package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/tool"
)

type richRegistryResultTool struct{ content []llm.ToolResultContentBlock }

func (t richRegistryResultTool) Name() string { return "rich" }
func (t richRegistryResultTool) ExecuteJSON(context.Context, []byte) (tool.ToolResult, error) {
	return tool.ToolResult{Text: "fallback", Content: append([]llm.ToolResultContentBlock(nil), t.content...), Details: map[string]any{"rich": true}}, nil
}

func TestFilesystemExecutorDispatchesRegistryTools(t *testing.T) {
	suite, err := tool.NewFilesystemSuite(tool.FilesystemOptions{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewFilesystemRegistry(suite)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewFilesystemExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	if executor.Name() != "filesystem" || !executor.Supports(tool.WriteToolName) || executor.Supports("missing") {
		t.Fatalf("unexpected support matrix")
	}
	output, err := executor.ExecuteNamed(context.Background(), tool.WriteToolName, []byte(`{"path":"nested/a.txt","content":"hello"}`), nil)
	if err != nil || !strings.Contains(output.Text, "Successfully wrote") {
		t.Fatalf("dispatch = %#v, %v", output, err)
	}
}

func TestFilesystemExecutorRequiresRegistry(t *testing.T) {
	if _, err := NewFilesystemExecutor(nil); err == nil {
		t.Fatal("NewFilesystemExecutor(nil) succeeded")
	}
}

func TestRegistryAndFilesystemExecutorsPreserveRichToolContent(t *testing.T) {
	text, err := llm.NewTextBlock("image result")
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry(richRegistryResultTool{content: []llm.ToolResultContentBlock{text, image}})
	if err != nil {
		t.Fatal(err)
	}
	registryExecutor, err := NewRegistryExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	filesystemExecutor, err := NewFilesystemExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	for name, executor := range map[string]NamedToolExecutor{"registry": registryExecutor, "filesystem": filesystemExecutor} {
		output, executeErr := executor.ExecuteNamed(context.Background(), "rich", []byte(`{}`), nil)
		if executeErr != nil || output.Text != "fallback" || len(output.Content) != 2 || output.Details.(map[string]any)["rich"] != true {
			t.Fatalf("%s rich output = %#v, %v", name, output, executeErr)
		}
		if _, ok := output.Content[0].(llm.TextBlock); !ok {
			t.Fatalf("%s text block = %T", name, output.Content[0])
		}
		if _, ok := output.Content[1].(llm.ImageBlock); !ok {
			t.Fatalf("%s image block = %T", name, output.Content[1])
		}
	}
}

func TestRegistryAndFilesystemExecutorsForwardNamedArgumentPreparation(t *testing.T) {
	suite, err := tool.NewFilesystemSuite(tool.FilesystemOptions{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewFilesystemRegistry(suite)
	if err != nil {
		t.Fatal(err)
	}
	registryExecutor, err := NewRegistryExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	filesystemExecutor, err := NewFilesystemExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	for name, preparer := range map[string]NamedToolArgumentPreparer{"registry": registryExecutor, "filesystem": filesystemExecutor} {
		original := map[string]any{
			"path": "target.txt", "oldText": "one", "newText": "ONE",
			"edits": `[{"oldText":"two","newText":"TWO"}]`,
		}
		prepared, prepareErr := preparer.PrepareArguments(tool.EditToolName, original)
		if prepareErr != nil {
			t.Fatalf("%s PrepareArguments = %v", name, prepareErr)
		}
		object, ok := prepared.(map[string]any)
		edits, editsOK := object["edits"].([]any)
		if !ok || !editsOK || len(edits) != 2 {
			t.Fatalf("%s prepared = %#v", name, prepared)
		}
		if _, exists := object["oldText"]; exists {
			t.Fatalf("%s retained top-level oldText: %#v", name, object)
		}
		if original["oldText"] != "one" {
			t.Fatalf("%s mutated caller arguments: %#v", name, original)
		}
	}
}
