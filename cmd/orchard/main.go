// Command orchard is a self-hosted Apple Music download server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orchard/internal/api"
	"orchard/internal/apple"
	"orchard/internal/catalog"
	"orchard/internal/config"
	"orchard/internal/download"
	"orchard/internal/playactivity"
	"orchard/internal/store"
	"orchard/internal/stream"
	"orchard/internal/webplayback"
	"orchard/internal/wrapper"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "orchard:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.Stderr)
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	prov := wrapper.NewProvisioner(wrapper.Options{
		Dir:    cfg.WrapperDir,
		Tag:    cfg.WrapperTag,
		URL:    cfg.WrapperURL,
		SHA256: cfg.WrapperSHA256,
	})
	appleMgr := apple.NewManager(prov, wrapper.Config{
		Host: cfg.WrapperHost,
		Ports: wrapper.Ports{
			Decrypt: cfg.DecryptPort,
			M3U8:    cfg.M3U8Port,
			Account: cfg.AccountPort,
			Key:     cfg.KeyPort,
		},
		DeviceInfo: cfg.WrapperDeviceInfo,
		Proxy:      cfg.WrapperProxy,
	})

	// Provisioning downloads ~50 MB, so it runs in the background: the API comes
	// up immediately and reports progress through /v1/apple/status.
	go func() {
		var err error
		if cfg.WrapperAutoProvision {
			err = prov.Ensure(ctx)
		} else {
			err = prov.Detect()
		}
		if err != nil {
			slog.Error("wrapper unavailable", "error", err)
			return
		}
		if err := appleMgr.Autostart(ctx); err != nil {
			slog.Error("wrapper autostart failed", "error", err)
		}
	}()

	streamClient := stream.New(stream.Config{
		Host:        cfg.WrapperHost,
		M3U8Port:    cfg.M3U8Port,
		DecryptPort: cfg.DecryptPort,
	})
	catalogClient := catalog.New(appleMgr.CatalogTokens, catalog.Config{
		RateLimit: cfg.AppleRateLimit,
		RateBurst: cfg.AppleRateBurst,
	})

	// A crash leaves jobs mid-flight; mark them failed rather than showing work
	// that will never progress.
	if err := st.ResumeInterrupted(ctx); err != nil {
		slog.Warn("could not reset interrupted jobs", "error", err)
	}

	// Reporting a play is a write to Apple on the user's behalf, and it is not
	// on playback's critical path — pace it with the same budget as catalog
	// reads rather than letting a chatty client hammer Apple.
	playActivityClient := playactivity.New(appleMgr.PlayActivityTokens, playactivity.Config{
		RateLimit: cfg.AppleRateLimit,
		RateBurst: cfg.AppleRateBurst,
	})

	wpClient := webplayback.New()
	downloads := download.New(st, streamClient, catalogClient, appleMgr.CatalogTokens, wpClient, download.Config{
		LibraryDir: cfg.LibraryDir,
	})
	go downloads.Run(ctx)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(cfg, st, appleMgr, catalogClient, streamClient, appleMgr.CatalogTokens, wpClient,
			playActivityClient, downloads),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("orchard listening",
			"addr", cfg.Addr,
			"data_dir", cfg.DataDir,
			"apple_state", appleMgr.Status().State,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	srvErr := srv.Shutdown(shutdownCtx)
	// Never leave an authenticated wrapper listening after we exit.
	if err := appleMgr.Shutdown(shutdownCtx); err != nil {
		slog.Error("stopping wrapper failed", "error", err)
	}
	return srvErr
}
