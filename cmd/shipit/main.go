// Command shipit runs the Shipit control plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Iyed-M/Shipit/internal/api"
	"github.com/Iyed-M/Shipit/internal/caddy"
	"github.com/Iyed-M/Shipit/internal/config"
	"github.com/Iyed-M/Shipit/internal/deploy"
	"github.com/Iyed-M/Shipit/internal/docker"
	"github.com/Iyed-M/Shipit/internal/github"
	"github.com/Iyed-M/Shipit/internal/store"
)

const (
	caddyWait       = 30 * time.Second
	shutdownTimeout = 10 * time.Second
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("shipit stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "builds"), 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "shipit.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	router := caddy.NewRouter(caddy.Config{
		AdminSocket:       cfg.CaddyAdminSocket,
		Domain:            cfg.Domain,
		DashboardUpstream: cfg.CaddyUpstream,
		StaticHost:        cfg.StaticHost,
		StaticRoot:        cfg.StaticRoot,
	}, db)
	gh := github.NewClient(cfg.GitHubToken, cfg.GitHubAPIURL, cfg.GitHubWebURL)
	pipeline := deploy.NewPipeline(deploy.Config{
		DataDir:     cfg.DataDir,
		AppsNetwork: cfg.AppsNetwork,
		Domain:      cfg.Domain,
	}, db, docker.CLI{}, gh, router, log)

	if err := pipeline.Recover(ctx); err != nil {
		return err
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, caddyWait)
	defer cancelWait()
	if err := router.WaitReady(waitCtx); err != nil {
		return err
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		pipeline.Run(ctx)
	}()

	server := &http.Server{
		Addr: cfg.Listen,
		Handler: api.New(api.Config{
			AdminToken: cfg.AdminToken,
			PublicURL:  cfg.PublicURL(),
			Domain:     cfg.Domain,
		}, db, gh, router, pipeline, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("shipit listening", "addr", cfg.Listen, "domain", cfg.Domain)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		stop()
		<-workerDone
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = server.Shutdown(shutdownCtx)
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
