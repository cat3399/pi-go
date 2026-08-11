package application

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestProjectTrustControlsProjectSkillDiscoveryAndToggle(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	skillPath := filepath.Join(cwd, ".pi", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, skillDocument("review", "Review code safely", false), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newResourceTestService(t, cwd, agentDir, ServiceOptions{})

	status, err := service.ProjectTrust(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !status.RequiresTrust || status.Trusted {
		t.Fatalf("initial trust = %#v", status)
	}
	before, err := service.ListSkills(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if before.ProjectResourcesLoaded || skillNamed(before.Skills, "review") != nil {
		t.Fatalf("untrusted project skill was loaded: %#v", before)
	}

	status, err = service.TrustProject(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !status.RequiresTrust || !status.Trusted {
		t.Fatalf("trusted project = %#v", status)
	}
	after, err := service.ListSkills(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	skill := skillNamed(after.Skills, "review")
	if !after.ProjectResourcesLoaded || skill == nil {
		t.Fatalf("trusted project skill missing: %#v", after)
	}
	if err := service.SetSkillModelInvocation(context.Background(), cwd, skill.FilePath, true); err != nil {
		t.Fatal(err)
	}
	toggled, err := service.ListSkills(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if skill := skillNamed(toggled.Skills, "review"); skill == nil || !skill.DisableModelInvocation {
		t.Fatalf("skill toggle was not persisted: %#v", toggled.Skills)
	}
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("disable-model-invocation: true")) {
		t.Fatalf("updated SKILL.md = %s", data)
	}
}

func TestNativeProjectSkillSearchInstallCheckAndUpdate(t *testing.T) {
	const (
		firstHash  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		secondHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	type registryState struct {
		sync.RWMutex
		hash        string
		description string
	}
	state := &registryState{hash: firstHash, description: "Review code safely"}
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.RLock()
		hash, description := state.hash, state.description
		state.RUnlock()
		switch {
		case request.URL.Path == "/api/search":
			writeTestJSON(t, writer, map[string]any{"skills": []map[string]any{
				{"id": "acme/skills/review", "name": "review", "source": "acme/skills", "installs": 12500},
				{"id": "acme/skills/tiny", "name": "tiny", "source": "acme/skills", "installs": 3},
			}})
		case request.URL.Path == "/repos/acme/skills/zipball/HEAD":
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(skillArchive(t, description))
		case request.URL.Path == "/repos/acme/skills/git/trees/HEAD":
			writeTestJSON(t, writer, map[string]any{
				"sha":  hash,
				"tree": []map[string]any{{"path": "skills/review", "type": "tree", "sha": hash}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(registry.Close)

	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	service := newResourceTestService(t, cwd, agentDir, ServiceOptions{
		SkillHTTP: registry.Client(), SkillsAPIURL: registry.URL, GitHubAPIURL: registry.URL,
	})
	results, err := service.SearchSkills(context.Background(), "review", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Package != "acme/skills@review" || results[0].Installs != "12.5K installs" {
		t.Fatalf("search results = %#v", results)
	}

	installed, err := service.InstallSkill(context.Background(), SkillInstallRequest{
		Package: "acme/skills@review", Scope: SkillScopeProject, CWD: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Install == nil || installed.Install.VersionHash != firstHash || !installed.Install.CanCheckForUpdates {
		t.Fatalf("installed skill = %#v", installed)
	}
	if installed.Description != "Review code safely" {
		t.Fatalf("installed description = %q", installed.Description)
	}
	status, err := service.ProjectTrust(context.Background(), cwd)
	if err != nil || !status.RequiresTrust || !status.Trusted {
		t.Fatalf("project trust after first install = %#v, %v", status, err)
	}
	lockData, err := os.ReadFile(filepath.Join(cwd, "skills-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(lockData, []byte(`"version": 1`)) || !bytes.Contains(lockData, []byte(firstHash)) || !bytes.Contains(lockData, []byte(`"skillPath": "skills/review/SKILL.md"`)) {
		t.Fatalf("project lock = %s", lockData)
	}
	global, err := service.InstallSkill(context.Background(), SkillInstallRequest{
		Package: "acme/skills@review", Scope: SkillScopeGlobal, CWD: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if global.Install == nil || global.Install.Scope != SkillScopeGlobal || global.Install.VersionHash != firstHash {
		t.Fatalf("shadowed global install = %#v", global)
	}
	linkPath := filepath.Join(agentDir, "skills", "review")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("global skill link: %v", err)
	}
	if linkTarget != filepath.Join(home, ".agents", "skills", "review") {
		t.Fatalf("global skill link = %q", linkTarget)
	}
	globalLock, err := os.ReadFile(filepath.Join(home, ".agents", ".skill-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(globalLock, []byte(`"version": 3`)) || !bytes.Contains(globalLock, []byte(`"skillFolderHash": "`+firstHash+`"`)) {
		t.Fatalf("global lock = %s", globalLock)
	}

	updates, err := service.CheckSkillUpdates(context.Background(), SkillUpdateRequest{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].State != SkillUpToDate {
		t.Fatalf("initial updates = %#v", updates)
	}
	state.Lock()
	state.hash, state.description = secondHash, "Review code with the updated workflow"
	state.Unlock()
	updates, err = service.CheckSkillUpdates(context.Background(), SkillUpdateRequest{
		CWD: cwd, Package: "acme/skills@review", Scope: SkillScopeProject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].State != SkillUpdateAvailable || updates[0].LatestVersion != secondHash {
		t.Fatalf("available update = %#v", updates)
	}
	updated, err := service.UpdateSkill(context.Background(), SkillUpdateRequest{
		CWD: cwd, Package: "acme/skills@review", Scope: SkillScopeProject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "Review code with the updated workflow" || updated.Install == nil || updated.Install.VersionHash != secondHash {
		t.Fatalf("updated skill = %#v", updated)
	}
}

func newResourceTestService(t *testing.T, cwd, agentDir string, extra ServiceOptions) *Service {
	t.Helper()
	extra.Context = context.Background()
	extra.Production = app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}
	extra.DisableReaper = true
	service, err := NewService(extra)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Errorf("Service.Close() error = %v", err)
		}
	})
	return service
}

func skillNamed(skills []SkillInfo, name string) *SkillInfo {
	for index := range skills {
		if skills[index].Name == name {
			return &skills[index]
		}
	}
	return nil
}

func skillDocument(name, description string, disabled bool) []byte {
	dormant := ""
	if disabled {
		dormant = "disable-model-invocation: true\n"
	}
	return []byte(fmt.Sprintf("---\nname: %s\ndescription: %s\n%s---\n\n# %s\n", name, description, dormant, name))
}

func skillArchive(t *testing.T, description string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, data := range map[string][]byte{
		"acme-skills-hash/skills/review/SKILL.md":          skillDocument("review", description, false),
		"acme-skills-hash/skills/review/references/use.md": []byte("Use the review checklist.\n"),
	} {
		file, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode registry response: %v", err)
	}
}

func TestSkillPackageValidationRejectsAmbiguousOrUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "owner/repo", "owner/repo@../escape", "owner/repo@Upper", "not-a-repo@skill", "owner/repo@two--hyphens"} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			t.Parallel()
			if _, err := parseSkillPackage(value); err == nil {
				t.Fatalf("parseSkillPackage(%q) succeeded", value)
			}
		})
	}
}
