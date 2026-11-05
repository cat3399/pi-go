// Command sync-models refreshes the checked-in built-in catalog from models.dev.
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
	source := flag.String("source", "", "replay a saved models.dev JSON file instead of downloading it")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: make sync-models [ARGS='-source /path/to/models.dev.json']")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	const target = "internal/model/catalogdata/catalog.json"
	fmt.Fprintln(os.Stdout, "Syncing built-in models from models.dev...")
	diff, err := model.SyncBuiltinCatalog(ctx, target, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync-models:", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, diff.String())
	if diff.Published {
		fmt.Fprintln(os.Stdout, "Updated", target)
	}
}
