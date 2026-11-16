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
	runtimeRollbackWatchdog := flag.Duration("runtime-rollback-watchdog", 0, "Wait for the expected service health then commit or roll back a pending runtime activation")
	flag.Parse()
	var watchdogNameErr error
	if *runtimeRollbackWatchdog > 0 {
		watchdogNameErr = updater.MarkRuntimeRollbackWatchdogProcess()
	}

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
	if *runtimeRollbackWatchdog > 0 {
		if watchdogNameErr != nil {
			rollbackErr := updater.RollbackRuntimeActivation(cfg.DataDir)
			log.Fatalf("[reader] watchdog process rename failed (%v), runtime rollback error: %v", watchdogNameErr, rollbackErr)
		}
		if err := updater.RunRuntimeActivationWatchdog(cfg.DataDir, cfg.Listen, updater.CurrentVersion, *runtimeRollbackWatchdog); err != nil {
			rollbackErr := updater.RollbackRuntimeActivation(cfg.DataDir)
			log.Fatalf("[reader] runtime rollback watchdog failed (%v), rollback error: %v", err, rollbackErr)
		}
		return
	}

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
	if err := updater.EnsureEmbeddedRuntime(cfg.DataDir, cfg.TmpDir(), updater.CurrentVersion); err != nil {
		log.Fatalf("[reader] install embedded runtime package: %v", err)
	}
	if updater.RuntimeActivationPending(cfg.DataDir) {
		if err := updater.SpawnRuntimeActivationWatchdog(cfg, updater.RuntimeActivationRollbackGracePeriod); err != nil {
			log.Fatalf("[reader] cannot protect pending runtime activation: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobRunner := jobs.NewRunner(conn, cfg)
	go jobRunner.Start(ctx)

	srv := api.NewServer(cfg, conn, webFS)
	handler := srv.Routes()
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
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
	var loopbackServer *http.Server
	if loopbackAddr, ok := updater.LoopbackHealthAddress(cfg.Listen); ok {
		loopbackServer = &http.Server{
			Addr:              loopbackAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			log.Printf("[reader] compatibility health listener on loopback %s", loopbackAddr)
			if err := loopbackServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("[reader] loopback health listener unavailable: %v", err)
			}
		}()
	}
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if updater.HealthCheckHasVersion(cfg.Listen, updater.CurrentVersion) {
				if err := updater.CommitRuntimeActivation(cfg.DataDir); err != nil {
					log.Printf("[reader] could not commit runtime rollback marker: %v", err)
				}
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	<-ctx.Done()
	log.Println("[reader] shutting down gracefully...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if loopbackServer != nil {
		_ = loopbackServer.Shutdown(shutdownCtx)
	}
}
