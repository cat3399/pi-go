package product

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectoryResolutionHasAnIndependentNamespace(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	cwd := t.TempDir()
	tests := []struct {
		explicit string
		env      []string
		want     string
	}{
		{"", []string{"PI_CODING_AGENT_DIR=/legacy"}, filepath.Join(home, ".pi-go", "agent")},
		{"", []string{"PI_GO_AGENT_DIR=custom"}, filepath.Join(cwd, "custom")},
		{"chosen", []string{"PI_GO_AGENT_DIR=ignored"}, filepath.Join(cwd, "chosen")},
		{"~/chosen", []string{}, filepath.Join(home, "chosen")},
	}
	for _, test := range tests {
		got, err := ResolveAgentDirectory(test.explicit, cwd, test.env)
		if err != nil || got != test.want {
			t.Fatalf("path = %q / %v, want %q", got, err, test.want)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatal("path resolution wrote files")
	}
}
