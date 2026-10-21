package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/internal/selfupdate"
)

// RunUpdate is the CLI adapter; it never opens an application or Agent session.
func RunUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunUpdateForBuild(ctx, args, stdout, stderr, product.CLIBuildInfo())
}

// RunUpdateForBuild is also used by the independently packaged GUI command.
func RunUpdateForBuild(ctx context.Context, args []string, stdout, stderr io.Writer, build product.BuildInfo) int {
	name := "pi-go"
	if build.Surface == "gui" {
		name = "pi-go-gui"
	}
	return runUpdateCommand(ctx, args, stdout, stderr, name, func() updateService {
		return selfupdate.New(build)
	})
}

type updateService interface {
	Check(context.Context, selfupdate.Options) (selfupdate.Plan, error)
	Apply(context.Context, selfupdate.Plan) (selfupdate.Result, error)
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer, newUpdater func() updateService) int {
	return runUpdateCommand(ctx, args, stdout, stderr, "pi-go", newUpdater)
}

func runUpdateCommand(ctx context.Context, args []string, stdout, stderr io.Writer, name string, newUpdater func() updateService) int {
	flags := flag.NewFlagSet(name+" update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "check the current surface's latest release without installing")
	version := flags.String("version", "", "install a specific stable version (allows downgrade)")
	force := flags.Bool("force", false, "reinstall even if the version is unchanged")
	_ = flags.Bool("self", false, "update pi-go itself (the default)")
	asJSON := flags.Bool("json", false, "write the result as JSON")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s update [--check] [--version <version>] [--force] [--json]\n", name)
		fmt.Fprintln(flags.Output(), "\nUpdate this installation's surface and platform from official GitHub releases.")
		flags.PrintDefaults()
	}
	if len(args) == 1 && args[0] == "help" {
		args = []string{"--help"}
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, name+" update: unexpected arguments")
		return 2
	}
	options := selfupdate.Options{Version: *version, Force: *force}
	if err := selfupdate.ValidateOptions(options); err != nil {
		fmt.Fprintln(stderr, name+" update:", err)
		return 2
	}
	updater := newUpdater()
	fmt.Fprintln(stderr, "Checking matching pi-go releases...")
	plan, err := updater.Check(ctx, options)
	if err != nil {
		fmt.Fprintln(stderr, name+" update:", err)
		return 1
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "Surface: %s (%s/%s)\nCurrent: %s\nTarget:  %s\nPath:    %s\n", plan.Surface, plan.OS, plan.Arch, plan.CurrentVersion, plan.TargetVersion, plan.Executable)
		if plan.Reason != "" && *check {
			fmt.Fprintln(stdout, plan.Reason)
		}
	}
	if *check {
		if *asJSON {
			return writeUpdateJSON(stdout, stderr, plan)
		}
		if plan.Available {
			fmt.Fprintf(stdout, "Available action: %s\n", plan.Action)
		} else if plan.CanUpdate {
			fmt.Fprintln(stdout, "Already at or ahead of the target release.")
		} else {
			fmt.Fprintln(stdout, "Automatic update unavailable.")
		}
		return 0
	}
	if !plan.CanUpdate {
		fmt.Fprintln(stderr, name+" update:", plan.Reason)
		return 1
	}
	if plan.Available {
		fmt.Fprintf(stderr, "Downloading and verifying %s...\n", plan.Asset)
	}
	result, err := updater.Apply(ctx, plan)
	if err != nil {
		fmt.Fprintln(stderr, name+" update:", err)
		return 1
	}
	if *asJSON {
		return writeUpdateJSON(stdout, stderr, result)
	}
	if result.Updated {
		fmt.Fprintf(stdout, "Updated %s from %s to %s.\nPrevious executable: %s\nRestart %s to use the new version.\n", plan.Surface, plan.CurrentVersion, plan.TargetVersion, result.Backup, name)
	} else {
		fmt.Fprintln(stdout, "Already at or ahead of the target release.")
	}
	return 0
}

func writeUpdateJSON(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		fmt.Fprintln(stderr, "pi-go update:", err)
		return 1
	}
	return 0
}
