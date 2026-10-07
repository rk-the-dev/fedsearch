// Command server runs the FedSearch API and the investigation console.
//
//	go run ./cmd/server -config deploy/lite.json     # no Docker needed
//	go run ./cmd/server -config deploy/docker.json   # MinIO + OpenSearch + Postgres
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rksurwase/fedsearch/internal/api"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/service"
	"github.com/rksurwase/fedsearch/internal/web"
)

func main() {
	cfgPath := flag.String("config", "deploy/lite.json", "config file")
	addr := flag.String("addr", "", "listen address (overrides config)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t0 := time.Now()
	svc, err := service.New(ctx, cfg)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	mode := strings.TrimSuffix(filepath.Base(*cfgPath), filepath.Ext(*cfgPath))
	srv := &http.Server{Addr: cfg.Server.Addr, Handler: (&api.Server{Svc: svc, Static: web.FS(), Mode: mode, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second}
	log.Info("fedsearch ready", "addr", cfg.Server.Addr, "mode", mode, "locations", len(svc.Catalog().Locations),
		"llm", svc.NL.Available(), "startup_ms", time.Since(t0).Milliseconds())

	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
