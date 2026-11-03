//go:build android

package main

import (
	"errors"
	"path/filepath"

	wails "github.com/wailsapp/wails/v3/pkg/application"
)

func platformDownloadDirectory() (string, error) {
	root := wails.Android.StoragePath()
	if root == "" {
		return "", errors.New("Android storage is unavailable")
	}
	return filepath.Join(root, "downloads"), nil
}
