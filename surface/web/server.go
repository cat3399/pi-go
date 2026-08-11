// Package web implements the browser, HTTP, and SSE surface above pi-go's
// transport-neutral application services. It owns presentation and transport
// state only, never Agent or durable Session product state.
package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

type Options struct {
	Version      string
	Assets       fs.FS
	Application  application.API
	AllowedHosts []string
}

type Server struct {
	handler http.Handler
}

func New(options Options) (*Server, error) {
	if options.Application == nil {
		return nil, errors.New("Web surface application API is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": options.Version})
	})
	mux.HandleFunc("GET /api/v1/capabilities", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"capabilities": Capabilities()})
	})
	registerAPIRoutes(mux, options.Application)
	mux.HandleFunc("/api/v1/", unsupportedAPI)
	mux.HandleFunc("/api/", unsupportedAPI)
	if options.Assets == nil {
		mux.Handle("/", http.NotFoundHandler())
	} else {
		mux.Handle("/", staticHandler(options.Assets))
	}
	return &Server{handler: protectAPIRequests(mux, options.AllowedHosts)}, nil
}

func (s *Server) Handler() http.Handler {
	if s == nil || s.handler == nil {
		return http.NotFoundHandler()
	}
	return s.handler
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

func protectAPIRequests(next http.Handler, allowedHosts []string) http.Handler {
	configured := make(map[string]struct{}, len(allowedHosts))
	for _, value := range allowedHosts {
		if hostname, ok := hostnameFromAuthority(value); ok {
			configured[hostname] = struct{}{}
		}
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			hostname, allowed := hostnameFromAuthority(request.Host)
			if allowed {
				_, configuredHost := configured[hostname]
				allowed = hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || net.ParseIP(hostname) != nil || configuredHost
			}
			if !allowed || strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")), "cross-site") {
				writeJSON(writer, http.StatusForbidden, map[string]any{"error": "Untrusted API request"})
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func hostnameFromAuthority(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \\/@\r\n\t") {
		return "", false
	}
	parsed, err := url.Parse("http://" + value)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return hostname, hostname != ""
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
