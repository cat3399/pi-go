package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The boolean results report whether the externally visible commit point may
// have been crossed. Callers use that fact to distinguish retryable pre-write
// errors from outcomes that require reopen/reconciliation.
type sessionStorage interface {
	read(path string) ([]byte, error)
	create(path string, data []byte) (created bool, err error)
	append(ctx context.Context, path string, data []byte) (writeStarted bool, err error)
	validateReplace(path string) error
	replace(path string, data []byte) (replaced bool, err error)
}

type osSessionStorage struct{}

type rewriteTemporary interface {
	io.Writer
	Name() string
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type sessionRewriteOps interface {
	createTemp(directory, pattern string) (rewriteTemporary, error)
	replaceTemporary(temporaryPath, targetPath string) (replaced bool, err error)
	remove(path string) error
}

type osSessionRewriteOps struct{}

func (osSessionRewriteOps) createTemp(directory, pattern string) (rewriteTemporary, error) {
	return os.CreateTemp(directory, pattern)
}

func (osSessionRewriteOps) replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	return replaceTemporary(temporaryPath, targetPath)
}

func (osSessionRewriteOps) remove(path string) error { return os.Remove(path) }

func (osSessionStorage) validateReplace(path string) error {
	links, err := sessionLinkCount(path)
	if err != nil {
		return fmt.Errorf("%w: verify rewrite target %s: %v", ErrUnsafeWriterAlias, path, err)
	}
	if links != 1 {
		return fmt.Errorf("%w: rewrite target %s has %d hard links", ErrUnsafeWriterAlias, path, links)
	}
	return nil
}

func (osSessionStorage) read(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSessionBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrSessionTooLarge, info.Size(), maxSessionBytes)
	}
	return os.ReadFile(path)
}

func (osSessionStorage) create(path string, data []byte) (bool, error) {
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("session parent is not a directory: %s", directory)
	}
	// The parent must already exist. Creating a directory here would make a
	// successful return depend on syncing every newly-created ancestor entry.
	temporary, err := os.CreateTemp(directory, ".pi-go-session-*")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	cleanup := func() error {
		var result error
		if temporary != nil {
			result = errors.Join(result, temporary.Close())
			temporary = nil
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		return result
	}

	if err := temporary.Chmod(0o600); err != nil {
		return false, errors.Join(err, cleanup())
	}
	if err := writeAll(temporary, data); err != nil {
		return false, errors.Join(err, cleanup())
	}
	if err := temporary.Sync(); err != nil {
		return false, errors.Join(err, cleanup())
	}
	if err := temporary.Close(); err != nil {
		temporary = nil
		return false, errors.Join(err, cleanup())
	}
	temporary = nil

	published, publishErr := publishTemporary(temporaryPath, path)
	if publishErr != nil {
		return published, errors.Join(publishErr, cleanup())
	}
	return true, nil
}

func (osSessionStorage) append(ctx context.Context, path string, data []byte) (bool, error) {
	if cause := context.Cause(ctx); cause != nil {
		return false, cause
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return false, errors.Join(cause, file.Close())
	}
	// Cancellation is safe only before this boundary. Once Write starts, this
	// call synchronously settles write, sync, and close so no append can commit
	// after the caller has received a cancellation result.
	started := true
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return started, errors.Join(writeErr, file.Close())
	}
	if syncErr := file.Sync(); syncErr != nil {
		return started, errors.Join(syncErr, file.Close())
	}
	if closeErr := file.Close(); closeErr != nil {
		return started, closeErr
	}
	return started, nil
}

// replace writes a complete private sibling, syncs it, atomically swaps it in
// place, then requests the platform's metadata-durability boundary (directory
// sync on Unix; write-through rename plus file flush on Windows). The boolean
// means that the visible name may already point to the replacement, so callers
// must fail closed on an error.
func (osSessionStorage) replace(path string, data []byte) (bool, error) {
	if err := (osSessionStorage{}).validateReplace(path); err != nil {
		return false, err
	}
	return replaceSessionFile(osSessionRewriteOps{}, path, data)
}

func replaceSessionFile(ops sessionRewriteOps, path string, data []byte) (bool, error) {
	directory := filepath.Dir(path)
	temporary, err := ops.createTemp(directory, ".pi-go-session-rewrite-*")
	if err != nil {
		return false, fmt.Errorf("create rewrite temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() error {
		var result error
		if temporary != nil {
			if err := temporary.Close(); err != nil {
				result = errors.Join(result, fmt.Errorf("close rewrite temporary during cleanup: %w", err))
			}
			temporary = nil
		}
		if err := ops.remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove rewrite temporary %s: %w", temporaryPath, err))
		}
		return result
	}
	if err := temporary.Chmod(0o600); err != nil {
		return false, errors.Join(fmt.Errorf("chmod rewrite temporary: %w", err), cleanup())
	}
	if err := writeAll(temporary, data); err != nil {
		return false, errors.Join(fmt.Errorf("write rewrite temporary: %w", err), cleanup())
	}
	if err := temporary.Sync(); err != nil {
		return false, errors.Join(fmt.Errorf("sync rewrite temporary: %w", err), cleanup())
	}
	if err := temporary.Close(); err != nil {
		return false, errors.Join(fmt.Errorf("close rewrite temporary: %w", err), cleanup())
	}
	temporary = nil
	replaced, err := ops.replaceTemporary(temporaryPath, path)
	if err != nil {
		publicationErr := fmt.Errorf("publish rewrite temporary: %w", err)
		if replaced {
			// The target name may already refer to these bytes. Never clean via a
			// path operation after the publication boundary.
			return true, publicationErr
		}
		return false, errors.Join(publicationErr, cleanup())
	}
	return true, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
