package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arkpix/relay/internal/app"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/config"
	"github.com/arkpix/relay/internal/db"
)

func main() {
	cfg := config.Load()

	logger := slog.New(common.NewRedactHandler(slog.NewJSONHandler(os.Stdout, nil)))
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("create data dir failed", "err", err)
		os.Exit(1)
	}
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = database.Close() }()
	if err := db.Migrate(context.Background(), database); err != nil {
		slog.Error("migrate database failed", "err", err)
		os.Exit(1)
	}

	handler, err := app.New(cfg, database)
	if err != nil {
		slog.Error("build app failed", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("relay server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
