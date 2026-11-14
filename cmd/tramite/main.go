// Command tramite serves realtime transit data prepared from GTFS feeds.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // timezones without the OS database (distroless image)

	"git.marcorealacci.me/marcorealacci/tramite/internal/api"
	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/service"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("TRAMITE_CONFIG", "config.yaml"), "config file")
	clockStart := flag.String("clock", "", "start the clock at this RFC 3339 time (replaying archived feeds)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := service.New(cfg, log)
	if *clockStart != "" {
		start, err := time.Parse(time.RFC3339, *clockStart)
		if err != nil {
			log.Error("-clock", "err", err)
			os.Exit(1)
		}
		offset := time.Until(start)
		for _, s := range svc.Sources {
			s.Now = func() time.Time { return time.Now().Add(offset) }
		}
		log.Warn("clock shifted for replay", "start", start)
	}
	go svc.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&api.API{Svc: svc, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams stay open (each write has its own deadline).
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("tramite listening", "addr", cfg.Listen, "version", version, "sources", len(cfg.Sources))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
