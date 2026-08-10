package application

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
)

func TestSupervisorOwnsSurfaceNeutralPathsAndLifecycle(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	supervisor, err := NewSupervisor(SupervisorOptions{
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
	if supervisor.DefaultCWD() != cwd || supervisor.AgentDir() != agentDir {
		t.Fatalf("paths = %q, %q", supervisor.DefaultCWD(), supervisor.AgentDir())
	}
	values, err := supervisor.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("sessions = %#v", values)
	}
	found, err := supervisor.SessionExists("not-present")
	if err != nil || found {
		t.Fatalf("SessionExists = %v, %v", found, err)
	}
	if err := supervisor.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSupervisorRejectsIncompleteModelSelectionBeforeOpeningRuntime(t *testing.T) {
	cwd := t.TempDir()
	supervisor, err := NewSupervisor(SupervisorOptions{
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
	t.Cleanup(func() { _ = supervisor.Close(context.Background()) })
	if _, err := supervisor.NewSession(context.Background(), NewSessionOptions{CWD: cwd, Provider: "only-provider"}); err == nil {
		t.Fatal("incomplete model selection was accepted")
	}
}
