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

	"github.com/jacobfg/AudiobooksGreenGarminSidecar/internal/sidecar"
)

func main() {
	if err := run(); err != nil {
		slog.Error("sidecar stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	s, err := sidecar.LoadSettings()
	if err != nil {
		return err
	}
	app, err := sidecar.NewServer(s)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()
	server := &http.Server{Addr: ":8082", Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 1)
	go func() { slog.Info("sidecar listening", "address", server.Addr); result <- server.ListenAndServe() }()
	select {
	case err = <-result:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if e := server.Shutdown(shutdown); e != nil {
		_ = server.Close()
		if err == nil {
			err = e
		}
	}
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
