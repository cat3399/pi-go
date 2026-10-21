//go:build !android

package main

import (
	"errors"
	"github.com/cat3399/pi-go/internal/selfupdate"
)

func newPlatformUpdater() (*selfupdate.Updater, error) {
	return nil, errors.New("mobile updates require the Android host")
}

func openDownloadURL(string) error { return errors.New("APK downloads require the Android host") }
