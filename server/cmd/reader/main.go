package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"reader/server/internal/api"
	"reader/server/internal/config"
	"reader/server/internal/db"
	"reader/server/internal/jobs"
	"reader/server/internal/updater"
	"reader/server/internal/web"
)

func main() {
	cfgPath := flag.String("config", "/mnt/sda1/reader/config.json", "Path to config.json")
	selfTest := flag.Bool("self-test", false, "Run pre-flight binary, database migration, and embedded asset verification then exit")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(updater.CurrentVersion)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("[reader] failed to load config: %v", err)
	}
	cfg.ConfigPath = *cfgPath

	if err := cfg.VerifyDiskMount(); err != nil {
		log.Fatalf("[reader] disk mount verification failed: %v", err)
	}

	// Redirect TMPDIR to external disk so uploads/SQLite never touch router RAM /tmp
	_ = os.Setenv("TMPDIR", cfg.TmpDir())

	conn, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("[reader] database initialization failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	webFS, err := web.DistFS()
	if err != nil {
		log.Printf("[reader] warning: embedded web dist unavailable: %v", err)
	}

	if *selfTest {
		fmt.Printf("SELF_TEST_OK %s\n", updater.CurrentVersion)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobRunner := jobs.NewRunner(conn, cfg)
	go jobRunner.Start(ctx)

	srv := api.NewServer(cfg, conn, webFS)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("[reader] %s listening on http://%s (data_dir=%s)", updater.CurrentVersion, cfg.Listen, cfg.DataDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[reader] http server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[reader] shutting down gracefully...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
