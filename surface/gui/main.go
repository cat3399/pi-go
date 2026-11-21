package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	coreapp "github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/surface/cli"
	wails "github.com/wailsapp/wails/v3/pkg/application"
)

// 前端构建结果只进入独立的 pi-go-gui 二进制，不会被默认 pi-go 命令链接。
//
//go:embed all:frontend/dist
var frontendAssets embed.FS

type launchOptions struct {
	cwd             string
	agentDir        string
	docsDir         string
	defaultRemote   string
	devToolsEnabled bool
}

func desktopFlags(options *launchOptions, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("pi-go-gui", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.cwd, "cwd", "", "default working directory for the embedded pi-go core")
	flags.StringVar(&options.agentDir, "agent-dir", "", "pi-go agent directory")
	flags.StringVar(&options.docsDir, "docs-dir", "", "pi-go documentation directory override")
	flags.StringVar(&options.defaultRemote, "remote", "", "initial remote pi-go endpoint")
	flags.BoolVar(&options.devToolsEnabled, "devtools", false, "enable WebView developer tools")
	return flags
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, openDesktop))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, open func(launchOptions) error) int {
	var options launchOptions
	flags := desktopFlags(&options, stderr)
	if !isDesktopInvocation(args, flags) {
		commands := cli.CommandLine{
			Build: product.GUIBuildInfo(), SourceBundles: guiSourceBundles(),
			ExtraHelp: func(w io.Writer) {
				fmt.Fprintln(w, "Desktop: pi-go-gui [options] (opens a window when no command is given)")
				flags.SetOutput(w)
				flags.PrintDefaults()
			},
		}
		return commands.Run(args, stdin, stdout, stderr)
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "pi-go-gui: unexpected positional argument %q after desktop options\n", flags.Arg(0))
		return 2
	}
	if err := open(options); err != nil {
		fmt.Fprintln(stderr, "pi-go-gui:", err)
		return 1
	}
	return 0
}

// Only a desktop option (not a bare word that happens to match its name) can
// select the window path. All subcommands and their arguments stay with the CLI.
func isDesktopInvocation(args []string, flags *flag.FlagSet) bool {
	if len(args) == 0 || args[0] == "--" {
		return true
	}
	name, _, _ := strings.Cut(args[0], "=")
	return strings.HasPrefix(name, "-") && flags.Lookup(strings.TrimLeft(name, "-")) != nil
}

func openDesktop(options launchOptions) error {
	bridge := NewGUIBridge(coreapp.ProductionConfig{
		AttachSession: cli.AttachSession,
		WorkingDir:    options.cwd,
		AgentDir:      options.agentDir,
		DocsDir:       options.docsDir,
		SourceBundles: guiSourceBundles(),
	}, options.defaultRemote, product.Version)

	application := wails.New(wails.Options{
		Name:        product.Name,
		Description: "pi-go desktop agent",
		Services: []wails.Service{
			wails.NewService(bridge),
			wails.NewService(NewUpdateBridge()),
		},
		Assets: wails.AssetOptions{
			Handler: wails.AssetFileServerFS(frontendAssets),
		},
		Mac: wails.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	application.Window.NewWithOptions(wails.WebviewWindowOptions{
		Title:            product.Name,
		Width:            1253,
		Height:           760,
		DevToolsEnabled:  options.devToolsEnabled,
		BackgroundColour: wails.NewRGB(255, 255, 255),
		URL:              "/",
		Mac: wails.MacWindow{
			// HiddenInset adds an empty NSToolbar, which gives the window larger corners on macOS Tahoe.
			TitleBar: wails.MacTitleBarHidden,
		},
	})

	return application.Run()
}
