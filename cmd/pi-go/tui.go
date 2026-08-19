package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
	tuisurface "github.com/cat3399/pi-go/surface/tui"
)

func runTUI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pi-go tui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cwd := flags.String("cwd", "", "working directory for a new session")
	agentDir := flags.String("agent-dir", "", "pi agent directory (defaults to PI_CODING_AGENT_DIR or ~/.pi/agent)")
	docsDir := flags.String("docs-dir", "", "pi documentation directory")
	sessionID := flags.String("session", "", "open an existing session ID")
	modelRef := flags.String("model", "", "initial model as provider/model-id")
	thinking := flags.String("thinking", "", "initial thinking level")
	screen := flags.String("screen", string(tuisurface.ScreenFull), "screen mode: full, inline, or auto")
	prompt := flags.String("prompt", "", "send an initial prompt after opening the TUI")
	fps := flags.Int("fps", 60, "maximum terminal render rate (1-120)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	initialPrompt := strings.TrimSpace(*prompt)
	if positional := strings.TrimSpace(strings.Join(flags.Args(), " ")); positional != "" {
		if initialPrompt != "" {
			initialPrompt += " "
		}
		initialPrompt += positional
	}
	mode, err := tuisurface.ParseScreenMode(*screen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go tui:", err)
		return 2
	}
	if *fps < 1 || *fps > 120 {
		_, _ = fmt.Fprintln(stderr, "pi-go tui: --fps must be between 1 and 120")
		return 2
	}
	providerID, modelID, err := parseTUIModel(*modelRef)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go tui:", err)
		return 2
	}
	var thinkingLevel *provider.ThinkingLevel
	if strings.TrimSpace(*thinking) != "" {
		level := provider.ThinkingLevel(strings.TrimSpace(*thinking))
		if !level.Valid() {
			_, _ = fmt.Fprintf(stderr, "pi-go tui: invalid thinking level %q\n", *thinking)
			return 2
		}
		thinkingLevel = &level
	}

	service, err := application.NewService(application.ServiceOptions{
		Context: ctx,
		Production: app.ProductionConfig{
			WorkingDir: *cwd,
			AgentDir:   *agentDir,
			DocsDir:    *docsDir,
		},
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go tui:", err)
		return 1
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if closeErr := service.Close(closeContext); closeErr != nil {
			_, _ = fmt.Fprintln(stderr, "pi-go tui: close:", closeErr)
		}
	}()

	selectedSession := strings.TrimSpace(*sessionID)
	if selectedSession == "" {
		state, createErr := service.NewSession(ctx, application.NewSessionOptions{
			CWD: service.DefaultCWD(), Provider: providerID, ModelID: modelID,
			ThinkingLevel: thinkingLevel,
		})
		if createErr != nil {
			_, _ = fmt.Fprintln(stderr, "pi-go tui:", createErr)
			return 1
		}
		selectedSession = state.SessionID
	} else {
		if providerID != "" {
			if _, dispatchErr := service.Dispatch(ctx, selectedSession, application.SetModelCommand{Provider: providerID, ModelID: modelID}); dispatchErr != nil {
				_, _ = fmt.Fprintln(stderr, "pi-go tui:", dispatchErr)
				return 1
			}
		}
		if thinkingLevel != nil {
			if _, dispatchErr := service.Dispatch(ctx, selectedSession, application.SetThinkingLevelCommand{Level: *thinkingLevel}); dispatchErr != nil {
				_, _ = fmt.Fprintln(stderr, "pi-go tui:", dispatchErr)
				return 1
			}
		}
	}

	if err := tuisurface.Run(ctx, tuisurface.Options{
		Application: service, SessionID: selectedSession, Version: version,
		ScreenMode: mode, InitialPrompt: initialPrompt, Input: stdin, Output: stdout, FPS: *fps,
	}); err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(stderr, "pi-go tui:", err)
		return 1
	}
	return 0
}

func parseTUIModel(value string) (providerID, modelID string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", nil
	}
	providerID, modelID, found := strings.Cut(value, "/")
	providerID, modelID = strings.TrimSpace(providerID), strings.TrimSpace(modelID)
	if !found || providerID == "" || modelID == "" {
		return "", "", errors.New("--model must be provider/model-id")
	}
	return providerID, modelID, nil
}
