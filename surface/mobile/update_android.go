//go:build android

package main

import (
	"encoding/json"
	"errors"
	"runtime"

	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/internal/selfupdate"
	wails "github.com/wailsapp/wails/v3/pkg/application"
)

func newPlatformUpdater() (*selfupdate.Updater, error) {
	var info struct {
		Version  string `json:"version"`
		BundleID string `json:"bundleId"`
	}
	if err := json.Unmarshal([]byte(wails.Android.AppInfoJSON()), &info); err != nil {
		return nil, err
	}
	if info.BundleID != "com.cat3399.pigo" || info.Version == "" {
		return nil, errors.New("Android package information is unavailable")
	}
	return selfupdate.New(product.BuildInfo{Version: info.Version, Surface: "mobile", OS: "android", Arch: runtime.GOARCH, Distribution: "apk"}), nil
}

func openDownloadURL(url string) error {
	wails.Android.OpenURL(url)
	return nil
}
