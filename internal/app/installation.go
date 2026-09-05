package app

import (
	"context"
	"fmt"
	"os"

	"github.com/cat3399/pi-go/internal/installation"
	"github.com/cat3399/pi-go/internal/product"
)

// PrepareProduction initializes process-owned filesystem resources before any
// service reads settings, discovers sessions or publishes a project snapshot.
// ResolveProductionPaths remains the separate, read-only path query.
func PrepareProduction(ctx context.Context, config ProductionConfig) (ProductionPaths, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := ResolveProductionPaths(config)
	if err != nil {
		return ProductionPaths{}, err
	}
	importLegacy := config.AgentDir == "" && product.EnvironmentValue(config.Environment, product.AgentDirectoryEnvironment) == ""
	if err := installation.InitializeAgent(ctx, paths.AgentDir, paths.WorkingDir, config.Environment, importLegacy); err != nil {
		return ProductionPaths{}, fmt.Errorf("initialize agent directory: %w", err)
	}
	if err := installation.InitializeProject(ctx, paths.WorkingDir); err != nil {
		return ProductionPaths{}, fmt.Errorf("initialize project directory: %w", err)
	}
	return paths, nil
}

func productionDocumentation(ctx context.Context, config ProductionConfig, paths ProductionPaths) (product.Documentation, error) {
	docsDirectory := ""
	if config.DocsDir != "" {
		directory, err := product.ResolvePath(config.DocsDir, paths.WorkingDir)
		if err != nil {
			return product.Documentation{}, fmt.Errorf("resolve documentation directory: %w", err)
		}
		info, err := os.Stat(directory)
		if err != nil {
			return product.Documentation{}, fmt.Errorf("open documentation directory: %w", err)
		}
		if !info.IsDir() {
			return product.Documentation{}, fmt.Errorf("documentation path is not a directory: %s", directory)
		}
		docsDirectory = directory
	}
	documentation, err := installation.InstallKnowledge(ctx, paths.AgentDir, config.SourceBundles)
	if err != nil {
		return product.Documentation{}, fmt.Errorf("install product documentation: %w", err)
	}
	if docsDirectory != "" {
		documentation.DocsPath = docsDirectory
	}
	return documentation, nil
}
