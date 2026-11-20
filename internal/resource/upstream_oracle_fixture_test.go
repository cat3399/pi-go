package resource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFrontmatterAndSourceFixtures(t *testing.T) {
	var oracle struct {
		Frontmatter struct {
			ScalarAlias struct {
				Frontmatter map[string]any `json:"frontmatter"`
				Body        string         `json:"body"`
			} `json:"scalarAlias"`
		} `json:"frontmatter"`
		PromptSources []struct {
			Description, Source, Scope, Origin, BaseDir string
		} `json:"promptSourcesInLoaderOrder"`
	}
	data, err := os.ReadFile("testdata/upstream_oracle.json")
	if err != nil {
		t.Fatalf("read resource fixture: %v", err)
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("decode resource fixture: %v", err)
	}
	var inputs struct {
		Frontmatter struct{ ScalarAlias, Merge, Typed string }
	}
	data, err = os.ReadFile("testdata/upstream_oracle_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &inputs); err != nil {
		t.Fatal(err)
	}
	front, body, err := parseFrontmatter(inputs.Frontmatter.ScalarAlias)
	if err != nil || !reflect.DeepEqual(front, oracle.Frontmatter.ScalarAlias.Frontmatter) || body != oracle.Frontmatter.ScalarAlias.Body {
		t.Fatalf("scalar alias = %#v, %q, %v; want %#v", front, body, err, oracle.Frontmatter.ScalarAlias)
	}
	if _, _, err := parseFrontmatter(inputs.Frontmatter.Merge); err == nil {
		t.Fatal("accepted a YAML merge key")
	}
	if _, _, err := frontmatter(inputs.Frontmatter.Typed); err == nil {
		t.Fatal("accepted invalid types for known frontmatter fields")
	}
	if len(oracle.PromptSources) != 3 || oracle.PromptSources[0].Scope != "user" || oracle.PromptSources[1].Scope != "project" || oracle.PromptSources[2].Scope != "temporary" || oracle.PromptSources[2].Origin != "top-level" || oracle.PromptSources[2].BaseDir != "<root>/additional" {
		t.Fatalf("frozen source provenance = %#v", oracle.PromptSources)
	}

	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	project := filepath.Join(root, "project")
	additional := filepath.Join(root, "additional")
	userPath := filepath.Join(agent, "prompts", "user.md")
	projectPath := filepath.Join(project, ".pi-go", "prompts", "project.md")
	additionalPath := filepath.Join(additional, "additional.md")
	write(t, userPath, "---\ndescription: user\n---\nuser")
	write(t, projectPath, "---\ndescription: project\n---\nproject")
	write(t, additionalPath, "---\ndescription: additional\n---\nadditional")
	service, err := New(Config{
		CWD: project, AgentDir: agent, NoPromptTemplates: true,
		PromptSources: []Source{
			{Path: userPath, Source: "local", Scope: ScopeUser, Origin: OriginTopLevel, BaseDir: filepath.Dir(userPath)},
			{Path: projectPath, Source: "local", Scope: ScopeProject, Origin: OriginTopLevel, BaseDir: filepath.Dir(projectPath)},
		},
		PromptPaths: []string{additionalPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	gotSources := make([]struct {
		Description, Source, Scope, Origin, BaseDir string
	}, 0, len(snapshot.Templates))
	for _, template := range snapshot.Templates {
		baseDir := filepath.ToSlash(strings.Replace(template.Source.BaseDir, root, "<root>", 1))
		gotSources = append(gotSources, struct {
			Description, Source, Scope, Origin, BaseDir string
		}{
			Description: template.Description,
			Source:      template.Source.Source,
			Scope:       string(template.Source.Scope),
			Origin:      string(template.Source.Origin),
			BaseDir:     baseDir,
		})
	}
	// The recorded project source uses .pi; check the actual .pi-go location in this scenario.
	projectSource := &oracle.PromptSources[1]
	projectSource.BaseDir = strings.Replace(projectSource.BaseDir, "/project/.pi/", "/project/.pi-go/", 1)
	if !reflect.DeepEqual(gotSources, oracle.PromptSources) {
		t.Fatalf("prompt discovery provenance = %#v, want %#v", gotSources, oracle.PromptSources)
	}
}
