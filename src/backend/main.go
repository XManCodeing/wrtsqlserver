package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// environment returns a nonempty environment value or the supplied nonsecret fallback.
func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// adminPassword reads a bounded secret file preferentially; no default credential exists.
func adminPassword() (string, error) {
	password := ""
	if path := os.Getenv("ADMIN_PASSWORD_FILE"); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", errors.New("cannot open ADMIN_PASSWORD_FILE")
		}
		defer file.Close()
		password, err = readPasswordFile(file)
		if err != nil {
			return "", err
		}
	} else {
		password = os.Getenv("ADMIN_PASSWORD")
	}
	if len(password) < 12 || len(password) > 4096 {
		return "", errors.New("administrator password must be configured and contain 12–4096 bytes")
	}
	return password, nil
}

// readPasswordFile consumes a bounded reader completely despite short reads.
// It returns the secret without one trailing line ending, or a safe read/size error.
func readPasswordFile(reader io.Reader) (string, error) {
	buffer, err := io.ReadAll(io.LimitReader(reader, 4099))
	if err != nil {
		return "", errors.New("cannot read ADMIN_PASSWORD_FILE")
	}
	if len(buffer) > 4098 {
		return "", errors.New("administrator password is too long")
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(buffer), "\n"), "\r"), nil
}

// run initializes secure storage, binds management first, then serves until shutdown or error.
func run() error {
	password, err := adminPassword()
	if err != nil {
		return err
	}
	address := environment("LISTEN_ADDR", "127.0.0.1:8080")
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return errors.New("LISTEN_ADDR must contain an explicit host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("LISTEN_ADDR port must be 1–65535")
	}
	maxConnections, err := strconv.Atoi(environment("MAX_CONNECTIONS", "1024"))
	if err != nil || maxConnections < 1 || maxConnections > 65536 {
		return errors.New("MAX_CONNECTIONS must be 1–65536")
	}
	store, err := newStore(environment("APP_DATA_DIR", "./data"))
	if err != nil {
		return fmt.Errorf("storage initialization: %w", err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("management listener: %w", err)
	}
	defer listener.Close()
	app, err := newApp(store, password, address, environment("WEB_DIR", "./web"), maxConnections)
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	failures := make(chan error, 1)
	go func() { failures <- server.Serve(listener) }()
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("management server started on %s", address)
	select {
	case err := <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-signals.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return err
		}
	}
	return nil
}

// main logs safe startup/shutdown failures and exits unsuccessfully on fatal configuration errors.
func main() {
	if err := run(); err != nil {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}
