package main

import (
	"os"

	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/surface/cli"
)

func main() {
	commands := cli.CommandLine{Build: product.CLIBuildInfo()}
	os.Exit(commands.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
