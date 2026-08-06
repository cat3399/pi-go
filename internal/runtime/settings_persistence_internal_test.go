package agentruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestRuntimeSettingsUndoIsExactAndConditional(t *testing.T) {
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultProvider":"p1","defaultModel":"m1","defaultThinkingLevel":"medium"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := model.NewRuntime(model.Options{AgentDir: agentDir, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	persist := runtimeSettingsPersistence(runtime)
	p2, m2, high := "p2", "m2", provider.ThinkingHigh
	result, err := persist(context.Background(), agent.SettingsUpdate{DefaultProvider: &p2, DefaultModel: &m2, DefaultThinkingLevel: &high})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *model.Settings) error {
		// Repeat the same values to cover the ABA case: field comparison alone
		// cannot distinguish this newer successful write from the transaction.
		settings.DefaultProvider = p2
		settings.DefaultModel = m2
		settings.DefaultThinkingLevel = high
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := result.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := runtime.Snapshot().Settings
	if got.DefaultProvider != p2 || got.DefaultModel != m2 || got.DefaultThinkingLevel != high {
		t.Fatalf("undo overwrote concurrent settings: %#v", got)
	}

	p4, m4, low := "p4", "m4", provider.ThinkingLow
	result, err = persist(context.Background(), agent.SettingsUpdate{DefaultProvider: &p4, DefaultModel: &m4, DefaultThinkingLevel: &low})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = runtime.Snapshot().Settings
	if got.DefaultProvider != p2 || got.DefaultModel != m2 || got.DefaultThinkingLevel != high {
		t.Fatalf("exact undo = %#v", got)
	}
}
