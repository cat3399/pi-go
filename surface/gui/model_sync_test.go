package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/model/catalog"
)

type fixtureCatalogHTTP struct{}

func (fixtureCatalogHTTP) Do(request *http.Request) (*http.Response, error) {
	if request.URL.String() != catalog.ModelsDevURL {
		return nil, fmt.Errorf("unexpected catalog URL: %s", request.URL)
	}
	file, err := os.Open("../../internal/model/testdata/catalog-sync.json")
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: file}, nil
}

func TestDesktopSyncBuiltinModelsUsesEmbeddedCore(t *testing.T) {
	logs := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, logs)
	}
	config := app.ProductionConfig{AgentDir: t.TempDir(), WorkingDir: t.TempDir(), Environment: []string{}}
	api, err := application.NewService(application.ServiceOptions{Production: config, DisableReaper: true, ModelHTTP: fixtureCatalogHTTP{}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close(context.Background())
	bridge := NewGUIBridge(config, "", "test")
	bridge.local = api
	result, err := bridge.SyncBuiltinModels()
	if err != nil || !result.Published || result.Summary == "" {
		t.Fatalf("first IPC sync = %+v, %v", result, err)
	}
	if _, err := catalog.Read(filepath.Join(config.AgentDir, catalog.Filename)); err != nil {
		t.Fatal(err)
	}
	result, err = bridge.SyncBuiltinModels()
	if err != nil || result.Published || result.FullOutputPath != "" {
		t.Fatalf("second IPC sync = %+v, %v", result, err)
	}
}
