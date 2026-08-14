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
)

func main() {
	cfg := config.Load()

	logger := slog.New(common.NewRedactHandler(slog.NewJSONHandler(os.Stdout, nil)))
	slog.SetDefault(logger)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.New(),
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
