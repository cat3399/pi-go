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
	return CommandLine{}.runModels(ctx, args, stdout, stderr)
}

func (c CommandLine) runModels(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	name := commandName(c.Build) + " models"
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(stdout, "Usage: %s update [--source <models.dev JSON file>] [--agent-dir <path>]\n", name)
		return 0
	}
	if args[0] != "update" {
		fmt.Fprintf(stderr, "%s: unknown command %q\n", name, args[0])
		return 2
	}
	flags := flag.NewFlagSet(name+" update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "replay a saved models.dev JSON file instead of downloading it")
	agentDir := flags.String("agent-dir", "", "agent data directory")
	if err := flags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, name+" update: unexpected arguments")
		return 2
	}
	paths, err := app.ResolveProductionPaths(app.ProductionConfig{AgentDir: *agentDir})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	target := filepath.Join(paths.AgentDir, catalog.Filename)
	fmt.Fprintln(stdout, "Syncing built-in models from models.dev...")
	diff, err := model.SyncBuiltinCatalog(ctx, target, *source)
	if err != nil {
		fmt.Fprintln(stderr, name+" update:", err)
		return 1
	}
	fmt.Fprint(stdout, diff.String())
	if diff.Published {
		fmt.Fprintln(stdout, "Updated", target, "(takes effect on startup or reload)")
	}
	return 0
}
