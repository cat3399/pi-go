package main

import (
	"embed"
	"fmt"
	"log"

	wails "github.com/wailsapp/wails/v3/pkg/application"
)

// The mobile Workbench is embedded only in the independently built mobile
// application. It is never linked into the default pi-go binary.
//
//go:embed all:frontend/dist
var frontendAssets embed.FS

var version = "0.1.0-dev"

func main() {
	bridge := NewRemoteBridge()
	application := wails.New(wails.Options{
		Name:        "pi",
		Description: "pi-go remote mobile client",
		Services: []wails.Service{
			wails.NewService(bridge),
		},
		Assets: wails.AssetOptions{
			Handler: wails.AssetFileServerFS(frontendAssets),
		},
	})

	application.Window.NewWithOptions(wails.WebviewWindowOptions{
		Title:            "pi",
		Width:            430,
		Height:           860,
		BackgroundColour: wails.NewRGB(255, 255, 255),
		URL:              "/",
	})

	if err := application.Run(); err != nil {
		log.Fatal(fmt.Errorf("run pi-go mobile: %w", err))
	}
}
