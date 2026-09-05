package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// InstallKnowledge installs a build's embedded sources only when its target is
// absent. Existing installations belong to the user and are used as-is. The
// first installation is published atomically so concurrent starts share it.
func InstallKnowledge(ctx context.Context, agentDir string, extra []SourceBundle) (product.Documentation, error) {
	base, err := coreSources()
	if err != nil {
		return product.Documentation{}, err
	}
	files := append([]sourceFile(nil), base...)
	for _, bundle := range extra {
		added, err := readBundle(bundle)
		if err != nil {
			return product.Documentation{}, err
		}
		files = append(files, added...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	manifest := knowledgeManifest{Version: product.Version, Files: make(map[string]string, len(files))}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00", product.Version)
	for _, file := range files {
		if file.name == "manifest.json" {
			return product.Documentation{}, errors.New("manifest.json is reserved for installation metadata")
		}
		if _, exists := manifest.Files[file.name]; exists {
			return product.Documentation{}, fmt.Errorf("duplicate source file %q", file.name)
		}
		manifest.Files[file.name] = file.digest
		_, _ = fmt.Fprintf(hash, "%s\x00%s\n", file.name, file.digest)
	}
	manifest.BuildID = hex.EncodeToString(hash.Sum(nil))
	root := product.KnowledgeDirectory(agentDir)
	target := filepath.Join(root, manifest.BuildID)
	documentation := product.Documentation{Version: manifest.Version, BuildID: manifest.BuildID, ReadmePath: filepath.Join(target, "README.md"), DocsPath: filepath.Join(target, "docs"), SourcePath: target}
	if _, err := os.Lstat(target); err == nil {
		return documentation, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return product.Documentation{}, err
	}

	release, err := lock(ctx, filepath.Join(root, ".install.lock"))
	if err != nil {
		return product.Documentation{}, err
	}
	defer release()
	// Another process may have completed installation while we waited.
	if _, err := os.Lstat(target); err == nil {
		return documentation, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return product.Documentation{}, err
	}
	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return product.Documentation{}, err
	}
	if err := writeKnowledge(ctx, stage, files, manifest); err != nil {
		return product.Documentation{}, fmt.Errorf("install knowledge (staging files retained at %s): %w", stage, err)
	}
	if err := ctx.Err(); err != nil {
		return product.Documentation{}, context.Cause(ctx)
	}
	if err := publishDirectory(stage, target); err != nil {
		return product.Documentation{}, fmt.Errorf("publish knowledge (staging files retained at %s): %w", stage, err)
	}
	if err := syncDirectory(root); err != nil {
		return product.Documentation{}, err
	}
	return documentation, nil
}

func writeKnowledge(ctx context.Context, stage string, files []sourceFile, manifest knowledgeManifest) error {
	directories := map[string]struct{}{stage: {}}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		target := filepath.Join(stage, filepath.FromSlash(file.name))
		directory := filepath.Dir(target)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		for current := directory; strings.HasPrefix(current, stage) && current != filepath.Dir(stage); current = filepath.Dir(current) {
			directories[current] = struct{}{}
			if current == stage {
				break
			}
		}
		if err := writeNewFile(target, file.data, 0o644); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNewFile(filepath.Join(stage, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		return err
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
