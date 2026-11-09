package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestSessionCLIControlsBorrowedOwnerAndRejectsOtherSessions(t *testing.T) {
	ctx := context.Background()
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{ID: "live-cli-test"})
	if err != nil {
		t.Fatal(err)
	}
	scripted, _ := provider.NewScriptedProvider(provider.ScriptedConfig{})
	current, err := agent.NewSession(agent.SessionConfig{Provider: scripted, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close(ctx)
	server := httptest.NewServer(sessionControlHandler(application.BorrowSessionContext(current), "fixture-token"))
	defer server.Close()
	t.Setenv("PI_SESSION_ID", manager.SessionID())
	t.Setenv("PI_SESSION_ENDPOINT", server.URL+"/session")
	t.Setenv("PI_SESSION_TOKEN", "fixture-token")
	invoke := func(input string, args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := RunSession(ctx, args, strings.NewReader(input), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	output, stderr, code := invoke("continue in module A\n", "--json", "context", "new", "--handoff-stdin", "--request-id", "retry-one")
	if code != 0 {
		t.Fatalf("new: %d %s", code, stderr)
	}
	var change agent.ContextChangeResult
	if err := json.Unmarshal([]byte(output), &change); err != nil || change.State != "completed" {
		t.Fatalf("change %s %v", output, err)
	}
	count := len(manager.Entries())
	_, stderr, code = invoke("continue in module A\n", "context", "new", "--handoff-stdin", "--request-id", "retry-one")
	if code != 0 || len(manager.Entries()) != count {
		t.Fatalf("retry: %d %s", code, stderr)
	}
	_, stderr, code = invoke("different", "context", "new", "--handoff-stdin", "--request-id", "retry-one")
	if code == 0 || len(manager.Entries()) != count {
		t.Fatal("conflicting request accepted", stderr)
	}
	for _, credentials := range []struct{ id, token string }{{manager.SessionID(), "bad-token"}, {"another-session", "fixture-token"}} {
		request := sessionControlRequest{SessionID: credentials.id, Action: "change", Change: agent.ContextChangeRequest{Mode: "handoff", Handoff: "unauthorized"}}
		if _, err := callSessionControl(ctx, server.URL+"/session", credentials.token, request); err == nil {
			t.Fatal("unauthorized command succeeded")
		}
	}
	if len(manager.Entries()) != count {
		t.Fatal("rejected control changed transcript")
	}
	data, _ := json.Marshal(sessionControlRequest{SessionID: manager.SessionID(), Action: "status"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/session", bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer fixture-token")
	request.Header.Set("Origin", "https://unrelated.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("browser origin accepted")
	}
	output, stderr, code = invoke("", "--json", "context", "status")
	if code != 0 || !strings.Contains(output, change.WindowID) {
		t.Fatal("status did not reflect owner", stderr)
	}
}

func TestSessionCLIHelpDoesNotRequireASelectedSession(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "")
	t.Setenv("PI_SESSION_ENDPOINT", "")
	for _, args := range [][]string{{"--help"}, {"history", "--help"}, {"history", "read", "--help"}, {"context", "--help"}, {"context", "new", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := RunSession(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr.String())
		}
	}
}
