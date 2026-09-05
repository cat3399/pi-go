package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const deepSeekBinaryLiveModelID = "deepseek-v4-flash"

// TestLiveDeepSeekBinaryToolResumeEndToEnd is the outermost opt-in acceptance
// test. Unlike the lower-level live transport matrix, it builds and executes
// the real pi-go command twice, exercises the built-in DeepSeek catalog and
// credential resolver, runs a production filesystem tool, then resumes the
// physical JSONL from a fresh OS process.
func TestLiveDeepSeekBinaryToolResumeEndToEnd(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY is not set")
	}

	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(packageDir, "..", ".."))
	if _, err := os.Stat(filepath.Join(repositoryRoot, "go.mod")); err != nil {
		t.Fatalf("resolve repository root from %s: %v", packageDir, err)
	}
	binaryName := "pi-go-live-e2e"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(t.TempDir(), binaryName)
	buildContext, buildCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	build := exec.CommandContext(buildContext, "go", "build", "-trimpath", "-o", binaryPath, "./cmd/pi-go")
	build.Dir = repositoryRoot
	buildOutput, buildErr := build.CombinedOutput()
	buildCancel()
	if buildErr != nil {
		t.Fatalf("build production pi-go binary: %v\n%s", buildErr, buildOutput)
	}

	const (
		toolFile      = "deepseek-live-tool.txt"
		toolContent   = "PI_GO_DEEPSEEK_LIVE_TOOL_CONTENT"
		firstAnswer   = "PI_GO_DEEPSEEK_LIVE_FIRST_OK"
		resumedAnswer = "PI_GO_DEEPSEEK_LIVE_RESUMED_OK"
	)
	workingDir, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(workingDir, "deepseek-live-session.jsonl")
	environment := deepSeekBinaryLiveEnvironment(os.Environ(), map[string]string{
		"DEEPSEEK_API_KEY": apiKey,
		"PI_GO_AGENT_DIR":  agentDir,
	})

	firstPrompt := "This is an end-to-end acceptance test. Call the write tool exactly once with path \"" + toolFile +
		"\" and content \"" + toolContent + "\". Do not call any other tool. After the tool succeeds, reply with exactly: " + firstAnswer
	first := runDeepSeekBinaryLive(t, binaryPath, workingDir, environment, []string{
		"run", "--model", "deepseek/" + deepSeekBinaryLiveModelID + ":off",
		"--session", sessionPath, "-p", firstPrompt,
	})
	if first.exitCode != ExitSuccess || !strings.Contains(first.stdout, firstAnswer) || first.stderr != "" {
		t.Fatalf("first production process = exit %d stdout %q stderr %q", first.exitCode, first.stdout, first.stderr)
	}
	written, err := os.ReadFile(filepath.Join(workingDir, toolFile))
	if err != nil || string(written) != toolContent {
		t.Fatalf("live DeepSeek tool side effect = %q, %v", written, err)
	}

	secondPrompt := "Do not call any tool. If the preceding persisted conversation contains one successful write of \"" +
		toolContent + "\" followed by \"" + firstAnswer + "\", reply with exactly: " + resumedAnswer + ". Otherwise reply FAILURE."
	second := runDeepSeekBinaryLive(t, binaryPath, workingDir, environment, []string{
		"run", "--model", "deepseek/" + deepSeekBinaryLiveModelID + ":off",
		"--session", sessionPath, "-p", secondPrompt,
	})
	if second.exitCode != ExitSuccess || !strings.Contains(second.stdout, resumedAnswer) || second.stderr != "" {
		t.Fatalf("resumed production process = exit %d stdout %q stderr %q", second.exitCode, second.stdout, second.stderr)
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(apiKey)) {
		t.Fatal("DeepSeek credential was persisted in the live session")
	}
	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.BuildContext().Messages()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 6 {
		t.Fatalf("live reopened message count = %d, want user/tool-call/tool-result/final/user/final", len(messages))
	}
	wantRoles := []llm.Role{
		llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant,
	}
	for index, want := range wantRoles {
		if messages[index].Role() != want {
			t.Fatalf("live reopened message %d role = %s, want %s", index, messages[index].Role(), want)
		}
	}
	toolUse, ok := messages[1].(llm.AssistantToolUseMessage)
	if !ok || len(toolUse.Blocks()) != 1 || toolUse.FinishReason() != llm.FinishToolUse {
		t.Fatalf("live reopened tool-use = %T %#v", messages[1], messages[1])
	}
	call, ok := toolUse.Blocks()[0].(llm.ToolCallBlock)
	if !ok || call.Name() != "write" {
		t.Fatalf("live reopened tool call = %#v", toolUse.Blocks())
	}
	var arguments struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.ArgumentsJSON(), &arguments); err != nil || arguments.Content != toolContent ||
		filepath.Base(filepath.Clean(arguments.Path)) != toolFile {
		t.Fatalf("live reopened tool arguments = %#v, %v", arguments, err)
	}
	toolResult, ok := messages[2].(llm.ToolResultMessage)
	if !ok || toolResult.IsError() || toolResult.ToolName() != "write" || toolResult.ToolCallID() != call.ID() {
		t.Fatalf("live reopened tool result = %T %#v", messages[2], messages[2])
	}
	if provenance := toolUse.AssistantProvenance(); !provenance.Matches(
		"deepseek", provider.OpenAICompletionsAPI, deepSeekBinaryLiveModelID,
	) {
		t.Fatalf("live DeepSeek tool provenance = %#v", provenance)
	}
	firstTerminal, ok := messages[3].(llm.AssistantTerminal)
	if !ok || firstTerminal.FinishReason() != llm.FinishStop || !strings.Contains(deepSeekBinaryLiveAssistantText(firstTerminal), firstAnswer) {
		t.Fatalf("live reopened first terminal = %T %#v", messages[3], messages[3])
	}
	resumedTerminal, ok := messages[5].(llm.AssistantTerminal)
	if !ok || resumedTerminal.FinishReason() != llm.FinishStop || !strings.Contains(deepSeekBinaryLiveAssistantText(resumedTerminal), resumedAnswer) {
		t.Fatalf("live reopened resumed terminal = %T %#v", messages[5], messages[5])
	}
}

type deepSeekBinaryLiveResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runDeepSeekBinaryLive(t *testing.T, binaryPath, workingDir string, environment, arguments []string) deepSeekBinaryLiveResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, arguments...)
	command.Dir = workingDir
	command.Env = append([]string(nil), environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("production DeepSeek process timed out: %v", ctx.Err())
	}
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("start production DeepSeek process: %v", err)
		}
		exitCode = exitError.ExitCode()
	}
	return deepSeekBinaryLiveResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func deepSeekBinaryLiveEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		overridden := false
		for key := range overrides {
			if name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key)) {
				overridden = true
				break
			}
		}
		if !overridden {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func deepSeekBinaryLiveAssistantText(message llm.AssistantTerminal) string {
	var blocks []llm.AssistantBlock
	switch value := message.(type) {
	case llm.AssistantTextMessage:
		blocks = value.Blocks()
	case llm.AssistantRichMessage:
		blocks = value.Blocks()
	case llm.AssistantToolUseMessage:
		blocks = value.Blocks()
	case llm.AssistantFailureMessage:
		blocks = value.Blocks()
	}
	var text strings.Builder
	for _, block := range blocks {
		if value, ok := block.(llm.TextBlock); ok {
			text.WriteString(value.Text())
		}
	}
	return text.String()
}
