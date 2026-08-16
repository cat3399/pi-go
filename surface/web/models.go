package web

import (
	"context"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

type modelListItemWire = surfacewire.ModelListItem
type modelsWire = surfacewire.ModelsView

func models(ctx context.Context, api application.API, cwd string) (modelsWire, error) {
	return surfacewire.Models(ctx, api, cwd)
}
