// Command coinapp-backend keeps the coin catalog fresh and serves the share API.
//
//	coinapp-backend --once        sync now and exit (what cron / the VPS timer runs)
//	coinapp-backend --dry-run     scrape and report, write nothing
//	coinapp-backend               sync now, then every SYNC_INTERVAL_HOURS, and
//	                              serve the share API on LISTEN_ADDR
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"coinapp-backend/internal/config"
	"coinapp-backend/internal/firebase"
	"coinapp-backend/internal/shares"
	"coinapp-backend/internal/sync"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A dry run only needs to read, and the catalog node is world-readable, so the
	// migration can be previewed with no credentials at all.
	var client *firebase.Client
	if cfg.DryRun && !firebase.HasCredentials() {
		log.Println("no Admin credentials found; running read-only")
		client = firebase.NewReadOnly()
	} else {
		client, err = firebase.New(ctx)
		if err != nil {
			log.Fatalf("firebase: %v", err)
		}
	}

	overrides := sync.LoadOverrides(filepath.Join("overrides.json"))
	if len(overrides) > 0 {
		log.Printf("%d manual id override(s) loaded", len(overrides))
	}

	if err := runSync(ctx, client, overrides, cfg); err != nil {
		if cfg.RunOnce {
			log.Fatalf("sync: %v", err)
		}
		log.Printf("sync failed, will retry on the next tick: %v", err)
	}

	if cfg.RunOnce {
		return
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           shares.Handler(client, cfg.AllowOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("share API listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
			return
		case <-ticker.C:
			if err := runSync(ctx, client, overrides, cfg); err != nil {
				log.Printf("sync failed: %v", err)
			}
		}
	}
}

func runSync(ctx context.Context, client *firebase.Client, overrides sync.Overrides, cfg *config.Config) error {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := sync.Run(runCtx, client, overrides, cfg.DryRun)
	return err
}
