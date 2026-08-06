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
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultProvider":"p1","defaultModel":"m1","defaultThinkingLevel":"medium","steeringMode":"one-at-a-time","followUpMode":"all","compaction":{"enabled":true},"retry":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := model.NewRuntime(model.Options{AgentDir: agentDir, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	persist := runtimeSettingsPersistence(runtime)
	p2, m2, high := "p2", "m2", provider.ThinkingHigh
	steering2, followUp2 := agent.QueueAll, agent.QueueOneAtATime
	compaction2, retry2 := false, true
	result, err := persist(context.Background(), agent.SettingsUpdate{
		DefaultProvider: &p2, DefaultModel: &m2, DefaultThinkingLevel: &high,
		SteeringMode: &steering2, FollowUpMode: &followUp2,
		AutoCompactionEnabled: &compaction2, AutoRetryEnabled: &retry2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *model.Settings) error {
		// Repeat the same values to cover the ABA case: field comparison alone
		// cannot distinguish this newer successful write from the transaction.
		settings.DefaultProvider = p2
		settings.DefaultModel = m2
		settings.DefaultThinkingLevel = high
		settings.SteeringMode = steering2.String()
		settings.FollowUpMode = followUp2.String()
		settings.Compaction.Enabled = &compaction2
		settings.Retry.Enabled = &retry2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := result.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := runtime.Snapshot().Settings
	if got.DefaultProvider != p2 || got.DefaultModel != m2 || got.DefaultThinkingLevel != high ||
		got.SteeringMode != steering2.String() || got.FollowUpMode != followUp2.String() ||
		got.Compaction.Enabled == nil || *got.Compaction.Enabled != compaction2 ||
		got.Retry.Enabled == nil || *got.Retry.Enabled != retry2 {
		t.Fatalf("undo overwrote concurrent settings: %#v", got)
	}

	p4, m4, low := "p4", "m4", provider.ThinkingLow
	steering4, followUp4 := agent.QueueOneAtATime, agent.QueueAll
	compaction4, retry4 := true, false
	result, err = persist(context.Background(), agent.SettingsUpdate{
		DefaultProvider: &p4, DefaultModel: &m4, DefaultThinkingLevel: &low,
		SteeringMode: &steering4, FollowUpMode: &followUp4,
		AutoCompactionEnabled: &compaction4, AutoRetryEnabled: &retry4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = runtime.Snapshot().Settings
	if got.DefaultProvider != p2 || got.DefaultModel != m2 || got.DefaultThinkingLevel != high ||
		got.SteeringMode != steering2.String() || got.FollowUpMode != followUp2.String() ||
		got.Compaction.Enabled == nil || *got.Compaction.Enabled != compaction2 ||
		got.Retry.Enabled == nil || *got.Retry.Enabled != retry2 {
		t.Fatalf("exact undo = %#v", got)
	}
}
