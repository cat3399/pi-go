package main

import (
	"context"
	"os"

	"github.com/cat3399/pi-go/internal/app"
)

func main() {
	exitCode := app.RunProduction(
		context.Background(),
		app.ProductionConfig{},
		os.Args[1:],
		os.Stdout,
		os.Stderr,
	)
	os.Exit(exitCode)
}
