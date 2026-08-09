package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/host"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
)

type launchOptions struct {
	cwd, sessionPath, providerID, modelID string
	apiKey                                *string
}

func RunProduction(ctx context.Context, config app.ProductionConfig, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdin == nil || stdout == nil || stderr == nil {
		return app.ExitFailure
	}
	options, err := parseLaunchOptions(args)
	if err != nil {
		writeError(stderr, err)
		return app.ExitFailure
	}
	if options.cwd != "" {
		if config.WorkingDir != "" && config.WorkingDir != options.cwd {
			writeError(stderr, errors.New("working directory is configured twice"))
			return app.ExitFailure
		}
		config.WorkingDir = options.cwd
	}
	runtime, err := app.OpenProductionRuntime(ctx, config, app.ProductionRuntimeOptions{
		SessionPath: options.sessionPath, ProviderID: options.providerID,
		ModelID: options.modelID, APIKey: options.apiKey,
	})
	if err != nil {
		writeError(stderr, err)
		return app.ExitFailure
	}
	for _, diagnostic := range runtime.Diagnostics() {
		prefix := ""
		switch diagnostic.Kind {
		case agentruntime.DiagnosticWarning:
			prefix = "Warning: "
		case agentruntime.DiagnosticError:
			prefix = "Error: "
		}
		_, _ = fmt.Fprintln(stderr, prefix+diagnostic.Message)
		if diagnostic.Kind == agentruntime.DiagnosticError {
			_ = runtime.Dispose(context.Background())
			return app.ExitFailure
		}
	}
	agentHost, err := host.New(ctx, runtime)
	if err != nil {
		_ = runtime.Dispose(context.Background())
		writeError(stderr, err)
		return app.ExitFailure
	}
	server, err := NewServer(agentHost, stdin, stdout)
	if err != nil {
		_ = agentHost.Dispose(context.Background())
		writeError(stderr, err)
		return app.ExitFailure
	}
	if err := server.Serve(ctx); err != nil && context.Cause(ctx) == nil {
		writeError(stderr, err)
		return app.ExitFailure
	}
	return app.ExitSuccess
}

func parseLaunchOptions(args []string) (launchOptions, error) {
	var options launchOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		name := args[index]
		if name != "--cwd" && name != "--session" && name != "--provider" && name != "--model" && name != "--api-key" {
			return launchOptions{}, fmt.Errorf("unsupported argument at position %d: %s", index+1, name)
		}
		if seen[name] {
			return launchOptions{}, fmt.Errorf("%s may be specified only once", name)
		}
		seen[name] = true
		if index+1 >= len(args) {
			return launchOptions{}, fmt.Errorf("%s requires a value", name)
		}
		index++
		value := args[index]
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
			return launchOptions{}, fmt.Errorf("%s requires a non-empty UTF-8 value", name)
		}
		switch name {
		case "--cwd":
			options.cwd = value
		case "--session":
			options.sessionPath = value
		case "--provider":
			options.providerID = value
		case "--model":
			options.modelID = value
		case "--api-key":
			copy := value
			options.apiKey = &copy
		}
	}
	return options, nil
}

func writeError(writer io.Writer, err error) {
	_, _ = fmt.Fprintln(writer, "pi-go-rpc: "+err.Error())
}
