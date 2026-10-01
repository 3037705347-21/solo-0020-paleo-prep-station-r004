package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"example.com/paleo-prep-station/internal/config"
	"example.com/paleo-prep-station/internal/httpapi"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/station"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.LoadServer(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}

	repo := repository.NewMemory()
	service := station.New(repo)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewRouter(service),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Bind first, in the foreground: a port conflict fails here at the
	// LISTEN stage instead of racing a background goroutine.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", cfg.Addr, err)
		return 1
	}
	defer listener.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	log.Printf("paleo prep station listening on %s (%s)", listener.Addr(), cfg.Summary())

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	case <-ctx.Done():
		log.Printf("shutdown signal received, allowing %s for in-flight requests", cfg.ShutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			// Grace period exhausted: force every connection closed so the
			// process cannot exit with requests or listeners lingering.
			fmt.Fprintln(os.Stderr, "shutdown:", err)
			if closeErr := server.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "force close:", closeErr)
			}
			<-serveErr
			return 1
		}
		// Wait for Serve to return so listener and goroutine are released
		// before the process exits.
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "serve:", err)
			return 1
		}
		log.Printf("server stopped cleanly")
		return 0
	}
}
