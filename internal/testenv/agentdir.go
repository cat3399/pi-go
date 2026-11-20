// Package testenv provides filesystem fixtures for production integration tests.
package testenv

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cat3399/pi-go/internal/installation"
)

var sharedRoot func() (string, error)

// WithSharedKnowledge runs a test package with one private installation. Prepare
// it on first use, outside runtime deadlines; unit tests and helper subprocesses
// that don't open a production runtime need no installation at all.
func WithSharedKnowledge(m *testing.M) {
	tmp := os.TempDir() // Individual tests may override TMPDIR.
	var root string
	sharedRoot = sync.OnceValues(func() (string, error) {
		var err error
		root, err = os.MkdirTemp(tmp, "pi-go-test-install-")
		if err != nil {
			return "", err
		}
		_, err = installation.InstallKnowledge(context.Background(), filepath.Join(root, "agent"), nil)
		return root, err
	})
	defer func() {
		if root != "" {
			if err := os.RemoveAll(root); err != nil {
				panic(err)
			}
		}
	}()
	m.Run() // The testing wrapper propagates the exit code when TestMain returns.
}

// AgentDir returns a fresh, empty agent directory. Only its sibling knowledge/
// tree is reused; tests that modify that tree must use a separate t.TempDir().
func AgentDir(t testing.TB) string {
	t.Helper()
	if sharedRoot == nil {
		t.Fatal("AgentDir requires testenv.WithSharedKnowledge in TestMain")
	}
	root, err := sharedRoot()
	if err != nil {
		t.Fatalf("prepare shared test installation: %v", err)
	}
	dir, err := os.MkdirTemp(root, "agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
