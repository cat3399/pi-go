package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/rpc"
)

var version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeUsage(stdout)
		return 0
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "run":
		return app.RunProduction(ctx, app.ProductionConfig{}, args[1:], stdout, stderr)
	case "rpc":
		return rpc.RunProduction(ctx, app.ProductionConfig{}, args[1:], stdin, stdout, stderr)
	case "web":
		return runWeb(ctx, args[1:], stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "pi-go: unknown command %q\n\n", args[0])
		writeUsage(stderr)
		return 2
	}
}

func writeUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, `Usage: pi-go <command> [options]

Commands:
  run      Run the command-line agent
  web      Serve the WebUI and application API
  rpc      Run the JSONL automation adapter
  version  Print the version
  help     Show this help`)
}
