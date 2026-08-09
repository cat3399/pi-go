//go:build pi_go_webui

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/webui"
	webassets "github.com/cat3399/pi-go/web"
)

const version = "0.1.0-dev"

func main() {
	flags := flag.NewFlagSet("pi-go-web", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	listen := flags.String("listen", "127.0.0.1:30141", "HTTP listen address")
	cwd := flags.String("cwd", "", "default working directory")
	agentDir := flags.String("agent-dir", "", "pi agent directory (defaults to PI_CODING_AGENT_DIR or ~/.pi/agent)")
	docsDir := flags.String("docs-dir", "", "pi documentation directory")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web: unexpected positional arguments")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	assets, err := webassets.FS()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web: open embedded assets:", err)
		os.Exit(1)
	}
	surface, err := webui.New(webui.Options{
		Version: version, Context: ctx, Assets: assets,
		Production: app.ProductionConfig{WorkingDir: *cwd, AgentDir: *agentDir, DocsDir: *docsDir},
	})
	if err != nil {
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
		_ = surface.Close(closeCtx)
		os.Exit(1)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := surface.Close(closeCtx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pi-go-web: close:", err)
		os.Exit(1)
	}
}
