package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/product"
)

func TestDesktopInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want launchOptions
	}{
		{name: "no arguments"},
		{"desktop options", []string{"--cwd=project", "-agent-dir", "agent", "--docs-dir", "docs", "--remote", "https://example.org", "-devtools"},
			launchOptions{cwd: "project", agentDir: "agent", docsDir: "docs", defaultRemote: "https://example.org", devToolsEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			open := func(got launchOptions) error {
				calls++
				if got != tc.want {
					t.Errorf("desktop options = %+v, want %+v", got, tc.want)
				}
				return nil
			}
			var stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), io.Discard, &stderr, open); code != 0 || calls != 1 {
				t.Fatalf("exit %d, opened %d windows: %s", code, calls, &stderr)
			}
		})
	}
}

func invokeCLI(t *testing.T, input string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(input), &stdout, &stderr, func(launchOptions) error {
		t.Fatalf("%v attempted to open a desktop window", args)
		return nil
	})
	return stdout.String(), stderr.String(), code
}

func TestHeadlessCommands(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		code     int
		out, err string
	}{
		{[]string{"help"}, 0, "Desktop: pi-go-gui", ""},
		{[]string{"--help"}, 0, "Usage: pi-go-gui <command>", ""},
		{[]string{"-h"}, 0, "  rpc", ""},
		{[]string{"version"}, 0, product.Version, ""},
		{[]string{"-v"}, 0, product.Version, ""},
		{[]string{"--version"}, 0, product.Version, ""},
		{[]string{"run"}, 1, "", "-p prompt is required"},
		{[]string{"tui", "--fps", "0"}, 2, "", "--fps"},
		{[]string{"tui", "--help"}, 0, "", "pi-go-gui tui"},
		{[]string{"web", "--help"}, 0, "", "pi-go-gui web"},
		{[]string{"models", "--help"}, 0, "pi-go-gui models update", ""},
		{[]string{"session", "context", "--help"}, 0, "pi-go-gui session", ""},
		{[]string{"update", "--help"}, 0, "", "pi-go-gui update"},
		{[]string{"cwd"}, 2, "", `unknown command "cwd"`},
		{[]string{"--typo"}, 2, "", `unknown command "--typo"`},
		{[]string{"--cwd"}, 2, "", "needs an argument"},
		{[]string{"--cwd=project", "tui"}, 2, "", "unexpected positional argument"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err, code := invokeCLI(t, "", tc.args...)
			matches := func(got, want string) bool {
				if want == "" {
					return got == ""
				}
				return strings.Contains(got, want)
			}
			if code != tc.code || !matches(out, tc.out) || !matches(err, tc.err) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, err)
			}
		})
	}
}

func TestHeadlessRPCUsesStdio(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	t.Setenv("PI_GO_AGENT_DIR", agentDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	out, stderr, code := invokeCLI(t, "{\"id\":\"gui-cli\",\"type\":\"get_state\"}\n", "rpc", "--cwd", t.TempDir())
	var response struct {
		ID, Type, Command string
		Success           bool
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil || code != 0 || !response.Success || response.Type != "response" || response.ID != "gui-cli" || response.Command != "get_state" {
		t.Fatalf("exit %d, response %s (%v), stderr %s", code, out, err, stderr)
	}
}
