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

func TestFrozenUpstreamFrontmatterAndSourceOracle(t *testing.T) {
	var oracle struct {
		UpstreamCommit string `json:"upstreamCommit"`
		Generator      struct {
			NodeVersion, Corpus string
		} `json:"generator"`
		Frontmatter struct {
			ScalarAlias struct {
				Frontmatter map[string]any `json:"frontmatter"`
				Body        string         `json:"body"`
			} `json:"scalarAlias"`
			Merge struct {
				Frontmatter map[string]any `json:"frontmatter"`
			} `json:"merge"`
			Typed struct {
				Frontmatter map[string]any `json:"frontmatter"`
			} `json:"typed"`
		} `json:"frontmatter"`
		PromptSources []struct {
			Description, Source, Scope, Origin, BaseDir string
		} `json:"promptSourcesInLoaderOrder"`
	}
	data, err := os.ReadFile("testdata/upstream_oracle.json")
	if err != nil {
		t.Fatalf("read frozen upstream oracle: %v", err)
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("decode frozen upstream oracle: %v", err)
	}
	if oracle.UpstreamCommit != "a116523434806910336b9de3e38a41aa5860030b" {
		t.Fatalf("unexpected upstream oracle commit %q", oracle.UpstreamCommit)
	}
	if oracle.Generator.NodeVersion != "v24.18.1" || oracle.Generator.Corpus != "upstream_oracle_corpus.json" {
		t.Fatalf("unpinned oracle generator metadata = %#v", oracle.Generator)
	}
	if _, err := os.Stat("testdata/" + oracle.Generator.Corpus); err != nil {
		t.Fatalf("oracle corpus is unavailable: %v", err)
	}
	front, body, err := parseFrontmatter("---\nsummary: &summary aliased text\ndescription: *summary\n---\nbody")
	if err != nil || front["description"] != oracle.Frontmatter.ScalarAlias.Frontmatter["description"] || body != oracle.Frontmatter.ScalarAlias.Body {
		t.Fatalf("scalar alias = %#v, %q, %v; frozen upstream = %#v", front, body, err, oracle.Frontmatter.ScalarAlias)
	}
	if _, expanded := oracle.Frontmatter.Merge.Frontmatter["<<"]; !expanded {
		t.Fatal("frozen upstream merge fixture was not generated from the YAML parser")
	}
	if _, _, err := parseFrontmatter("---\ndefaults: &defaults\n  description: merged\n<<: *defaults\n---\nbody"); err == nil {
		t.Fatal("Go hardening unexpectedly expanded the merge accepted by the frozen upstream oracle")
	}
	if _, ok := oracle.Frontmatter.Typed.Frontmatter["name"].(float64); !ok {
		t.Fatalf("frozen typed name is not numeric: %#v", oracle.Frontmatter.Typed.Frontmatter)
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
		// Namespace is an intentional product difference; compare discovery
		// order and provenance against the unchanged upstream fixture.
		baseDir := strings.Replace(template.Source.BaseDir, root, "<root>", 1)
		baseDir = strings.Replace(baseDir, "/project/.pi-go/", "/project/.pi/", 1)
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
	if !reflect.DeepEqual(gotSources, oracle.PromptSources) {
		t.Fatalf("prompt discovery provenance = %#v, frozen upstream = %#v", gotSources, oracle.PromptSources)
	}
}
