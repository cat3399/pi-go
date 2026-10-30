package application

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestServiceForkKeepsRunningSourceAndRegistersIndependentBranch(t *testing.T) {
	for _, position := range []agent.ForkPosition{agent.ForkAt, agent.ForkBefore} {
		t.Run(string(position), func(t *testing.T) {
			service := newFileTestService(t, t.TempDir())
			cwd := service.DefaultCWD()
			dir, err := session.SessionDirForAgentDir(cwd, service.AgentDir())
			if err != nil {
				t.Fatal(err)
			}
			manager, err := session.CreateSessionManager(cwd, dir, session.NewSessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			first := appendCatalogMessage(t, manager, "first", time.Now(), false)
			answer := appendCatalogMessage(t, manager, "saved answer", time.Now(), true)
			managed := addCatalogManagedSession(t, service, manager)
			source := managed.session.Runtime().Session()
			implementation := managed.session.Runtime().Services().Provider.(*provider.ScriptedProvider)
			message, _ := answer.Message()
			terminal, ok := message.(llm.AssistantTerminal)
			if !ok {
				t.Fatal("fixture must contain an assistant terminal")
			}
			step, err := provider.FixedResponseStep(terminal)
			if err != nil {
				t.Fatal(err)
			}
			if err := implementation.SetResponses([]provider.ScriptStep{step, step}); err != nil {
				t.Fatal(err)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			var once, unblock sync.Once
			defer unblock.Do(func() { close(release) })
			unsubscribe := source.Subscribe(func(ctx context.Context, event agent.SessionEvent) {
				if _, ok := event.(agent.MessageUpdateEvent); !ok {
					return
				}
				once.Do(func() {
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
			})
			defer unsubscribe()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id := manager.SessionID()
			if _, err := service.Dispatch(ctx, id, PromptCommand{Message: "still running"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("source did not start")
			}
			selected := answer.ID()
			if position == agent.ForkBefore {
				selected = first.ID()
			}
			value, err := service.Dispatch(ctx, id, ForkCommand{EntryID: selected, Position: position})
			if err != nil {
				t.Fatal(err)
			}
			fork := value.(ForkResult)
			if fork.Cancelled || fork.SessionID == nil || *fork.SessionID == id {
				t.Fatalf("fork = %#v", fork)
			}
			current, active, err := service.LiveState(id)
			if err != nil || !active || !current.IsPromptRunning || source != managed.session.Runtime().Session() {
				t.Fatalf("source was replaced/stopped: %#v, %v, %v", current, active, err)
			}
			branch, ok := service.active(*fork.SessionID)
			if !ok || branch == managed || branch.manager() == manager {
				t.Fatal("branch did not get an independent owner")
			}
			wantCount := 2
			if position == agent.ForkBefore {
				wantCount = 0
			}
			if got := branch.manager().Summary().MessageCount; got != wantCount {
				t.Fatalf("fork history count = %d", got)
			}
			if _, err := service.Dispatch(ctx, *fork.SessionID, PromptCommand{Message: "branch question"}); err != nil {
				t.Fatal(err)
			}
			if err := branch.session.Runtime().Session().WaitForIdle(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Dispatch(ctx, *fork.SessionID, AbortCommand{}); err != nil {
				t.Fatal(err)
			}
			if state, _, _ := service.LiveState(id); !state.IsPromptRunning {
				t.Fatal("branch operation stopped source")
			}
			unblock.Do(func() { close(release) })
			if err := source.WaitForIdle(ctx); err != nil {
				t.Fatal(err)
			}
			if got := manager.Summary().MessageCount; got != 4 {
				t.Fatalf("source history count = %d", got)
			}
			if got := branch.manager().Summary().MessageCount; got != wantCount+2 {
				t.Fatalf("branch history count = %d", got)
			}
			values, err := service.ListSessions()
			if err != nil || len(values) != 2 {
				t.Fatalf("catalog = %#v, %v", values, err)
			}
			for _, info := range values {
				if info.ID == *fork.SessionID && info.ParentSessionID != id {
					t.Fatalf("branch parent = %q", info.ParentSessionID)
				}
			}
		})
	}
}
