package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"termcall/internal/buildinfo"
	"termcall/internal/logging"
	signaling "termcall/internal/server/websocket"
)

func main() {
	flags := flag.NewFlagSet("termcall-signald", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print version information")
	listenAddress := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: termcall-signald [--listen address] [--log-level level]")
		flags.PrintDefaults()
	}

	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(buildinfo.String("termcall-signald"))
		return
	}

	logger, err := logging.New(os.Stderr, *logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	signalingServer := signaling.New(signaling.DefaultConfig(), logger)
	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           signalingServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()

	logger.Info("signaling service listening", "address", *listenAddress)
	serveErr := httpServer.ListenAndServe()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = signalingServer.Shutdown(shutdownContext)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		logger.Error("signaling service failed", "error", serveErr)
		os.Exit(1)
	}
}
