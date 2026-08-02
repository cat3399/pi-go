package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/tool"
)

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
