package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/product"
)

var terminalCLI = CommandLine{Build: product.CLIBuildInfo()}

func TestUnifiedCommandShowsAvailableSurfaces(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := terminalCLI.Run(nil, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, command := range []string{"run", "tui", "web", "rpc", "models", "session", "update", "version", "help"} {
		if !strings.Contains(stdout.String(), "  "+command) {
			t.Fatalf("usage does not contain %q: %s", command, stdout.String())
		}
	}
}

func TestHelpAndVersionDoNotInitializeUserData(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(product.AgentDirectoryEnvironment, "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	for _, flag := range []string{"--help", "--version"} {
		var stdout, stderr bytes.Buffer
		if code := terminalCLI.Run([]string{flag}, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d: %s", flag, code, &stderr)
		}
		if flag == "--version" && stdout.String() != terminalCLI.Build.Version+"\n" {
			t.Fatalf("--version = %q", stdout.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, product.DirectoryName)); !os.IsNotExist(err) {
		t.Fatalf("help/version should leave user data absent: stat error = %v", err)
	}
}

func TestUpdateHelpDoesNotOpenApplication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := terminalCLI.Run([]string{"update", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pi-go update") {
		t.Fatalf("missing update help: %s", stderr.String())
	}
}

func TestTUIHelpDoesNotOpenApplication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := terminalCLI.Run([]string{"tui", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}

func TestUnifiedCommandRejectsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := terminalCLI.Run([]string{"unknown"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "unknown"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWebHelpDoesNotRequireEmbeddedAssets(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := terminalCLI.Run([]string{"web", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRemoteWebListenRequiresPassword(t *testing.T) {
	for _, address := range []string{"0.0.0.0:30141", "[::]:30141", "192.168.1.10:30141", ":30141"} {
		if !requiresWebPassword(address) {
			t.Errorf("requiresWebPassword(%q) = false", address)
		}
	}
	for _, address := range []string{"127.0.0.1:30141", "[::1]:30141", "localhost:30141"} {
		if requiresWebPassword(address) {
			t.Errorf("requiresWebPassword(%q) = true", address)
		}
	}
}
