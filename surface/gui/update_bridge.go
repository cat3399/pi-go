package main

import (
	"context"

	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/internal/selfupdate"
	wails "github.com/wailsapp/wails/v3/pkg/application"
)

// UpdateBridge belongs to this desktop host, independently of its Core connection.
type UpdateBridge struct {
	ctx        context.Context
	controller *selfupdate.Controller
}

func NewUpdateBridge() *UpdateBridge {
	return &UpdateBridge{ctx: context.Background(), controller: selfupdate.NewController(selfupdate.New(product.GUIBuildInfo()))}
}

func (b *UpdateBridge) ServiceStartup(ctx context.Context, _ wails.ServiceOptions) error {
	b.ctx = ctx
	return nil
}

func (b *UpdateBridge) Check(options selfupdate.Options) (selfupdate.CheckedPlan, error) {
	return b.controller.Check(b.ctx, options)
}

func (b *UpdateBridge) Apply(id string) (selfupdate.Result, error) {
	return b.controller.Apply(b.ctx, id)
}

func (b *UpdateBridge) Status() selfupdate.Result { return b.controller.Status() }
