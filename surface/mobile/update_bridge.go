package main

import (
	"context"

	"github.com/cat3399/pi-go/internal/selfupdate"
	wails "github.com/wailsapp/wails/v3/pkg/application"
)

type MobileHostInfo struct {
	Version     string `json:"version"`
	UpdateError string `json:"updateError,omitempty"`
}

// UpdateBridge selects this client's APK without linking or calling Agent Core.
type UpdateBridge struct {
	ctx        context.Context
	controller *selfupdate.Controller
	host       MobileHostInfo
	err        error
}

func (b *UpdateBridge) ServiceStartup(ctx context.Context, _ wails.ServiceOptions) error {
	b.ctx = ctx
	updater, err := newPlatformUpdater()
	b.err = err
	b.host.Version = version
	if err != nil {
		b.host.UpdateError = err.Error()
	} else {
		b.host.Version = updater.Build.Version
		b.controller = selfupdate.NewController(updater)
	}
	return nil
}

func (b *UpdateBridge) HostInfo() MobileHostInfo { return b.host }

func (b *UpdateBridge) Check() (selfupdate.CheckedPlan, error) {
	if b.err != nil {
		return selfupdate.CheckedPlan{}, b.err
	}
	return b.controller.Check(b.ctx, selfupdate.Options{})
}

func (b *UpdateBridge) Download(id string) error {
	if b.err != nil {
		return b.err
	}
	url, err := b.controller.DownloadURL(id)
	if err != nil {
		return err
	}
	return openDownloadURL(url)
}
