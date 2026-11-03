//go:build !android

package main

import "errors"

func platformDownloadDirectory() (string, error) {
	return "", errors.New("mobile downloads require the Android host")
}
