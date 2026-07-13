package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"termcall/internal/buildinfo"
	"termcall/internal/logging"
	"termcall/internal/protocol"
	"termcall/internal/server/access"
	turncredentials "termcall/internal/server/turn"
	signaling "termcall/internal/server/websocket"
)

type stringList []string

func (values *stringList) String() string { return fmt.Sprint([]string(*values)) }
func (values *stringList) Set(value string) error {
	if !protocol.ValidSTUNURL(value) {
		return fmt.Errorf("STUN URL must begin with stun: or stuns: and contain no whitespace")
	}
	*values = append(*values, value)
	return nil
}

type turnURLList []string

func (values *turnURLList) String() string { return fmt.Sprint([]string(*values)) }
func (values *turnURLList) Set(value string) error {
	if !protocol.ValidTURNURL(value) {
		return fmt.Errorf("TURN URL must begin with turn: or turns: and contain no whitespace")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	flags := flag.NewFlagSet("termcall-signald", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print version information")
	defaultListen := "127.0.0.1:8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultListen = "0.0.0.0:" + port
	}
	listenAddress := flags.String("listen", defaultListen, "HTTP listen address")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	accessKeyFile := flags.String("access-key-file", "", "permission-protected file containing the shared access key")
	openMode := flags.Bool("open", false, "allow an unauthenticated non-loopback signaling server")
	tlsCertificate := flags.String("tls-cert", "", "TLS certificate file (TLS may also terminate at a reverse proxy)")
	tlsKey := flags.String("tls-key", "", "TLS private key file")
	turnSecretFile := flags.String("turn-secret-file", "", "file containing the coturn REST shared secret")
	turnTTL := flags.Duration("turn-ttl", 10*time.Minute, "temporary TURN credential lifetime")
	var stunURLs stringList
	var turnURLs turnURLList
	flags.Var(&stunURLs, "stun", "STUN URL advertised to clients; may be repeated")
	flags.Var(&turnURLs, "turn", "TURN URL issued to clients; may be repeated")
	for _, value := range strings.Split(os.Getenv("TERMCALL_STUN_URLS"), ",") {
		if strings.TrimSpace(value) != "" {
			_ = stunURLs.Set(strings.TrimSpace(value))
		}
	}
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
	if (*tlsCertificate == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "--tls-cert and --tls-key must be provided together")
		os.Exit(2)
	}
	if (*turnSecretFile == "") != (len(turnURLs) == 0) {
		fmt.Fprintln(os.Stderr, "--turn-secret-file and at least one --turn URL must be provided together")
		os.Exit(2)
	}
	accessKey, err := loadAccessKey(*accessKeyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(accessKey) == 0 && !isLoopbackListen(*listenAddress) && !*openMode {
		fmt.Fprintln(os.Stderr, "non-loopback listening requires an access key or explicit --open")
		os.Exit(2)
	}
	if len(turnURLs) != 0 && len(accessKey) == 0 {
		fmt.Fprintln(os.Stderr, "TURN credential issuance requires an access key")
		os.Exit(2)
	}

	logger, err := logging.New(os.Stderr, *logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	serverConfig := signaling.DefaultConfig()
	serverConfig.STUNURLs = append([]string(nil), stunURLs...)
	if len(accessKey) != 0 {
		serverConfig.Access, err = access.New(accessKey)
		clear(accessKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *turnSecretFile != "" {
		secretInfo, err := os.Stat(*turnSecretFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "inspect TURN shared secret: %v\n", err)
			os.Exit(2)
		}
		if secretInfo.Mode().Perm()&0o077 != 0 {
			fmt.Fprintln(os.Stderr, "TURN shared secret file must not be accessible by group or other users")
			os.Exit(2)
		}
		secret, err := os.ReadFile(*turnSecretFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read TURN shared secret: %v\n", err)
			os.Exit(2)
		}
		issuer, err := turncredentials.New(turncredentials.Config{
			Secret: bytes.TrimSpace(secret), URLs: turnURLs, STUNURLs: stunURLs, TTL: *turnTTL,
		})
		clear(secret)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure TURN credentials: %v\n", err)
			os.Exit(2)
		}
		serverConfig.TURN = issuer
	}
	signalingServer := signaling.New(serverConfig, logger)
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

	logger.Info("signaling service listening", "address", *listenAddress, "tls", *tlsCertificate != "", "access_control", serverConfig.Access != nil, "open", *openMode)
	var serveErr error
	if *tlsCertificate != "" {
		serveErr = httpServer.ListenAndServeTLS(*tlsCertificate, *tlsKey)
	} else {
		serveErr = httpServer.ListenAndServe()
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = signalingServer.Shutdown(shutdownContext)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		logger.Error("signaling service failed", "error", serveErr)
		os.Exit(1)
	}
}

func loadAccessKey(path string) ([]byte, error) {
	if path == "" {
		return []byte(os.Getenv("TERMCALL_ACCESS_KEY")), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect access key file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("access key file must not be accessible by group or other users")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read access key file: %w", err)
	}
	return bytes.TrimSpace(value), nil
}

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
