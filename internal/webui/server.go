// Package webui implements the optional HTTP surface above pi-go's
// transport-neutral Application Host. It must never own Agent product state.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/app"
)

type Options struct {
	Version     string
	Assets      fs.FS
	Context     context.Context
	Production  app.ProductionConfig
	IdleTimeout time.Duration

	// Test seams remain package-private so product callers always use the real
	// production assembler and idle lifecycle.
	openRuntime   runtimeOpener
	disableReaper bool
}

type Server struct {
	handler    http.Handler
	supervisor *Supervisor
}

func New(options Options) (*Server, error) {
	assets := options.Assets
	if assets == nil {
		return nil, errors.New("WebUI assets are required")
	}
	supervisor, err := newSupervisor(supervisorOptions{
		Context: options.Context, Production: options.Production, IdleTimeout: options.IdleTimeout,
		OpenRuntime: options.openRuntime, DisableReaper: options.disableReaper,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": options.Version})
	})
	mux.HandleFunc("GET /api/capabilities", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"capabilities": Capabilities()})
	})
	registerAPIRoutes(mux, supervisor)
	mux.HandleFunc("/api/", unsupportedAPI)
	mux.Handle("/", staticHandler(assets))
	return &Server{handler: mux, supervisor: supervisor}, nil
}

func (s *Server) Handler() http.Handler {
	if s == nil || s.handler == nil {
		return http.NotFoundHandler()
	}
	return s.handler
}

func (s *Server) Close(ctx context.Context) error {
	if s == nil || s.supervisor == nil {
		return nil
	}
	return s.supervisor.Close(ctx)
}

func unsupportedAPI(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusNotImplemented, map[string]any{
		"error":       "This pi-go WebUI capability is not implemented",
		"code":        "capability_not_implemented",
		"method":      request.Method,
		"requestPath": request.URL.Path,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func staticHandler(assets fs.FS) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+request.URL.Path), "/")
		if clean == "." || clean == "" {
			clean = "index.html"
		}
		if info, err := fs.Stat(assets, clean); err == nil && !info.IsDir() {
			setAssetCacheHeaders(writer, clean)
			files.ServeHTTP(writer, request)
			return
		}
		// Client-side navigation owns non-API routes. Serve the real exported
		// application shell; API misses are handled above and never become HTML.
		clone := request.Clone(request.Context())
		clone.URL.Path = "/"
		setAssetCacheHeaders(writer, "index.html")
		files.ServeHTTP(writer, clone)
	})
}

func setAssetCacheHeaders(writer http.ResponseWriter, name string) {
	switch {
	case name == "index.html", name == "sw.js", name == "manifest.webmanifest":
		writer.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	case strings.HasPrefix(name, "_next/static/"):
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		writer.Header().Set("Cache-Control", "public, max-age=3600")
	}
}
