package application

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/resource"
	yaml "go.yaml.in/yaml/v3"
)

const (
	defaultSkillSearchLimit = 50
	maxSkillSearchLimit     = 50
	maxSkillResponseBytes   = 2 << 20
	maxSkillArchiveBytes    = 64 << 20
	maxInstalledSkillBytes  = 128 << 20
)

var (
	ErrSkillAlreadyInstalled  = errors.New("skill is already installed")
	ErrInstalledSkillMissing  = errors.New("installed skill not found")
	ErrSkillUpdateUnsupported = errors.New("skill cannot be updated automatically")
	ErrInvalidSkillRequest    = errors.New("invalid skill request")
)

type SkillSearchResult struct {
	Package  string
	Installs string
	URL      string
}

type SkillInstallRequest struct {
	Package string
	Scope   SkillInstallScope
	CWD     string
}

type SkillUpdateRequest struct {
	CWD     string
	Package string
	Scope   SkillInstallScope
}

type SkillUpdateState string

const (
	SkillUpToDate          SkillUpdateState = "up-to-date"
	SkillUpdateAvailable   SkillUpdateState = "update-available"
	SkillUpdateUnsupported SkillUpdateState = "unsupported"
	SkillUpdateError       SkillUpdateState = "error"
)

type SkillUpdateResult struct {
	Package        string
	Scope          SkillInstallScope
	State          SkillUpdateState
	CurrentVersion string
	LatestVersion  string
	Message        string
}

type remoteSkillFile struct {
	Path string
	Data []byte
	Mode os.FileMode
}

type remoteSkill struct {
	Source    string
	Name      string
	Ref       string
	SkillPath string
	TreeHash  string
	Files     []remoteSkillFile
}

type skillPackage struct {
	Source string
	Name   string
	Ref    string
}

var (
	githubSourcePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	skillNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
)

func validGitHubSource(value string) bool { return githubSourcePattern.MatchString(value) }

func (s *Service) SearchSkills(ctx context.Context, query string, limit int) ([]SkillSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("skill search query is required")
	}
	if limit <= 0 {
		limit = defaultSkillSearchLimit
	}
	if limit > maxSkillSearchLimit {
		limit = maxSkillSearchLimit
	}
	endpoint := s.skillsAPI + "/api/search?q=" + url.QueryEscape(query) + "&limit=" + strconv.Itoa(limit)
	var response struct {
		Skills []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Source   string `json:"source"`
			Installs int64  `json:"installs"`
		} `json:"skills"`
	}
	if err := s.getJSON(ctx, endpoint, &response, false); err != nil {
		return nil, fmt.Errorf("search skills.sh: %w", err)
	}
	result := make([]SkillSearchResult, 0, len(response.Skills))
	for _, value := range response.Skills {
		name, source, slug := strings.TrimSpace(value.Name), strings.TrimSpace(value.Source), strings.TrimSpace(value.ID)
		if name == "" || source == "" && slug == "" {
			continue
		}
		if source == "" {
			source = slug
		}
		result = append(result, SkillSearchResult{
			Package: source + "@" + name, Installs: formatSkillInstalls(value.Installs),
			URL: func() string {
				if slug == "" {
					return ""
				}
				return s.skillsAPI + "/" + strings.TrimPrefix(slug, "/")
			}(),
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return parseFormattedInstalls(result[i].Installs) > parseFormattedInstalls(result[j].Installs)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *Service) InstallSkill(ctx context.Context, request SkillInstallRequest) (SkillInfo, error) {
	ctx = normalizeContext(ctx)
	parsed, err := parseSkillPackage(request.Package)
	if err != nil {
		return SkillInfo{}, err
	}
	cwd := request.CWD
	if request.Scope == "" {
		request.Scope = SkillScopeGlobal
	}
	if request.Scope != SkillScopeGlobal && request.Scope != SkillScopeProject {
		return SkillInfo{}, fmt.Errorf("%w: scope must be global or project", ErrInvalidSkillRequest)
	}
	if request.Scope == SkillScopeProject {
		cwd, err = ValidateCWD(cwd)
		if err != nil {
			return SkillInfo{}, err
		}
		status, statusErr := s.ProjectTrust(ctx, cwd)
		if statusErr != nil {
			return SkillInfo{}, statusErr
		}
		if !status.Trusted {
			return SkillInfo{}, os.ErrPermission
		}
	} else if strings.TrimSpace(cwd) == "" {
		cwd = s.paths.WorkingDir
	} else {
		cwd, err = ValidateCWD(cwd)
		if err != nil {
			return SkillInfo{}, err
		}
	}
	remote, err := s.downloadSkill(ctx, parsed, "")
	if err != nil {
		return SkillInfo{}, err
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if request.Scope == SkillScopeProject {
		status, statusErr := s.ProjectTrust(ctx, cwd)
		if statusErr != nil {
			return SkillInfo{}, statusErr
		}
		if !status.Trusted {
			return SkillInfo{}, os.ErrPermission
		}
		// Installing the first project skill is itself an explicit trust-bearing
		// action. Persist the decision only after the remote skill is valid, and
		// immediately before publishing the first trust-requiring resource.
		if !status.RequiresTrust {
			store, storeErr := resource.NewTrustStore(s.paths.AgentDir)
			if storeErr != nil {
				return SkillInfo{}, storeErr
			}
			if storeErr := store.Set(ctx, cwd, true); storeErr != nil {
				return SkillInfo{}, storeErr
			}
		}
	}
	if err := s.publishSkill(ctx, cwd, request.Scope, remote, false); err != nil {
		return SkillInfo{}, err
	}
	return s.findInstalledSkill(ctx, cwd, request.Scope, parsed.Source+"@"+parsed.Name)
}

func (s *Service) CheckSkillUpdates(ctx context.Context, request SkillUpdateRequest) ([]SkillUpdateResult, error) {
	ctx = normalizeContext(ctx)
	cwd, err := ValidateCWD(request.CWD)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.ListSkills(ctx, cwd)
	if err != nil {
		return nil, err
	}
	installs := make([]SkillInstallInfo, 0)
	for _, skill := range snapshot.Skills {
		if skill.Install == nil {
			continue
		}
		if request.Package != "" || request.Scope != "" {
			if request.Package == "" || request.Scope == "" {
				return nil, errors.New("package and scope must be provided together")
			}
			if skill.Install.Package != request.Package || skill.Install.Scope != request.Scope {
				continue
			}
		}
		installs = append(installs, *skill.Install)
	}
	if request.Package != "" && len(installs) == 0 {
		return nil, ErrInstalledSkillMissing
	}
	result := make([]SkillUpdateResult, len(installs))
	for index, install := range installs {
		result[index] = s.checkSkillUpdate(ctx, install)
	}
	return result, nil
}

func (s *Service) UpdateSkill(ctx context.Context, request SkillUpdateRequest) (SkillInfo, error) {
	ctx = normalizeContext(ctx)
	cwd, err := ValidateCWD(request.CWD)
	if err != nil {
		return SkillInfo{}, err
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	snapshot, err := s.ListSkills(ctx, cwd)
	if err != nil {
		return SkillInfo{}, err
	}
	var selected *SkillInfo
	for index := range snapshot.Skills {
		install := snapshot.Skills[index].Install
		if install != nil && install.Package == request.Package && install.Scope == request.Scope {
			selected = &snapshot.Skills[index]
			break
		}
	}
	if selected == nil || selected.Install == nil {
		return SkillInfo{}, ErrInstalledSkillMissing
	}
	if !selected.Install.CanCheckForUpdates {
		return SkillInfo{}, ErrSkillUpdateUnsupported
	}
	parsed := skillPackage{Source: selected.Install.Source, Name: selected.Name, Ref: selected.Install.Ref}
	remote, err := s.downloadSkill(ctx, parsed, selected.Install.SkillPath)
	if err != nil {
		return SkillInfo{}, err
	}
	if err := s.publishSkill(ctx, cwd, request.Scope, remote, true); err != nil {
		return SkillInfo{}, err
	}
	return s.findInstalledSkill(ctx, cwd, request.Scope, request.Package)
}

func (s *Service) findInstalledSkill(ctx context.Context, cwd string, scope SkillInstallScope, pkg string) (SkillInfo, error) {
	snapshot, err := s.ListSkills(ctx, cwd)
	if err != nil {
		return SkillInfo{}, err
	}
	for _, skill := range snapshot.Skills {
		if skill.Install != nil && skill.Install.Scope == scope && skill.Install.Package == pkg {
			return skill, nil
		}
	}
	parsed, parseErr := parseSkillPackage(pkg)
	if parseErr != nil {
		return SkillInfo{}, ErrInstalledSkillMissing
	}
	root := filepath.Join(cwd, ".pi", "skills")
	if scope == SkillScopeGlobal {
		root = s.globalSkillsDirectory()
	}
	filePath := filepath.Join(root, parsed.Name, "SKILL.md")
	loader, loadErr := resource.New(resource.Config{
		CWD: cwd, AgentDir: s.paths.AgentDir, NoSkills: true, SkillPaths: []string{filePath},
	})
	if loadErr != nil {
		return SkillInfo{}, loadErr
	}
	if loadErr := loader.Reload(ctx); loadErr != nil {
		return SkillInfo{}, loadErr
	}
	direct, loadErr := loader.Snapshot()
	if loadErr != nil {
		return SkillInfo{}, loadErr
	}
	globalLock := readSkillLock(s.globalSkillLockPath())
	projectLock := readSkillLock(filepath.Join(cwd, "skills-lock.json"))
	for _, value := range direct.Skills {
		if value.Name != parsed.Name {
			continue
		}
		info := SkillInfo{
			Name: value.Name, Description: value.Description, FilePath: value.Path, BaseDir: value.BaseDir,
			DisableModelInvocation: value.DisableModelInvocation,
			SourceInfo:             SkillSourceInfo{Source: value.Source.Source, Scope: string(value.Source.Scope)},
		}
		info.Install = s.skillInstallInfo(cwd, value, globalLock, projectLock)
		if info.Install != nil && info.Install.Scope == scope && info.Install.Package == pkg {
			return info, nil
		}
	}
	return SkillInfo{}, ErrInstalledSkillMissing
}

func (s *Service) checkSkillUpdate(ctx context.Context, install SkillInstallInfo) SkillUpdateResult {
	result := SkillUpdateResult{
		Package: install.Package, Scope: install.Scope, State: SkillUpdateUnsupported,
		CurrentVersion: install.VersionHash,
	}
	if !install.CanCheckForUpdates || install.VersionHash == "" || install.SkillPath == "" {
		result.Message = "This lock entry cannot be checked automatically."
		return result
	}
	var latest string
	var err error
	if install.Scope == SkillScopeProject && len(install.VersionHash) != 40 {
		latest, err = s.skillsSnapshotHash(ctx, install.Source, install.Package)
	} else {
		latest, err = s.gitTreeHash(ctx, install.Source, install.Ref, path.Dir(filepath.ToSlash(install.SkillPath)))
	}
	if err != nil {
		result.State, result.Message = SkillUpdateError, err.Error()
		return result
	}
	result.LatestVersion = latest
	if latest == install.VersionHash {
		result.State = SkillUpToDate
	} else {
		result.State = SkillUpdateAvailable
	}
	return result
}

func (s *Service) skillsSnapshotHash(ctx context.Context, source, pkg string) (string, error) {
	parts := strings.Split(source, "/")
	if len(parts) != 2 {
		return "", errors.New("invalid skills.sh source")
	}
	name := pkg
	if at := strings.LastIndex(name, "@"); at >= 0 {
		name = name[at+1:]
	}
	name = skillSlug(name)
	endpoint := s.skillsAPI + "/api/download/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/" + url.PathEscape(name)
	var response struct {
		Hash string `json:"hash"`
	}
	if err := s.getJSON(ctx, endpoint, &response, false); err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Hash) == "" {
		return "", errors.New("skills.sh did not return a version hash")
	}
	return response.Hash, nil
}

func skillSlug(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastHyphen := false
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if valid {
			result.WriteRune(character)
			lastHyphen = false
			continue
		}
		if !lastHyphen && result.Len() != 0 {
			result.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func parseSkillPackage(value string) (skillPackage, error) {
	value = strings.TrimSpace(value)
	at := strings.LastIndex(value, "@")
	if at <= 0 || at == len(value)-1 {
		return skillPackage{}, fmt.Errorf("%w: package must use owner/repository@skill", ErrInvalidSkillRequest)
	}
	sourceRef, name := value[:at], value[at+1:]
	ref := ""
	if hash := strings.LastIndex(sourceRef, "#"); hash > 0 {
		ref, sourceRef = sourceRef[hash+1:], sourceRef[:hash]
	}
	source := normalizeSkillSource(sourceRef, "github")
	if !validGitHubSource(source) {
		return skillPackage{}, fmt.Errorf("%w: only GitHub owner/repository sources are supported", ErrInvalidSkillRequest)
	}
	if !skillNamePattern.MatchString(name) || strings.Contains(name, "--") {
		return skillPackage{}, fmt.Errorf("%w: skill name is invalid", ErrInvalidSkillRequest)
	}
	if strings.ContainsAny(ref, "\x00\r\n") {
		return skillPackage{}, fmt.Errorf("%w: skill ref is invalid", ErrInvalidSkillRequest)
	}
	return skillPackage{Source: source, Name: name, Ref: ref}, nil
}

func (s *Service) downloadSkill(ctx context.Context, pkg skillPackage, expectedPath string) (remoteSkill, error) {
	ref := pkg.Ref
	if ref == "" {
		ref = "HEAD"
	}
	endpoint := s.githubAPI + "/repos/" + pkg.Source + "/zipball/" + url.PathEscape(ref)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return remoteSkill{}, err
	}
	s.addGitHubHeaders(request)
	response, err := s.skillHTTP.Do(request)
	if err != nil {
		return remoteSkill{}, fmt.Errorf("download skill archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return remoteSkill{}, fmt.Errorf("download skill archive: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxSkillArchiveBytes+1))
	if err != nil {
		return remoteSkill{}, err
	}
	if len(archive) > maxSkillArchiveBytes {
		return remoteSkill{}, errors.New("skill archive is too large")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return remoteSkill{}, fmt.Errorf("open skill archive: %w", err)
	}
	type candidate struct {
		filePath string
		dir      string
	}
	var candidates []candidate
	for _, file := range reader.File {
		name, ok := archiveRelativePath(file.Name)
		if !ok || path.Base(name) != "SKILL.md" || file.FileInfo().IsDir() {
			continue
		}
		content, readErr := readZipFile(file, 256<<10)
		if readErr != nil {
			continue
		}
		metadata, parseErr := parseSkillMetadata(content, path.Base(path.Dir(name)))
		if parseErr != nil || metadata.Name != pkg.Name {
			continue
		}
		if expectedPath != "" && path.Clean(filepath.ToSlash(expectedPath)) != path.Clean(name) {
			continue
		}
		candidates = append(candidates, candidate{filePath: name, dir: path.Dir(name)})
	}
	if len(candidates) == 0 {
		return remoteSkill{}, fmt.Errorf("skill %q was not found in %s", pkg.Name, pkg.Source)
	}
	if len(candidates) > 1 {
		return remoteSkill{}, fmt.Errorf("skill %q is ambiguous in %s", pkg.Name, pkg.Source)
	}
	selected := candidates[0]
	files := make([]remoteSkillFile, 0)
	total := int64(0)
	for _, file := range reader.File {
		name, ok := archiveRelativePath(file.Name)
		if !ok || file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 {
			continue
		}
		relative, relErr := filepath.Rel(filepath.FromSlash(selected.dir), filepath.FromSlash(name))
		relative = filepath.ToSlash(relative)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, "../") || relative == "." {
			continue
		}
		remaining := maxInstalledSkillBytes - total
		if remaining < 0 || file.UncompressedSize64 > uint64(remaining) {
			return remoteSkill{}, errors.New("installed skill is too large")
		}
		content, readErr := readZipFile(file, remaining)
		if readErr != nil {
			return remoteSkill{}, readErr
		}
		total += int64(len(content))
		mode := file.Mode().Perm()
		if mode&0o111 != 0 {
			mode = 0o700
		} else {
			mode = 0o600
		}
		files = append(files, remoteSkillFile{Path: relative, Data: content, Mode: mode})
	}
	if len(files) == 0 {
		return remoteSkill{}, errors.New("skill archive contained no installable files")
	}
	treeHash, err := s.gitTreeHash(ctx, pkg.Source, pkg.Ref, selected.dir)
	if err != nil {
		return remoteSkill{}, fmt.Errorf("resolve remote skill version: %w", err)
	}
	return remoteSkill{
		Source: pkg.Source, Name: pkg.Name, Ref: pkg.Ref, SkillPath: selected.filePath,
		TreeHash: treeHash, Files: files,
	}, nil
}

func (s *Service) publishSkill(ctx context.Context, cwd string, scope SkillInstallScope, remote remoteSkill, replace bool) (err error) {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	root := filepath.Join(cwd, ".pi", "skills")
	lockPath := filepath.Join(cwd, "skills-lock.json")
	var linkPath string
	if scope == SkillScopeGlobal {
		root = s.globalSkillsDirectory()
		lockPath = s.globalSkillLockPath()
		linkPath = filepath.Join(s.paths.AgentDir, "skills", remote.Name)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".pi-go-skill-stage-*")
	if err != nil {
		return err
	}
	stageExists := true
	defer func() {
		if stageExists {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	for _, file := range remote.Files {
		target := filepath.Join(stage, filepath.FromSlash(file.Path))
		relative, relErr := filepath.Rel(stage, target)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("skill archive path escapes destination")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, file.Data, file.Mode); err != nil {
			return err
		}
	}
	destination := filepath.Join(root, remote.Name)
	_, destinationErr := os.Lstat(destination)
	if destinationErr == nil && !replace {
		return ErrSkillAlreadyInstalled
	}
	if destinationErr != nil && !errors.Is(destinationErr, os.ErrNotExist) {
		return destinationErr
	}
	backup := ""
	if destinationErr == nil {
		backup = filepath.Join(root, fmt.Sprintf(".pi-go-skill-backup-%d", time.Now().UnixNano()))
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	}
	rollback := func() error {
		var rollbackErr error
		if removeErr := os.RemoveAll(destination); removeErr != nil {
			rollbackErr = errors.Join(rollbackErr, removeErr)
		}
		if backup != "" {
			rollbackErr = errors.Join(rollbackErr, os.Rename(backup, destination))
		}
		return rollbackErr
	}
	if err := os.Rename(stage, destination); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	stageExists = false
	createdLink := false
	if linkPath != "" {
		if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
			return errors.Join(err, rollback())
		}
		linkInfo, linkErr := os.Lstat(linkPath)
		if errors.Is(linkErr, os.ErrNotExist) {
			if err := os.Symlink(destination, linkPath); err != nil {
				return errors.Join(err, rollback())
			}
			createdLink = true
		} else if linkErr != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
			return errors.Join(errors.New("global skill link path is occupied"), rollback())
		} else if resolved, resolveErr := filepath.EvalSymlinks(linkPath); resolveErr != nil || cleanPathKey(resolved) != cleanPathKey(destination) {
			return errors.Join(errors.New("global skill link points to another installation"), rollback())
		}
	}
	lock := readSkillLock(lockPath)
	entry := skillLockEntry{
		Source: remote.Source, SourceType: "github", SkillPath: remote.SkillPath, Ref: remote.Ref,
	}
	if scope == SkillScopeGlobal {
		entry.SkillFolderHash = remote.TreeHash
	} else {
		entry.ComputedHash = remote.TreeHash
	}
	lock[remote.Name] = entry
	lockVersion := 1
	if scope == SkillScopeGlobal {
		lockVersion = 3
	}
	if err := writeSkillLock(lockPath, lock, lockVersion); err != nil {
		if createdLink {
			_ = os.Remove(linkPath)
		}
		return errors.Join(err, rollback())
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("skill updated but old installation cleanup failed: %w", err)
		}
	}
	return nil
}

func writeSkillLock(lockPath string, skills map[string]skillLockEntry, version int) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(skillLockFile{Version: version, Skills: skills}, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(lockPath, encoded, 0o600)
}

func (s *Service) gitTreeHash(ctx context.Context, source, ref, folder string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	endpoint := s.githubAPI + "/repos/" + source + "/git/trees/" + url.PathEscape(ref) + "?recursive=1"
	var response struct {
		SHA  string `json:"sha"`
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := s.getJSON(ctx, endpoint, &response, true); err != nil {
		return "", err
	}
	folder = strings.Trim(path.Clean(filepath.ToSlash(folder)), "/")
	if folder == "" || folder == "." {
		if validGitHash(response.SHA) {
			return response.SHA, nil
		}
		return "", errors.New("GitHub did not return a repository tree hash")
	}
	for _, entry := range response.Tree {
		if entry.Type == "tree" && path.Clean(entry.Path) == folder && validGitHash(entry.SHA) {
			return entry.SHA, nil
		}
	}
	return "", errors.New("remote skill path was not found")
}

func (s *Service) getJSON(ctx context.Context, endpoint string, target any, github bool) error {
	request, err := http.NewRequestWithContext(normalizeContext(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "pi-go")
	if github {
		s.addGitHubHeaders(request)
	}
	response, err := s.skillHTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSkillResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxSkillResponseBytes {
		return errors.New("response is too large")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	return nil
}

func (s *Service) addGitHubHeaders(request *http.Request) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pi-go")
	token := environmentValue(s.production.Environment, "GITHUB_TOKEN")
	if token == "" {
		token = environmentValue(s.production.Environment, "GH_TOKEN")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
}

func archiveRelativePath(value string) (string, bool) {
	value = filepath.ToSlash(value)
	if strings.HasPrefix(value, "/") {
		return "", false
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return "", false
	}
	value = path.Clean(strings.Join(parts[1:], "/"))
	if value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return "", false
	}
	return value, true
}

func readZipFile(file *zip.File, limit int64) ([]byte, error) {
	if limit < 0 || file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("skill archive entry is too large")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("skill archive entry is too large")
	}
	return data, nil
}

func parseSkillMetadata(data []byte, fallback string) (struct{ Name string }, error) {
	var result struct{ Name string }
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return result, errors.New("skill frontmatter is required")
	}
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	end := bytes.Index(normalized[4:], []byte("\n---"))
	if end < 0 {
		return result, errors.New("skill frontmatter is malformed")
	}
	var front struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(normalized[4:4+end], &front); err != nil {
		return result, err
	}
	front.Name = strings.TrimSpace(front.Name)
	if front.Name == "" {
		front.Name = fallback
	}
	if !skillNamePattern.MatchString(front.Name) || strings.Contains(front.Name, "--") || strings.TrimSpace(front.Description) == "" {
		return result, errors.New("skill metadata is invalid")
	}
	result.Name = front.Name
	return result, nil
}

func validGitHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func formatSkillInstalls(count int64) string {
	if count <= 0 {
		return ""
	}
	format := func(value float64, suffix string) string {
		text := strconv.FormatFloat(value, 'f', 1, 64)
		text = strings.TrimSuffix(text, ".0")
		return text + suffix + " installs"
	}
	switch {
	case count >= 1_000_000:
		return format(float64(count)/1_000_000, "M")
	case count >= 1_000:
		return format(float64(count)/1_000, "K")
	case count == 1:
		return "1 install"
	default:
		return strconv.FormatInt(count, 10) + " installs"
	}
}

func parseFormattedInstalls(value string) float64 {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	number := fields[0]
	multiplier := float64(1)
	if strings.HasSuffix(number, "M") {
		multiplier, number = 1_000_000, strings.TrimSuffix(number, "M")
	} else if strings.HasSuffix(number, "K") {
		multiplier, number = 1_000, strings.TrimSuffix(number, "K")
	}
	parsed, _ := strconv.ParseFloat(number, 64)
	return parsed * multiplier
}
