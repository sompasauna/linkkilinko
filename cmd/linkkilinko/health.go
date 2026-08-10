package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func startHealthServer(ctx context.Context, app *application, listenAddress string) func() {
	if strings.TrimSpace(listenAddress) == "" {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(response http.ResponseWriter, request *http.Request) {
		if err := app.state.Ping(request.Context()); err != nil {
			http.Error(response, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := app.client.Ping(request.Context()); err != nil {
			http.Error(response, "telegram unavailable", http.StatusServiceUnavailable)
			return
		}
		backlog, err := app.state.OutboxBacklog(request.Context())
		if err != nil {
			http.Error(response, "outbox unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(response, `{"status":"ready","outbox_backlog":%d,"updates_seen":%d,"last_update_unix":%d}`+"\n", backlog, app.updatesSeen.Load(), app.lastUpdateUnix.Load())
	})
	server := &http.Server{Addr: listenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server stopped", "error", err)
		}
	}()
	stop := func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop
}
