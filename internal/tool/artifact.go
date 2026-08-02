package tool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"
)

type artifactFile interface {
	io.Writer
	Close() error
}

type artifactFactory interface {
	create() (artifactFile, string, error)
}

var (
	ErrArtifactSecurity = errors.New("secure bash output artifact unavailable")
	ErrArtifactIO       = errors.New("bash output artifact I/O failure")
)

type artifactStore struct {
	mu            sync.Mutex
	requestedRoot string
	root          string
}

func newArtifactStore(root string) (*artifactStore, error) {
	if root != "" {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve artifact directory: %w", ErrInvalidBashOptions, err)
		}
		root = filepath.Clean(absolute)
	}
	return &artifactStore{requestedRoot: root}, nil
}

func (s *artifactStore) create() (artifactFile, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := platformArtifactSupport(); err != nil {
		return nil, "", err
	}
	root, err := s.ensureRootLocked()
	if err != nil {
		return nil, "", err
	}

	file, err := os.CreateTemp(root, "pi-bash-*.log")
	if err != nil {
		return nil, "", fmt.Errorf("%w: create file: %w", ErrArtifactIO, err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: set file mode for %s: %w", ErrArtifactSecurity, path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: inspect file %s: %w", ErrArtifactSecurity, path, err)
	}
	if err := validatePrivateArtifactFile(info); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: %s: %w", ErrArtifactSecurity, path, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: resolve file path: %w", ErrArtifactIO, err)
	}
	if !utf8.ValidString(absolute) {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: artifact path is not valid UTF-8", ErrArtifactSecurity)
	}
	return file, filepath.Clean(absolute), nil
}

func (s *artifactStore) ensureRootLocked() (string, error) {
	if s.root != "" {
		return s.root, nil
	}

	if s.requestedRoot == "" {
		root, err := os.MkdirTemp("", "pi-go-output-")
		if err != nil {
			return "", fmt.Errorf("%w: create private directory: %w", ErrArtifactIO, err)
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return "", fmt.Errorf("%w: set directory mode for %s: %w", ErrArtifactSecurity, root, err)
		}
		s.requestedRoot = root
	} else {
		info, err := os.Lstat(s.requestedRoot)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%w: artifact directory must not be a symlink: %s", ErrArtifactSecurity, s.requestedRoot)
			}
		case errors.Is(err, os.ErrNotExist):
			parent := filepath.Dir(s.requestedRoot)
			parentInfo, parentErr := os.Stat(parent)
			if parentErr != nil {
				return "", fmt.Errorf("%w: artifact directory parent %s: %w", ErrArtifactIO, parent, parentErr)
			}
			if !parentInfo.IsDir() {
				return "", fmt.Errorf("%w: artifact directory parent is not a directory: %s", ErrArtifactIO, parent)
			}
			if err := os.Mkdir(s.requestedRoot, 0o700); err != nil {
				return "", fmt.Errorf("%w: create directory %s: %w", ErrArtifactIO, s.requestedRoot, err)
			}
			if err := os.Chmod(s.requestedRoot, 0o700); err != nil {
				return "", fmt.Errorf("%w: set directory mode for %s: %w", ErrArtifactSecurity, s.requestedRoot, err)
			}
		default:
			return "", fmt.Errorf("%w: inspect directory %s: %w", ErrArtifactIO, s.requestedRoot, err)
		}
	}

	info, err := os.Lstat(s.requestedRoot)
	if err != nil {
		return "", fmt.Errorf("%w: inspect directory %s: %w", ErrArtifactSecurity, s.requestedRoot, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: artifact path is not a private directory: %s", ErrArtifactSecurity, s.requestedRoot)
	}
	if err := validatePrivateArtifactDirectory(info); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrArtifactSecurity, s.requestedRoot, err)
	}
	s.root = s.requestedRoot
	return s.root, nil
}
