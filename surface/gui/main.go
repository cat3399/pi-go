package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	coreapp "github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/installation"
	"github.com/cat3399/pi-go/internal/product"
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

func parseLaunchOptions(args []string) (launchOptions, error) {
	flags := flag.NewFlagSet("pi-go-gui", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var options launchOptions
	flags.StringVar(&options.cwd, "cwd", "", "default working directory for the embedded pi-go core")
	flags.StringVar(&options.agentDir, "agent-dir", "", "pi-go agent directory")
	flags.StringVar(&options.docsDir, "docs-dir", "", "pi-go documentation directory override")
	flags.StringVar(&options.defaultRemote, "remote", "", "initial remote pi-go endpoint")
	flags.BoolVar(&options.devToolsEnabled, "devtools", false, "enable WebView developer tools")
	if err := flags.Parse(args); err != nil {
		return launchOptions{}, err
	}
	if flags.NArg() != 0 {
		return launchOptions{}, errors.New("unexpected positional arguments")
	}
	return options, nil
}

func main() {
	options, err := parseLaunchOptions(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}

	bridge := NewGUIBridge(coreapp.ProductionConfig{
		WorkingDir:    options.cwd,
		AgentDir:      options.agentDir,
		DocsDir:       options.docsDir,
		SourceBundles: []installation.SourceBundle{{Prefix: "surface/gui", Files: guiSources}},
	}, options.defaultRemote, product.Version)

	application := wails.New(wails.Options{
		Name:        product.Name,
		Description: "pi-go desktop agent",
		Services: []wails.Service{
			wails.NewService(bridge),
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
			TitleBar: wails.MacTitleBarHiddenInset,
		},
	})

	if err := application.Run(); err != nil {
		log.Fatal(fmt.Errorf("run pi-go GUI: %w", err))
	}
}
