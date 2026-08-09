package resource

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalResourceRootSetScalesLinearlyAcrossThousandRoots(t *testing.T) {
	calls := 0
	set := &canonicalResourceRootSet{
		seen: map[string]struct{}{},
		canonicalize: func(path string) string {
			calls++
			return filepath.Clean(path)
		},
	}
	for index := 0; index < 1000; index++ {
		path := filepath.Join("root", fmt.Sprintf("%04d", index))
		if !set.add(path) {
			t.Fatalf("unique root %d was deduplicated", index)
		}
		if set.add(path) {
			t.Fatalf("duplicate root %d was retained", index)
		}
	}
	if calls != 2000 || len(set.seen) != 1000 {
		t.Fatalf("canonical calls=%d roots=%d; want one call per candidate", calls, len(set.seen))
	}
}

func TestResourcePathParametersRejectNUL(t *testing.T) {
	base := Config{CWD: t.TempDir(), AgentDir: t.TempDir()}
	for _, mutate := range []func(*Config){
		func(config *Config) { config.CWD += "\x00bad" },
		func(config *Config) { config.AgentDir += "\x00bad" },
		func(config *Config) { config.HomeDir = "bad\x00home" },
		func(config *Config) { config.SkillPaths = []string{"bad\x00skill"} },
		func(config *Config) { config.PromptPaths = []string{"bad\x00prompt"} },
		func(config *Config) { config.SkillSources = []Source{{Path: "bad\x00source"}} },
	} {
		config := base
		mutate(&config)
		if _, err := validateConfig(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NUL path config = %#v, error = %v", config, err)
		}
	}
	if _, err := NewTrustStore("bad\x00agent"); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("NUL trust directory error = %v", err)
	}
}

func TestFrontmatterRejectsMergeAndStrictlyTypesKnownFields(t *testing.T) {
	front, body, err := parseFrontmatter("---\nsummary: &summary aliased text\ndescription: *summary\n---\nbody")
	if err != nil || front["description"] != "aliased text" || body != "body" {
		t.Fatalf("scalar alias = %#v, %q, %v", front, body, err)
	}
	if _, _, err := parseFrontmatter("---\ndefaults: &defaults\n  description: merged\n<<: *defaults\n---\nbody"); err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("YAML merge error = %v", err)
	}

	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	home := filepath.Join(root, "home")
	prompts := filepath.Join(root, "prompts")
	skills := filepath.Join(root, "skills")
	for name, metadata := range map[string]string{
		"bad-name":        "name: 7\ndescription: valid",
		"bad-description": "name: bad-description\ndescription: false",
		"bad-disable":     "name: bad-disable\ndescription: valid\ndisable-model-invocation: 'true'",
	} {
		write(t, filepath.Join(skills, name, "SKILL.md"), "---\n"+metadata+"\n---\nbody")
	}
	write(t, filepath.Join(prompts, "description.md"), "---\ndescription: [not, text]\n---\nbody")
	write(t, filepath.Join(prompts, "hint.md"), "---\ndescription: valid\nargument-hint: 3\n---\nbody")
	write(t, filepath.Join(prompts, "name.md"), "---\nname: 7\ndescription: valid\n---\nbody")
	service, err := New(Config{
		CWD: cwd, AgentDir: agent, HomeDir: home,
		NoSkills: true, NoPromptTemplates: true,
		SkillPaths: []string{skills}, PromptPaths: []string{prompts},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.Snapshot()
	if len(snapshot.Skills) != 0 || len(snapshot.Templates) != 0 || len(snapshot.Diagnostics) != 6 {
		t.Fatalf("strict metadata = skills %#v templates %#v diagnostics %#v", snapshot.Skills, snapshot.Templates, snapshot.Diagnostics)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.Source.Path != diagnostic.Path || diagnostic.Source.Scope != ScopeTemporary || diagnostic.Source.Origin != OriginTopLevel {
			t.Fatalf("diagnostic lost provenance: %#v", diagnostic)
		}
	}
}

func TestAdditionalPathsLoseToDefaultsAndPreserveProvenance(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	home := filepath.Join(root, "home")
	additional := filepath.Join(root, "additional")
	packageRoot := filepath.Join(root, "package")
	write(t, filepath.Join(agent, "prompts", "review.md"), "---\ndescription: user winner\n---\nuser")
	write(t, filepath.Join(agent, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: user winner\n---")
	write(t, filepath.Join(additional, "review.md"), "---\ndescription: additional loser\n---\nadditional")
	write(t, filepath.Join(additional, "SKILL.md"), "---\nname: review\ndescription: additional loser\n---")
	packagePrompt := filepath.Join(packageRoot, "package.md")
	packageSkill := filepath.Join(packageRoot, "skill", "SKILL.md")
	write(t, packagePrompt, "---\ndescription: package prompt\n---\npackage")
	write(t, packageSkill, "---\nname: package-skill\ndescription: package skill\n---")
	service, err := New(Config{
		CWD: cwd, AgentDir: agent, HomeDir: home,
		PromptSources: []Source{{Path: packagePrompt, Source: "example-package", Scope: ScopeUser, Origin: OriginPackage, BaseDir: packageRoot}},
		SkillSources:  []Source{{Path: packageSkill, Source: "example-package", Scope: ScopeUser, Origin: OriginPackage, BaseDir: packageRoot}},
		PromptPaths:   []string{filepath.Join(additional, "review.md")},
		SkillPaths:    []string{filepath.Join(additional, "SKILL.md")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.Snapshot()
	if len(snapshot.Templates) != 2 || snapshot.Templates[0].Description != "user winner" || snapshot.Templates[1].Source.Source != "example-package" || snapshot.Templates[1].Source.Origin != OriginPackage || snapshot.Templates[1].Source.BaseDir != packageRoot {
		t.Fatalf("template precedence/provenance = %#v", snapshot.Templates)
	}
	if len(snapshot.Skills) != 2 || snapshot.Skills[0].Description != "user winner" || snapshot.Skills[1].Source.Source != "example-package" || snapshot.Skills[1].Source.Origin != OriginPackage || snapshot.Skills[1].Source.BaseDir != packageRoot {
		t.Fatalf("skill precedence/provenance = %#v", snapshot.Skills)
	}
	if len(snapshot.Diagnostics) != 2 {
		t.Fatalf("collision diagnostics = %#v", snapshot.Diagnostics)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.WinnerSource.Scope != ScopeUser || diagnostic.LoserSource.Scope != ScopeTemporary || diagnostic.LoserSource.Origin != OriginTopLevel || diagnostic.LoserSource.BaseDir != additional {
			t.Fatalf("collision provenance = %#v", diagnostic)
		}
	}
}
