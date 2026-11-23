package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOperationsNeverCreateAnImplicitTerminal(t *testing.T) {
	resolved := false
	s := New(Options{Resolve: func() (Defaults, error) {
		resolved = true
		return Defaults{}, errors.New("unexpected terminal startup")
	}})
	ctx := context.Background()
	for _, action := range []Action{Run, Read, Write, Interrupt, Close, Resize} {
		for _, id := range []string{"", " \t\n"} {
			request := Request{Action: action, ID: id, Command: "echo unexpected", Input: "unexpected\n", Cols: 80, Rows: 24}
			if _, err := s.Do(ctx, request, Agent); err == nil || !strings.Contains(err.Error(), "id is required") {
				t.Errorf("Agent %s with ID %q = %v, want missing ID error", action, id, err)
			}
			if _, err := s.View(ctx, request); err == nil || !strings.Contains(err.Error(), "id is required") {
				t.Errorf("surface %s with ID %q = %v, want missing ID error", action, id, err)
			}
		}
	}
	if _, err := s.Do(ctx, Request{Action: Run, ID: "missing", Command: "echo unexpected"}, Agent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID = %v, want ErrNotFound", err)
	}
	listed, err := s.Do(ctx, Request{Action: List}, Agent)
	if err != nil || resolved || len(listed.Terminals) != 0 {
		t.Fatalf("invalid operations started a terminal: resolved=%t, terminals=%v, error=%v", resolved, listed.Terminals, err)
	}
}

func TestBuildEnvironmentStripsStaleSessionVariablesAndAppliesOverlay(t *testing.T) {
	base := []string{
		"KEEP=value",
		"PI_EXECUTABLE=stale-bin",
		"PI_SESSION_ID=stale-id",
		"PI_SESSION_FILE=stale-file",
		"PI_SESSION_ENDPOINT=stale-endpoint",
		"PI_SESSION_TOKEN=stale-token",
		"PI_SESSION_AGENT_DIR=stale-dir",
		"PI_PROVIDER=stale-provider",
		"PI_MODEL=stale-model",
		"PI_REASONING_LEVEL=stale-reasoning",
		"OVERRIDE=old",
	}
	toMap := func(entries []string) map[string]string {
		out := make(map[string]string, len(entries))
		for _, entry := range entries {
			key, value, _ := strings.Cut(entry, "=")
			out[key] = value
		}
		return out
	}

	userEnv := toMap(buildEnvironment(base, nil))
	if userEnv["KEEP"] != "value" || userEnv["OVERRIDE"] != "old" {
		t.Fatalf("base entries missing: %v", userEnv)
	}
	for name := range strippedSessionEnvironment {
		if _, exists := userEnv[name]; exists {
			t.Fatalf("stale session variable %s leaked into user terminal environment", name)
		}
	}

	agentEnv := toMap(buildEnvironment(base, map[string]string{
		"PI_EXECUTABLE":        "/bin/pi-go",
		"PI_SESSION_ID":        "sess-1",
		"PI_SESSION_AGENT_DIR": "/tmp/agent",
		"OVERRIDE":             "new",
	}))
	if agentEnv["PI_EXECUTABLE"] != "/bin/pi-go" || agentEnv["PI_SESSION_ID"] != "sess-1" || agentEnv["PI_SESSION_AGENT_DIR"] != "/tmp/agent" || agentEnv["OVERRIDE"] != "new" {
		t.Fatalf("overlay not applied cleanly: %v", agentEnv)
	}
	for _, omitted := range []string{"PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL", "PI_SESSION_ENDPOINT", "PI_SESSION_TOKEN"} {
		if _, exists := agentEnv[omitted]; exists {
			t.Fatalf("unexpected variable %s present in agent terminal environment", omitted)
		}
	}
}

func TestValidateRejectsInvalidEnvironment(t *testing.T) {
	s := New(Options{})
	for _, env := range []map[string]string{
		{"": "value"},
		{"BAD=KEY": "value"},
		{"BAD\x00KEY": "value"},
		{"KEY": "bad\x00value"},
	} {
		if _, err := s.Do(context.Background(), Request{Action: Open, Environment: env}, Agent); err == nil {
			t.Fatalf("expected validation error for environment %v", env)
		}
	}
}
