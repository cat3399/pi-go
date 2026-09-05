package installation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cat3399/pi-go/internal/product"
)

const migrationRecordName = ".migration.json"

var agentEntries = []string{
	"auth.json", "models.json", "settings.json", "trust.json", "projects.json", "keybindings.json",
	"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD", "SYSTEM.md", "APPEND_SYSTEM.md",
	"skills", "prompts", "sessions",
}

var projectEntries = []string{"settings.json", "SYSTEM.md", "APPEND_SYSTEM.md", "skills", "prompts"}

type migrationRecord struct {
	Version     int      `json:"version"`
	Source      string   `json:"source,omitempty"`
	Files       []string `json:"files"`
	Skipped     []string `json:"skipped,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// InitializeAgent imports legacy data only when the product selected its own
// default directory. An explicit agent directory is an independent installation.
// The legacy environment variable is consulted solely to locate an import source.
func InitializeAgent(ctx context.Context, target, cwd string, environment []string, importLegacy bool) error {
	if !importLegacy {
		return nil
	}
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	source := product.EnvironmentValue(environment, "PI_CODING_AGENT_DIR")
	if source == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		source = filepath.Join(home, ".pi", "agent")
	}
	source, err := product.ResolvePath(source, cwd)
	if err != nil {
		return fmt.Errorf("resolve legacy agent directory: %w", err)
	}
	return initializeDirectory(ctx, source, target, cwd, agentEntries)
}

// InitializeProject is called by the application before project discovery.
// Shared project instructions (AGENTS.md) remain at the project root.
func InitializeProject(ctx context.Context, cwd string) error {
	source := filepath.Join(cwd, ".pi")
	target := product.ProjectDirectory(cwd)
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return initializeDirectory(ctx, source, target, cwd, projectEntries)
}

func initializeDirectory(ctx context.Context, source, target, cwd string, entries []string) error {
	parent := filepath.Dir(target)
	release, err := lock(ctx, filepath.Join(parent, "."+filepath.Base(target)+"-initialize.lock"))
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if source != "" {
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			source = ""
		} else if err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("legacy configuration is not a directory: %s", source)
		}
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(target)+"-import-")
	if err != nil {
		return err
	}
	if err := copyInstallation(ctx, source, target, stage, cwd, entries); err != nil {
		return fmt.Errorf("initialize %s (staging data retained at %s): %w", target, stage, err)
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if err := publishDirectory(stage, target); err != nil {
		return fmt.Errorf("publish %s (staging data retained at %s): %w", target, stage, err)
	}
	return syncDirectory(parent)
}

type migrationCopy struct {
	ctx                                        context.Context
	source, target, stage, cwd, physicalSource string
	entries                                    map[string]struct{}
	directories                                map[string]struct{}
	record                                     migrationRecord
}

func copyInstallation(ctx context.Context, source, target, stage, cwd string, entries []string) error {
	copy := migrationCopy{ctx: ctx, source: source, target: target, stage: stage, cwd: cwd, entries: map[string]struct{}{}, directories: map[string]struct{}{stage: {}}, record: migrationRecord{Version: 1, Source: source, Files: []string{}}}
	for _, name := range entries {
		copy.entries[name] = struct{}{}
	}
	if source != "" {
		physical, err := filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		copy.physicalSource = physical
		children, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		// Copy actual directory entries, using supported spellings only as an
		// allowlist. AGENTS.md and AGENTS.MD can resolve to the same entry on a
		// case-insensitive volume; distinct entries must still stay distinct.
		for _, child := range children {
			if _, ok := copy.entries[child.Name()]; !ok {
				copy.record.Skipped = append(copy.record.Skipped, child.Name())
				continue
			}
			if err := copy.entry(child.Name()); err != nil {
				return err
			}
		}
	}
	sort.Strings(copy.record.Files)
	encoded, err := json.MarshalIndent(copy.record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNewFile(filepath.Join(stage, migrationRecordName), append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return syncDirectories(copy.directories)
}

func (m *migrationCopy) entry(relative string) error {
	if err := m.ctx.Err(); err != nil {
		return context.Cause(m.ctx)
	}
	source := filepath.Join(m.source, relative)
	target := filepath.Join(m.stage, relative)
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return m.symlink(relative)
	}
	if info.IsDir() {
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		m.directories[target] = struct{}{}
		children, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, child := range children {
			// Writer locks and unfinished temporary files carry no durable
			// session semantics and must not become part of an import.
			if strings.HasPrefix(filepath.ToSlash(relative)+"/", "sessions/") && (strings.HasSuffix(child.Name(), ".lock") || strings.HasPrefix(child.Name(), ".pi-go-")) {
				m.record.Skipped = append(m.record.Skipped, filepath.Join(relative, child.Name()))
				continue
			}
			if err := m.entry(filepath.Join(relative, child.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported legacy file type: %s", source)
	}
	return m.regularFile(relative, info)
}

func (m *migrationCopy) symlink(relative string) error {
	source := filepath.Join(m.source, relative)
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve legacy link %s: %w", source, err)
	}
	// Credentials and configuration become private regular files even when the
	// old installation obtained them through a link.
	if !strings.ContainsRune(relative, filepath.Separator) && strings.HasSuffix(relative, ".json") {
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("configuration link is not a file: %s", source)
		}
		return m.regularFile(relative, info)
	}
	target := resolved
	if rel, ok := relativeWithin(m.physicalSource, resolved); ok {
		top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		if _, included := m.entries[top]; !included {
			return fmt.Errorf("legacy link %s points into an unsupported resource %s", source, top)
		}
		target = filepath.Join(m.target, rel)
	}
	// External shared resources retain their intentional references; links
	// within the imported tree are rebased to the new tree.
	if err := os.Symlink(target, filepath.Join(m.stage, relative)); err != nil {
		return err
	}
	m.record.Files = append(m.record.Files, filepath.ToSlash(relative))
	return nil
}

func (m *migrationCopy) regularFile(relative string, info os.FileInfo) error {
	source := filepath.Join(m.source, relative)
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("legacy file changed type: %s", source)
	}
	target := filepath.Join(m.stage, relative)
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600|info.Mode().Perm()&0o100)
	if err != nil {
		return err
	}
	defer output.Close()
	reader := bufio.NewReader(contextReader{ctx: m.ctx, reader: input})
	if relative == "settings.json" {
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		data = m.rebaseSettings(data)
		if _, err := output.Write(data); err != nil {
			return err
		}
	} else if strings.HasPrefix(filepath.ToSlash(relative), "sessions/") && strings.HasSuffix(relative, ".jsonl") {
		first, err := reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		first = m.rebaseSessionHeader(first, relative)
		if _, err := output.Write(first); err != nil {
			return err
		}
		if _, err := io.Copy(output, reader); err != nil {
			return err
		}
	} else {
		if _, err := io.Copy(output, reader); err != nil {
			return err
		}
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("legacy file changed during import; retry after its writer finishes: %s", source)
	}
	current, err := os.Stat(source)
	if err != nil || !os.SameFile(before, current) {
		return fmt.Errorf("legacy file was replaced during import: %s", source)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	m.record.Files = append(m.record.Files, filepath.ToSlash(relative))
	return nil
}

func (m *migrationCopy) rebaseSettings(data []byte) []byte {
	object, err := decodeMigrationObject(data)
	if err != nil {
		m.record.Diagnostics = append(m.record.Diagnostics, "settings.json is ambiguous or malformed; original bytes preserved")
		return data
	}
	changed := false
	for _, key := range []string{"skills", "prompts"} {
		var values []string
		if json.Unmarshal(object[key], &values) != nil {
			continue
		}
		localChange := false
		for index, value := range values {
			if next := m.rebasePath(value); next != value {
				values[index] = next
				localChange = true
			}
		}
		if localChange {
			object[key], _ = json.Marshal(values)
			changed = true
		}
	}
	var sessionDir string
	if json.Unmarshal(object["sessionDir"], &sessionDir) == nil {
		if next := m.rebasePath(sessionDir); next != sessionDir {
			object["sessionDir"], _ = json.Marshal(next)
			changed = true
		}
	}
	if !changed {
		return data
	}
	encoded, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return data
	}
	return append(encoded, '\n')
}

func (m *migrationCopy) rebaseSessionHeader(data []byte, relative string) []byte {
	object, err := decodeMigrationObject(data)
	if err != nil {
		m.record.Diagnostics = append(m.record.Diagnostics, filepath.ToSlash(relative)+": ambiguous or malformed header; original bytes preserved")
		return data
	}
	var parent string
	if json.Unmarshal(object["parentSession"], &parent) != nil {
		return data
	}
	next := m.rebasePath(parent)
	if next == parent {
		return data
	}
	object["parentSession"], _ = json.Marshal(next)
	encoded, err := json.Marshal(object)
	if err != nil {
		return data
	}
	if bytes.HasSuffix(data, []byte{'\n'}) {
		encoded = append(encoded, '\n')
	}
	return encoded
}

// Only rewrite unambiguous objects. In particular, decoding duplicate keys into
// a map would silently discard source data when an unrelated path is rebased.
func decodeMigrationObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected an object key")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("duplicate object key: %s", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after JSON object")
	}
	return object, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.reader.Read(buffer)
}

func (m *migrationCopy) rebasePath(value string) string {
	if value == "" {
		return value
	}
	prefix := ""
	path := value
	if strings.HasPrefix(path, "!") {
		prefix = "!"
		path = path[1:]
	}
	resolved, err := product.ResolvePath(path, m.cwd)
	if err != nil {
		return value
	}
	for _, root := range []string{m.source, m.physicalSource} {
		if relative, ok := relativeWithin(root, resolved); ok {
			return prefix + filepath.Join(m.target, relative)
		}
	}
	return value
}

func relativeWithin(root, target string) (string, bool) {
	if root == "" {
		return "", false
	}
	relative, err := filepath.Rel(root, target)
	return relative, err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
