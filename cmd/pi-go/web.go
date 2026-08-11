package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	websurface "github.com/cat3399/pi-go/surface/web"
)

func runWeb(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("pi-go web", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:30141", "HTTP listen address")
	cwd := flags.String("cwd", "", "default working directory")
	agentDir := flags.String("agent-dir", "", "pi agent directory (defaults to PI_CODING_AGENT_DIR or ~/.pi/agent)")
	docsDir := flags.String("docs-dir", "", "pi documentation directory")
	apiOnly := flags.Bool("api-only", false, "serve only the Go API (for the frontend development server)")
	assetsDir := flags.String("assets-dir", "", "serve browser assets from a directory instead of the embedded production export")
	var extraAllowedHosts []string
	flags.Func("allowed-host", "additional HTTP Host allowed to access /api (repeatable)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("allowed host must be non-empty")
		}
		extraAllowedHosts = append(extraAllowedHosts, value)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "pi-go web: unexpected positional arguments")
		return 2
	}

	assets, err := resolveWebAssets(*apiOnly, *assetsDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go web:", err)
		return 1
	}
	service, err := application.NewService(application.ServiceOptions{
		Context: ctx,
		Production: app.ProductionConfig{
			WorkingDir: *cwd,
			AgentDir:   *agentDir,
			DocsDir:    *docsDir,
		},
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go web:", err)
		return 1
	}
	allowedHost := *listen
	if host, _, splitErr := net.SplitHostPort(*listen); splitErr == nil {
		allowedHost = host
	}
	allowedHosts := append([]string{allowedHost}, extraAllowedHosts...)
	surface, err := websurface.New(websurface.Options{
		Version: version, Assets: assets, Application: service, AllowedHosts: allowedHosts,
	})
	if err != nil {
		_ = service.Close(context.Background())
		_, _ = fmt.Fprintln(stderr, "pi-go web:", err)
		return 1
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           surface.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	_, _ = fmt.Fprintf(stderr, "pi-go web: listening on http://%s\n", *listen)
	serveErr := server.ListenAndServe()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(stderr, "pi-go web:", serveErr)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	closeErr := service.Close(closeCtx)
	if closeErr != nil {
		_, _ = fmt.Fprintln(stderr, "pi-go web: close:", closeErr)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) || closeErr != nil {
		return 1
	}
	return 0
}

func resolveWebAssets(apiOnly bool, directory string) (fs.FS, error) {
	if apiOnly && directory != "" {
		return nil, errors.New("--api-only and --assets-dir cannot be used together")
	}
	if apiOnly {
		return nil, nil
	}
	if directory != "" {
		resolved, err := filepath.Abs(directory)
		if err != nil {
			return nil, fmt.Errorf("resolve assets directory: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("open assets directory: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("assets path is not a directory: %s", resolved)
		}
		return os.DirFS(resolved), nil
	}
	assets, err := websurface.EmbeddedAssets()
	if err == nil {
		return assets, nil
	}
	if errors.Is(err, websurface.ErrEmbeddedAssetsUnavailable) {
		return nil, errors.New("embedded Web assets are not linked; use --api-only for development, --assets-dir for an existing export, or build with scripts/build-webui.sh")
	}
	return nil, fmt.Errorf("open embedded assets: %w", err)
}
