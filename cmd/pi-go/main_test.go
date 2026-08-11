package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUnifiedCommandShowsAvailableSurfaces(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, command := range []string{"run", "web", "rpc"} {
		if !strings.Contains(stdout.String(), "  "+command) {
			t.Fatalf("usage does not contain %q: %s", command, stdout.String())
		}
	}
}

func TestUnifiedCommandRejectsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"unknown"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "unknown"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWebHelpDoesNotRequireEmbeddedAssets(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"web", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}
