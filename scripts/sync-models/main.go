// Command sync-models refreshes the checked-in built-in catalog from upstream.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cat3399/pi-go/internal/model"
)

func main() {
	version := flag.String("version", "latest", "upstream release version or latest")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: make sync-models ARGS='-version <release>'")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	const target = "internal/model/catalogdata/catalog.json"
	fmt.Fprintln(os.Stdout, "Syncing built-in models from upstream...")
	diff, err := model.SyncBuiltinCatalog(ctx, target, *version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync-models:", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, diff.String())
	if diff.Published {
		fmt.Fprintln(os.Stdout, "Updated", target)
	}
}
