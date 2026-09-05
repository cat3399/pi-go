package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/model/catalog"
)

// RunModels is the command-line adapter for installed built-in data updates.
func RunModels(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "Usage: pi-go models update [--version <release>] [--agent-dir <path>]")
		return 0
	}
	if args[0] != "update" {
		fmt.Fprintf(stderr, "pi-go models: unknown command %q\n", args[0])
		return 2
	}
	flags := flag.NewFlagSet("pi-go models update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "latest", "upstream release version or latest")
	agentDir := flags.String("agent-dir", "", "agent data directory")
	if err := flags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pi-go models update: unexpected arguments")
		return 2
	}
	paths, err := app.ResolveProductionPaths(app.ProductionConfig{AgentDir: *agentDir})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	target := filepath.Join(paths.AgentDir, catalog.Filename)
	fmt.Fprintln(stdout, "Syncing built-in models from upstream...")
	diff, err := model.SyncBuiltinCatalog(ctx, target, *version)
	if err != nil {
		fmt.Fprintln(stderr, "pi-go models update:", err)
		return 1
	}
	fmt.Fprint(stdout, diff.String())
	if diff.Published {
		fmt.Fprintln(stdout, "Updated", target, "(takes effect on startup or reload)")
	}
	return 0
}
