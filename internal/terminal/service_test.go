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
