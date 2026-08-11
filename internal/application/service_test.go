package application

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
)

func TestServiceOwnsSurfaceNeutralPathsAndLifecycle(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	service, err := NewService(ServiceOptions{
		Context: context.Background(),
		Production: app.ProductionConfig{
			WorkingDir: cwd,
			AgentDir:   agentDir,
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.DefaultCWD() != cwd || service.AgentDir() != agentDir {
		t.Fatalf("paths = %q, %q", service.DefaultCWD(), service.AgentDir())
	}
	values, err := service.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("sessions = %#v", values)
	}
	found, err := service.SessionExists("not-present")
	if err != nil || found {
		t.Fatalf("SessionExists = %v, %v", found, err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestServiceRejectsIncompleteModelSelectionBeforeOpeningRuntime(t *testing.T) {
	cwd := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent")},
		OpenRuntime: func(context.Context, app.ProductionConfig, app.ProductionRuntimeOptions) (*agentruntime.Runtime, error) {
			t.Fatal("runtime opener must not be called")
			return nil, nil
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if _, err := service.NewSession(context.Background(), NewSessionOptions{CWD: cwd, Provider: "only-provider"}); err == nil {
		t.Fatal("incomplete model selection was accepted")
	}
}
