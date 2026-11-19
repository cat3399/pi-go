package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	pigo "github.com/cat3399/pi-go"
	"github.com/cat3399/pi-go/internal/product"
)

// SourceBundle adds the maintained inputs of an independently built module.
// Prefix is the module's path in the product source tree, e.g. surface/gui.
type SourceBundle struct {
	Prefix string
	Files  fs.FS
}

type sourceFile struct {
	name   string
	data   []byte
	digest string
}

type knowledgeManifest struct {
	Version string            `json:"version"`
	BuildID string            `json:"buildId"`
	Files   map[string]string `json:"files"`
}

var coreSources = sync.OnceValues(func() ([]sourceFile, error) {
	return readBundle(SourceBundle{Files: pigo.Sources})
})

func readBundle(bundle SourceBundle) ([]sourceFile, error) {
	if bundle.Files == nil || bundle.Prefix != "" && (!fs.ValidPath(bundle.Prefix) || bundle.Prefix == ".") {
		return nil, errors.New("invalid source bundle")
	}
	var files []sourceFile
	err := fs.WalkDir(bundle.Files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(bundle.Files, name)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		files = append(files, sourceFile{name: path.Join(bundle.Prefix, name), data: data, digest: hex.EncodeToString(digest[:])})
		return nil
	})
	return files, err
}

const receiptsDir = ".pi-go-bundles"

type knowledgePart struct {
	files    []sourceFile
	manifest knowledgeManifest
}

func knowledgeParts(version string, stable bool, base []sourceFile, extra []SourceBundle) ([]knowledgePart, error) {
	groups := [][]sourceFile{base}
	for _, bundle := range extra {
		files, err := readBundle(bundle)
		if err != nil {
			return nil, err
		}
		if len(files) != 0 {
			groups = append(groups, files)
		}
	}
	if !stable && len(groups) > 1 {
		// Unreleased builds can change even just their GUI inputs under the same version.
		groups = [][]sourceFile{slices.Concat(groups...)}
	}
	var parts []knowledgePart
	seen := make(map[string]bool)
	for _, files := range groups {
		m := knowledgeManifest{Version: version, Files: make(map[string]string, len(files))}
		for _, f := range files {
			top, _, _ := strings.Cut(f.name, "/")
			if top == receiptsDir || top == "manifest.json" {
				return nil, fmt.Errorf("source path %q is reserved for installation metadata", f.name)
			}
			if _, err := filepath.Localize(f.name); err != nil {
				return nil, err
			}
			if seen[f.name] {
				return nil, fmt.Errorf("duplicate source file %q", f.name)
			}
			seen[f.name] = true
			m.Files[f.name] = f.digest
		}
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%s\x00", version)
		for _, name := range slices.Sorted(maps.Keys(m.Files)) {
			_, _ = fmt.Fprintf(hash, "%s\x00%s\n", name, m.Files[name])
		}
		m.BuildID = hex.EncodeToString(hash.Sum(nil))
		parts = append(parts, knowledgePart{files: files, manifest: m})
	}
	return parts, nil
}

func (p knowledgePart) receipt() string { return receiptsDir + "/" + p.manifest.BuildID + ".json" }

// InstallKnowledge publishes the common release tree once. Optional module inputs
// are additive, regardless of which binary starts first. Receipts, not the state
// of user-editable source files, determine whether a module has been installed.
func InstallKnowledge(ctx context.Context, agentDir string, extra []SourceBundle) (product.Documentation, error) {
	base, err := coreSources()
	if err != nil {
		return product.Documentation{}, err
	}
	return installKnowledge(ctx, product.KnowledgeDirectory(agentDir), product.Version, base, extra)
}

func installKnowledge(ctx context.Context, root, version string, base []sourceFile, extra []SourceBundle) (product.Documentation, error) {
	release, stable := product.StableVersion(version)
	if stable {
		version = release
	}
	parts, err := knowledgeParts(version, stable, base, extra)
	if err != nil {
		return product.Documentation{}, err
	}
	id := parts[0].manifest.BuildID
	locked := false
	for {
		target, exists, err := knowledgeTarget(root, release, id)
		if err != nil {
			return product.Documentation{}, err
		}
		missing := parts[1:]
		if exists {
			missing = nil
			for _, p := range parts[1:] {
				if _, err := os.Lstat(filepath.Join(target, p.receipt())); errors.Is(err, fs.ErrNotExist) {
					missing = append(missing, p)
				} else if err != nil {
					return product.Documentation{}, err
				}
			}
		}
		if !exists || len(missing) != 0 {
			if !locked {
				unlock, err := lock(ctx, filepath.Join(root, ".install.lock"))
				if err != nil {
					return product.Documentation{}, err
				}
				defer unlock()
				locked = true
				continue // Recheck after concurrent installers finish.
			}
			if !exists {
				err = publishKnowledge(ctx, root, target, parts)
			} else {
				for _, p := range missing {
					if err = supplementKnowledge(ctx, root, target, p); err != nil {
						break
					}
				}
			}
			if err != nil {
				return product.Documentation{}, err
			}
		}
		return product.Documentation{Version: version, BuildID: id, ReadmePath: filepath.Join(target, "README.md"), DocsPath: filepath.Join(target, "docs"), SourcePath: target}, nil
	}
}

func knowledgeTarget(root, release, id string) (string, bool, error) {
	for length := 12; length <= len(id); length += 4 {
		name := release
		if name == "" {
			name = "dev-" + id[:length]
		}
		target := filepath.Join(root, name)
		info, err := os.Lstat(target)
		if errors.Is(err, fs.ErrNotExist) {
			return target, false, nil
		}
		if err != nil {
			return "", false, err
		}
		if info.IsDir() {
			meta, err := os.Lstat(filepath.Join(target, receiptsDir))
			if err == nil && meta.IsDir() {
				if _, err = os.Lstat(filepath.Join(target, receiptsDir, id)); err == nil {
					return target, true, nil
				}
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", false, err
			}
		}
		if release != "" {
			return "", false, fmt.Errorf("knowledge directory %s does not identify the common sources for release %s; refusing to overwrite it", target, release)
		}
	}
	return "", false, fmt.Errorf("no unused knowledge directory for build %s", id)
}

func manifestFile(name string, m knowledgeManifest) (sourceFile, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	return sourceFile{name: name, data: append(data, '\n')}, err
}

func publishKnowledge(ctx context.Context, root, target string, parts []knowledgePart) error {
	// The private identity stamp remains usable even if the public manifest is edited or moved.
	files := []sourceFile{{name: receiptsDir + "/" + parts[0].manifest.BuildID}}
	for i, p := range parts {
		name := p.receipt()
		if i == 0 {
			name = "manifest.json"
		}
		manifest, err := manifestFile(name, p.manifest)
		if err != nil {
			return err
		}
		files = append(files, p.files...)
		files = append(files, manifest)
	}
	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return err
	}
	if err := writeKnowledge(ctx, stage, files); err != nil {
		return fmt.Errorf("install knowledge (staging files retained at %s): %w", stage, err)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := publishDirectory(stage, target); err != nil {
		return fmt.Errorf("publish knowledge (staging files retained at %s): %w", stage, err)
	}
	return syncDirectory(root)
}

func supplementKnowledge(ctx context.Context, root, target string, p knowledgePart) error {
	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return err
	}
	dataDir := filepath.Join(stage, "files")
	manifest, err := manifestFile("receipt", p.manifest)
	if err == nil {
		err = writeKnowledge(ctx, dataDir, p.files)
	}
	if err == nil {
		err = writeKnowledge(ctx, stage, []sourceFile{manifest})
	}
	if err == nil {
		err = mergeKnowledge(ctx, dataDir, target)
	}
	if err == nil {
		// Publish completion only after the source entries are durable. A retry
		// can merge a partially published module without replacing any files.
		err = publishDirectory(filepath.Join(stage, manifest.name), filepath.Join(target, p.receipt()))
	}
	if err == nil {
		err = syncDirectory(filepath.Join(target, receiptsDir))
	}
	if err != nil {
		return fmt.Errorf("supplement knowledge (staging files retained at %s): %w", stage, err)
	}
	return os.RemoveAll(stage) // Only unpublished copies from our own staging tree.
}

// Move whole new subtrees atomically. Recurse only into real directories; a
// user-created file or symlink must never become a write destination.
func mergeKnowledge(ctx context.Context, stage, target string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		from, to := filepath.Join(stage, entry.Name()), filepath.Join(target, entry.Name())
		info, err := os.Lstat(to)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := publishDirectory(from, to); err != nil {
				return err
			}
		case err != nil:
			return err
		case entry.IsDir():
			if !info.IsDir() {
				return fmt.Errorf("cannot install module through non-directory %s", to)
			}
			if err := mergeKnowledge(ctx, from, to); err != nil {
				return err
			}
		}
	}
	return syncDirectory(target)
}

func writeKnowledge(ctx context.Context, stage string, files []sourceFile) error {
	directories := map[string]struct{}{stage: {}}
	for _, file := range files {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		target := filepath.Join(stage, filepath.FromSlash(file.name))
		directory := filepath.Dir(target)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		for current := directory; current != stage; current = filepath.Dir(current) {
			directories[current] = struct{}{}
		}
		if err := writeNewFile(target, file.data, 0o644); err != nil {
			return err
		}
	}
	return syncDirectories(directories)
}

func writeNewFile(path string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close())
}

func syncDirectories(directories map[string]struct{}) error {
	paths := make([]string, 0, len(directories))
	for directory := range directories {
		paths = append(paths, directory)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, directory := range paths {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}
