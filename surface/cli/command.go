package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/installation"
	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/internal/rpc"
)

// CommandLine runs the same in-process commands in both executable hosts.
type CommandLine struct {
	Build         product.BuildInfo
	SourceBundles []installation.SourceBundle
	ExtraHelp     func(io.Writer)
}

func (c CommandLine) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		c.writeUsage(stdout)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "help", "-h", "--help":
		c.writeUsage(stdout)
		return 0
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, c.Build.Version)
		return 0
	case "update":
		return RunUpdateForBuild(ctx, args[1:], stdout, stderr, c.Build)
	case "run":
		return app.RunProduction(ctx, c.productionConfig(), args[1:], stdout, stderr)
	case "models":
		return c.runModels(ctx, args[1:], stdout, stderr)
	case "session":
		return c.runSession(ctx, args[1:], stdin, stdout, stderr)
	case "tui":
		return c.runTUI(ctx, args[1:], stdin, stdout, stderr)
	case "rpc":
		return rpc.RunProduction(ctx, c.productionConfig(), args[1:], stdin, stdout, stderr)
	case "web":
		return c.runWeb(ctx, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n\n", commandName(c.Build), args[0])
		c.writeUsage(stderr)
		return 2
	}
}

func (c CommandLine) productionConfig() app.ProductionConfig {
	return app.ProductionConfig{AttachSession: AttachSession, SourceBundles: c.SourceBundles}
}

func commandName(build product.BuildInfo) string {
	if build.Surface == "gui" {
		return "pi-go-gui"
	}
	return "pi-go"
}

func (c CommandLine) writeUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %s <command> [options]

Commands:
  run      Run the command-line agent
  tui      Run the interactive terminal surface
  web      Serve the WebUI and application API
  rpc      Run the JSONL automation adapter
  session  Read history and manage the current context
  models   Update installed built-in model data
  update   Update this installation's surface
  version  Print the version
  help     Show this help
`, commandName(c.Build))
	if c.ExtraHelp != nil {
		fmt.Fprintln(w)
		c.ExtraHelp(w)
	}
}
