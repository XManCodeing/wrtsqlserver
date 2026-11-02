// relay-updater is a root host service; it never listens on TCP or runs inside the app container.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opernwrtdocker/backend/internal/onlineupdate"
)

// main loads the root-private config and serves the fixed Unix socket until graceful shutdown.
func main() {
	configPath := flag.String("config", onlineupdate.Root+"/config.json", "root-owned 0600 host configuration")
	flag.Parse()
	logger := log.New(os.Stderr, "relay-updater: ", log.LstdFlags|log.LUTC)
	if err := run(*configPath, logger); err != nil {
		logger.Print("service stopped: ", err)
		os.Exit(1)
	}
}

// run owns the singleton socket lock for the complete lifetime of asynchronous operations.
func run(configPath string, logger *log.Logger) error {
	config, err := onlineupdate.LoadConfig(configPath)
	if err != nil {
		return err
	}
	host := &onlineupdate.DockerHost{Config: config, Runner: onlineupdate.ExecRunner{}}
	preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), time.Minute)
	err = host.Preflight(preflightCtx)
	cancelPreflight()
	if err != nil {
		return err
	}
	listener, cleanup, err := onlineupdate.ListenHost()
	if err != nil {
		return err
	}
	defer cleanup()
	service, err := onlineupdate.NewService(onlineupdate.FileStore{Path: onlineupdate.StatePath}, host, onlineupdate.NewGitHub(), logger)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
		service.Wait()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-signals.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err = server.Shutdown(ctx)
		service.Wait()
		return err
	}
}
