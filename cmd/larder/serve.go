package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/alpinekind"
	"github.com/mmrzaf/larder/internal/kinds/aptkind"
	"github.com/mmrzaf/larder/internal/kinds/cratekind"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/kinds/gokind"
	"github.com/mmrzaf/larder/internal/kinds/mavenkind"
	"github.com/mmrzaf/larder/internal/kinds/npmkind"
	"github.com/mmrzaf/larder/internal/kinds/pypikind"
	"github.com/mmrzaf/larder/internal/kinds/rustupkind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/server"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/internal/version"
)

func runServe(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "override the public listener address")
	adminListen := fs.String("admin-listen", "", "override the admin listener address")
	offline := fs.Bool("offline", false, "start in offline mode")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "serve does not accept arguments")
		return exitUsage
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *adminListen != "" {
		cfg.Admin.Listen = *adminListen
	}
	if *offline {
		cfg.Offline = true
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "open data directory: %v\n", err)
		return 4
	}
	idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		_ = st.Close()
		fmt.Fprintf(stderr, "open index: %v\n", err)
		return 4
	}
	defer func() { _ = st.Close() }()
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := idx.QuickCheck(ctx); err != nil {
		cancel()
		fmt.Fprintf(stderr, "index integrity check failed: %v\n", err)
		fmt.Fprintln(stderr, "restore the latest file from snapshots/ or run larder reindex --rebuild-blobs-only")
		return 4
	}
	cancel()

	registry := kinds.NewRegistry()
	if err := registry.Register("generic", generickind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("go", gokind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("maven", mavenkind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("npm", npmkind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("pypi", pypikind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("crates", cratekind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("rustup", rustupkind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("apt", aptkind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := registry.Register("alpine", alpinekind.New); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := metrics.New(version.Get().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var offlineOverride *bool
	if *offline {
		value := true
		offlineOverride = &value
	}
	srv, err := server.New(server.Options{
		Config: cfg, ConfigPath: configPath, Store: st, Index: idx,
		Kinds: registry, Metrics: m,
		ListenOverride: *listen, AdminListenOverride: *adminListen, OfflineOverride: offlineOverride,
	})
	if err != nil {
		fmt.Fprintf(stderr, "start server: %v\n", err)
		return 1
	}
	if err := srv.Start(); err != nil {
		_ = srv.Shutdown(context.Background())
		fmt.Fprintf(stderr, "start listeners: %v\n", err)
		return 1
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	for {
		select {
		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				if err := srv.Reload(); err != nil {
					fmt.Fprintf(stderr, "reload configuration: %v\n", err)
				}
			default:
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace.Duration())
				err := srv.Shutdown(shutdownCtx)
				shutdownCancel()
				if err != nil && !errors.Is(err, context.Canceled) {
					fmt.Fprintf(stderr, "shutdown: %v\n", err)
					return 1
				}
				return exitOK
			}
		case err := <-srv.Errors():
			fmt.Fprintf(stderr, "listener failed: %v\n", err)
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace.Duration())
			_ = srv.Shutdown(shutdownCtx)
			shutdownCancel()
			return 1
		}
	}
}

func runHealthcheck(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "readiness URL")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "healthcheck does not accept arguments")
		return exitUsage
	}
	url := *rawURL
	if url == "" {
		cfg, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if cfg.Admin.Listen == "" {
			fmt.Fprintln(stderr, "admin listener is disabled")
			return exitFailure
		}
		url = "http://" + cfg.Admin.Listen + "/readyz"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return exitFailure
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		return exitOK
	}
	fmt.Fprintf(stderr, "healthcheck: status %d\n", response.StatusCode)
	return exitFailure
}
