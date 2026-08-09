package resource_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/resource"
)

func TestServiceRebuildsPromptAndExpandsInputFromOneHealthySnapshot(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	skillDir := filepath.Join(cwd, ".pi", "skills", "review")
	promptDir := filepath.Join(cwd, ".pi", "prompts")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\nReview every change."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "ship.md"), []byte("Ship $@"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: agentDir,
		Tools: []resource.Tool{
			{Name: "read", Snippet: "Read files"},
			{Name: "bash", Snippet: "Run commands"},
			{Name: "edit", Snippet: "Edit files", PromptGuidelines: []string{"Edit carefully"}},
		},
		SelectedTools: []string{"read", "bash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt, options, err := service.BuildSystemPromptForTools([]string{"read", "edit"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "- read: Read files") || !strings.Contains(prompt, "- edit: Edit files") || strings.Contains(prompt, "- bash: Run commands") {
		t.Fatalf("rebuilt prompt tools = %q", prompt)
	}
	if !strings.Contains(prompt, "Edit carefully") || !strings.Contains(prompt, "<available_skills>") {
		t.Fatalf("rebuilt prompt resources = %q", prompt)
	}
	if len(options.SelectedTools) != 2 || options.SelectedTools[0] != "read" || options.SelectedTools[1] != "edit" || len(options.Skills) != 1 {
		t.Fatalf("build options = %#v", options)
	}
	prompt, options, err = service.BuildSystemPromptForTools([]string{})
	if err != nil || !strings.Contains(prompt, "Available tools:\n(none)") || strings.Contains(prompt, "<available_skills>") || options.SelectedTools == nil {
		t.Fatalf("empty active tool prompt = (%q, %#v, %v)", prompt, options, err)
	}
	expanded, err := service.ExpandInput("/skill:review now")
	if err != nil || !strings.Contains(expanded, `<skill name="review"`) || !strings.Contains(expanded, "Review every change.\n</skill>\n\nnow") {
		t.Fatalf("skill expansion = (%q, %v)", expanded, err)
	}
	expanded, err = service.ExpandInput("/ship release")
	if err != nil || expanded != "Ship release" {
		t.Fatalf("template expansion = (%q, %v)", expanded, err)
	}
}
