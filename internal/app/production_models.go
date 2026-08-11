package app

import (
	"context"
	"fmt"
	"os"

	modelcatalog "github.com/cat3399/pi-go/internal/model"
)

// OpenProductionModelRuntime builds the same provider, authentication, and
// adapter graph used by production AgentSession construction without creating
// a session. Long-lived application surfaces use it for model discovery and
// configuration views so they never invent a credential-blind catalog beside
// the Agent runtime.
func OpenProductionModelRuntime(
	ctx context.Context,
	config ProductionConfig,
	workingDir string,
	projectTrusted bool,
) (*modelcatalog.Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	config.WorkingDir = workingDir
	paths, err := ResolveProductionPaths(config)
	if err != nil {
		return nil, err
	}
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	}
	adapters, err := newProductionProviderAdapters(config)
	if err != nil {
		return nil, err
	}
	authResolver, err := newProductionProviderAuthResolver(paths.AgentDir, environmentMap(environment), config)
	if err != nil {
		return nil, err
	}
	runtime, err := modelcatalog.NewRuntime(modelcatalog.Options{
		AgentDir: paths.AgentDir, WorkingDir: paths.WorkingDir, ProjectTrusted: projectTrusted,
		Adapters: adapters, AuthResolver: authResolver,
	})
	if err != nil {
		return nil, fmt.Errorf("open production model runtime: %w", err)
	}
	return runtime, nil
}
