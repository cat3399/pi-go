package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/rpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(rpc.RunProduction(ctx, app.ProductionConfig{}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
