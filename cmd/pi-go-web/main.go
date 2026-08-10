package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	websurface "github.com/cat3399/pi-go/surface/web"
)

const version = "0.1.0-dev"

func main() {
	flags := flag.NewFlagSet("pi-go-web", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	listen := flags.String("listen", "127.0.0.1:30141", "HTTP listen address")
	cwd := flags.String("cwd", "", "default working directory")
	agentDir := flags.String("agent-dir", "", "pi agent directory (defaults to PI_CODING_AGENT_DIR or ~/.pi/agent)")
	docsDir := flags.String("docs-dir", "", "pi documentation directory")
	apiOnly := flags.Bool("api-only", false, "serve only the Go API (for the frontend development server)")
	assetsDir := flags.String("assets-dir", "", "serve browser assets from a directory instead of the embedded production export")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web: unexpected positional arguments")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	assets, err := resolveAssets(*apiOnly, *assetsDir)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web:", err)
		os.Exit(1)
	}
	supervisor, err := application.NewSupervisor(application.SupervisorOptions{
		Context:    ctx,
		Production: app.ProductionConfig{WorkingDir: *cwd, AgentDir: *agentDir, DocsDir: *docsDir},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web:", err)
		os.Exit(1)
	}
	surface, err := websurface.New(websurface.Options{Version: version, Assets: assets, Supervisor: supervisor})
	if err != nil {
		_ = supervisor.Close(context.Background())
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web:", err)
		os.Exit(1)
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
	_, _ = fmt.Fprintf(os.Stderr, "pi-go-web: listening on http://%s\n", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web:", err)
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = supervisor.Close(closeCtx)
		os.Exit(1)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := supervisor.Close(closeCtx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web: close:", err)
		os.Exit(1)
	}
}

func resolveAssets(apiOnly bool, directory string) (fs.FS, error) {
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
	if err != nil {
		if errors.Is(err, websurface.ErrEmbeddedAssetsUnavailable) {
			return nil, errors.New("embedded Web assets are not linked; use --api-only for development, --assets-dir for an existing export, or build with scripts/build-webui.sh")
		}
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	return assets, nil
}
