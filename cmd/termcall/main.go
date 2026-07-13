package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"termcall/internal/buildinfo"
	"termcall/internal/client/app"
)

const defaultServerURL = "ws://127.0.0.1:8080/v1/ws"

type stringList []string

func (values *stringList) String() string { return fmt.Sprint([]string(*values)) }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(buildinfo.String("termcall"))
		return
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "listen":
		err = runListen(ctx, os.Args[2:])
	case "chat":
		err = runChat(ctx, os.Args[2:])
	case "help", "--help", "-h":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "termcall: %v\n", err)
		os.Exit(1)
	}
}

func runListen(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("termcall listen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	username := flags.String("username", "", "ephemeral local username (required)")
	serverURL := flags.String("server", defaultServerURL, "signaling WebSocket URL")
	var stunURLs stringList
	flags.Var(&stunURLs, "stun", "STUN URL; may be repeated")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *username == "" {
		flags.Usage()
		return errorsUsage("listen requires --username")
	}
	return app.RunListen(ctx, app.Config{
		Username: *username, ServerURL: *serverURL, STUNURLs: stunURLs,
		Input: os.Stdin, Output: os.Stdout, ErrorOutput: os.Stderr,
	})
}

func runChat(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 || arguments[0] == "" || strings.HasPrefix(arguments[0], "-") {
		return errorsUsage("chat requires a target username before its flags")
	}
	target := arguments[0]
	flags := flag.NewFlagSet("termcall chat", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	username := flags.String("username", "", "ephemeral local username (required)")
	serverURL := flags.String("server", defaultServerURL, "signaling WebSocket URL")
	var stunURLs stringList
	flags.Var(&stunURLs, "stun", "STUN URL; may be repeated")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *username == "" {
		flags.Usage()
		return errorsUsage("chat requires --username")
	}
	return app.RunChat(ctx, app.Config{
		Username: *username, ServerURL: *serverURL, STUNURLs: stunURLs,
		Input: os.Stdin, Output: os.Stdout, ErrorOutput: os.Stderr,
	}, target)
}

type usageError string

func (err usageError) Error() string   { return string(err) }
func errorsUsage(message string) error { return usageError(message) }

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  termcall listen --username <name> [--server <ws-url>] [--stun <url>]")
	fmt.Fprintln(os.Stderr, "  termcall chat <username> --username <name> [--server <ws-url>] [--stun <url>]")
	fmt.Fprintln(os.Stderr, "  termcall --version")
}
