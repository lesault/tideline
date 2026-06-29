// Command tideline is a self-hosted read-later triage funnel: links decay in an
// inbox unless you act on them, and the keepers get pushed to Wallabag.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lesault/tideline/internal/auth"
	"github.com/lesault/tideline/internal/fetch"
	"github.com/lesault/tideline/internal/server"
	"github.com/lesault/tideline/internal/store"
	"github.com/lesault/tideline/internal/wallabag"
)

func main() {
	cfg := loadConfig()

	st, err := store.Open(cfg.dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	sessions := auth.NewSessionManager(cfg.sessionTTL)
	fetcher := fetch.New(cfg.fetchTimeout)
	wb := wallabag.New(cfg.fetchTimeout)
	srv := server.New(st, sessions, fetcher, wb)
	srv.SetOpenRegistration(cfg.openRegistration)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go srv.RunSweeper(ctx, cfg.sweepInterval)

	httpSrv := &http.Server{Addr: cfg.addr, Handler: srv.Handler()}
	go func() {
		log.Printf("tideline listening on %s (db=%s)", cfg.addr, cfg.dbPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
}

type config struct {
	addr             string
	dbPath           string
	sessionTTL       time.Duration
	fetchTimeout     time.Duration
	sweepInterval    time.Duration
	openRegistration bool
}

func loadConfig() config {
	return config{
		addr:             env("TIDELINE_ADDR", ":8080"),
		dbPath:           env("TIDELINE_DB", "tideline.db"),
		sessionTTL:       envDuration("TIDELINE_SESSION_TTL", 30*24*time.Hour),
		fetchTimeout:     envDuration("TIDELINE_FETCH_TIMEOUT", 15*time.Second),
		sweepInterval:    envDuration("TIDELINE_SWEEP_INTERVAL", time.Hour),
		openRegistration: envBool("TIDELINE_OPEN_REGISTRATION", true),
	}
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("warning: invalid %s, using default %v", key, def)
		return def
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("warning: invalid %s=%q, using default %s", key, v, def)
	}
	return def
}
