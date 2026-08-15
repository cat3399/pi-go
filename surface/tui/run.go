package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/application"
	"golang.org/x/term"
)

type ScreenMode string

const (
	ScreenAuto   ScreenMode = "auto"
	ScreenFull   ScreenMode = "full"
	ScreenInline ScreenMode = "inline"
)

func ParseScreenMode(value string) (ScreenMode, error) {
	mode := ScreenMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case "", ScreenAuto:
		return ScreenAuto, nil
	case ScreenFull, ScreenInline:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid TUI screen mode %q", value)
	}
}

type Options struct {
	Application application.API
	SessionID   string
	Version     string
	ScreenMode  ScreenMode
	Theme       Theme

	Input       io.Reader
	Output      io.Writer
	Environment []string
	FPS         int
}

// Run owns the one Bubble Tea Program for a TUI surface. Agent and durable
// session lifecycles remain owned by application.API; Run only keeps a view
// projection and closes its event subscription on exit.
func Run(ctx context.Context, options Options) error {
	if options.Application == nil {
		return errors.New("TUI application API is required")
	}
	options.SessionID = strings.TrimSpace(options.SessionID)
	if options.SessionID == "" {
		return errors.New("TUI session ID is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := ParseScreenMode(string(options.ScreenMode))
	if err != nil {
		return err
	}
	options.ScreenMode = resolveScreenMode(mode, options.Output, options.Environment)

	// Dispatching GetState opens an inactive durable session through the same
	// Application lifecycle used by every other surface.
	if _, active, stateErr := options.Application.LiveState(options.SessionID); stateErr != nil {
		return stateErr
	} else if !active {
		if _, dispatchErr := options.Application.Dispatch(ctx, options.SessionID, application.GetStateCommand{}); dispatchErr != nil {
			return fmt.Errorf("open TUI session: %w", dispatchErr)
		}
	}
	snapshot, err := options.Application.SnapshotSession(options.SessionID, "")
	if err != nil {
		return fmt.Errorf("snapshot TUI session: %w", err)
	}
	model, err := newModel(ctx, options, snapshot)
	if err != nil {
		return err
	}
	defer model.Close()

	programOptions := []tea.ProgramOption{tea.WithContext(ctx)}
	if options.Input != nil {
		programOptions = append(programOptions, tea.WithInput(options.Input))
	}
	if options.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(options.Output))
	}
	if options.Environment != nil {
		programOptions = append(programOptions, tea.WithEnvironment(append([]string(nil), options.Environment...)))
	}
	if options.FPS > 0 {
		programOptions = append(programOptions, tea.WithFPS(options.FPS))
	}
	_, err = tea.NewProgram(model, programOptions...).Run()
	return err
}

type fileDescriptorWriter interface {
	Fd() uintptr
}

func resolveScreenMode(mode ScreenMode, output io.Writer, environment []string) ScreenMode {
	if mode != ScreenAuto {
		return mode
	}
	if environmentValue(environment, "TERM") == "dumb" {
		return ScreenInline
	}
	if output == nil {
		output = os.Stdout
	}
	writer, ok := output.(fileDescriptorWriter)
	if ok && term.IsTerminal(int(writer.Fd())) {
		return ScreenFull
	}
	return ScreenInline
}

func environmentValue(environment []string, name string) string {
	if environment == nil {
		return os.Getenv(name)
	}
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}
