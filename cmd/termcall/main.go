package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	xterm "golang.org/x/term"

	"termcall/internal/buildinfo"
	"termcall/internal/client/app"
	clientdaemon "termcall/internal/client/daemon"
	"termcall/internal/client/diagnostics"
	"termcall/internal/client/profile"
	"termcall/internal/client/signaling"
	"termcall/internal/identity"
	"termcall/internal/protocol"
)

const defaultServerURL = "ws://127.0.0.1:8080/v1/ws"

type stringList []string

func (values *stringList) String() string         { return fmt.Sprint([]string(*values)) }
func (values *stringList) Set(value string) error { *values = append(*values, value); return nil }

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
	case "init":
		err = runInit(os.Args[2:])
	case "identity":
		err = runIdentity(os.Args[2:])
	case "trust":
		err = runTrustCommand(os.Args[2:], "trust")
	case "untrust":
		err = runTrustCommand(os.Args[2:], "untrust")
	case "block":
		err = runTrustCommand(os.Args[2:], "block")
	case "unblock":
		err = runTrustCommand(os.Args[2:], "unblock")
	case "daemon":
		err = runDaemon(ctx, os.Args[2:])
	case "answer":
		err = runIncoming(ctx, os.Args[2:], false)
	case "decline":
		err = runIncoming(ctx, os.Args[2:], true)
	case "listen":
		err = runListen(ctx, os.Args[2:])
	case "chat":
		err = runChat(ctx, os.Args[2:])
	case "devices":
		if len(os.Args) != 2 {
			err = errorsUsage("devices does not accept arguments")
		} else {
			err = diagnostics.Devices(ctx, os.Stdout)
		}
	case "doctor":
		if len(os.Args) != 2 {
			err = errorsUsage("doctor does not accept arguments")
		} else {
			err = diagnostics.Doctor(ctx, os.Stdout)
		}
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

func runInit(arguments []string) error {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return errorsUsage("init requires a base name")
	}
	baseName := arguments[0]
	if !protocol.ValidBaseName(baseName) {
		return errors.New("base name must be 3-19 lowercase characters, start/end alphanumeric, and contain only a-z, 0-9, _ or -")
	}
	flags := flag.NewFlagSet("termcall init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	serverURL := flags.String("server", savedServerURL(), "signaling WebSocket URL")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errorsUsage("init received unexpected arguments")
	}
	if err := validateServerURL(*serverURL); err != nil {
		return err
	}

	localIdentity, err := identity.Load()
	if errors.Is(err, os.ErrNotExist) {
		localIdentity, err = identity.Generate()
		if err != nil {
			return err
		}
		if err := identity.SaveNew(localIdentity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	address, err := identity.CanonicalAddress(baseName, localIdentity.PublicKey)
	if err != nil {
		return err
	}
	accessKey, err := profile.AccessKeyFromEnvironment()
	if err != nil {
		return err
	}
	if accessKey == "" {
		accessKey, err = readSecret(bufio.NewReader(os.Stdin), "Server access key (blank for a local open server): ")
		if err != nil {
			return err
		}
	}
	if accessKey == "" && !isLocalServer(*serverURL) {
		return errors.New("a remote signaling server requires an access key")
	}
	value := profile.Profile{Version: profile.Version, BaseName: baseName, Address: address, ServerURL: *serverURL, AccessKey: accessKey}
	if err := profile.Save(value); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "initialized %s\nfingerprint: %s\n", address, identity.Fingerprint(localIdentity.PublicKey))
	return nil
}

func runIdentity(arguments []string) error {
	if len(arguments) != 0 {
		return errorsUsage("identity does not accept arguments")
	}
	value, localIdentity, err := loadProfileIdentity()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s\n%s\n", value.Address, identity.Fingerprint(localIdentity.PublicKey))
	return nil
}

func runTrustCommand(arguments []string, command string) error {
	if len(arguments) != 1 {
		return errorsUsage(command + " requires a fingerprint or known unique prefix")
	}
	store, err := identity.OpenTrustStore()
	if err != nil {
		return err
	}
	var record identity.TrustRecord
	switch command {
	case "trust":
		record, err = store.SetTrusted(arguments[0], true)
	case "untrust":
		record, err = store.SetTrusted(arguments[0], false)
	case "block":
		record, err = store.SetBlockedFingerprint(arguments[0], true)
	case "unblock":
		record, err = store.SetBlockedFingerprint(arguments[0], false)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n", command, record.Fingerprint)
	return nil
}

func runDaemon(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("termcall daemon", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socketDefault, err := clientdaemon.DefaultSocketPath()
	if err != nil {
		return err
	}
	socketPath := flags.String("socket", socketDefault, "local daemon socket path")
	incoming := flags.String("incoming", string(clientdaemon.BehaviorOpenTerminal), "incoming behavior: open-terminal, print, or ignore")
	dnd := flags.Bool("do-not-disturb", false, "decline incoming calls")
	terminalCommand := flags.String("terminal", "", "terminal emulator executable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errorsUsage("daemon received unexpected arguments")
	}
	value, localIdentity, err := loadProfileIdentity()
	if err != nil {
		return err
	}
	connect := func(connectContext context.Context) (*signaling.Client, error) {
		return signaling.Connect(connectContext, signaling.Config{URL: value.ServerURL, Address: value.Address, AccessKey: value.AccessKey, Identity: localIdentity})
	}
	trustStore, err := identity.OpenTrustStore()
	if err != nil {
		return err
	}
	var replays identity.ReplayGuard
	verify := func(_ context.Context, invite protocol.SignalMessage) error {
		proof, publicKey, fingerprint, err := identity.Verify(invite, time.Now())
		if err != nil {
			return err
		}
		if !replays.Accept(fingerprint, proof.Nonce, proof.ExpiresAt, time.Now()) {
			return errors.New("replayed invitation")
		}
		record, reused, err := trustStore.Observe(value.ServerURL, invite.From, publicKey)
		if err != nil {
			return err
		}
		if len(reused) != 0 {
			fmt.Fprintf(os.Stderr, "termcall daemon: WARNING alias %s previously used fingerprint %s; current identity is UNKNOWN\n", invite.From, strings.Join(reused, ", "))
		}
		if record.Blocked {
			return fmt.Errorf("fingerprint %s is blocked", fingerprint)
		}
		if !record.Trusted {
			fmt.Fprintf(os.Stderr, "termcall daemon: UNKNOWN identity %s (%s)\n", fingerprint, invite.From)
		}
		return nil
	}
	return clientdaemon.Run(ctx, clientdaemon.Config{Username: value.Address, SocketPath: *socketPath, Behavior: clientdaemon.Behavior(*incoming), DoNotDisturb: *dnd, Terminal: *terminalCommand, Connect: connect, Output: os.Stdout, ErrorOutput: os.Stderr, VerifyInvitation: verify})
}

func runIncoming(ctx context.Context, arguments []string, decline bool) error {
	command := "answer"
	if decline {
		command = "decline"
	}
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return errorsUsage(command + " requires a call ID")
	}
	callID := arguments[0]
	flags := flag.NewFlagSet("termcall "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socketDefault, err := clientdaemon.DefaultSocketPath()
	if err != nil {
		return err
	}
	socketPath := flags.String("socket", socketDefault, "local daemon socket path")
	media := addMediaFlags(flags)
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errorsUsage(command + " received unexpected arguments")
	}
	value, localIdentity, err := loadProfileIdentity()
	if err != nil {
		return err
	}
	handoff, err := clientdaemon.Connect(ctx, *socketPath, callID)
	if err != nil {
		return err
	}
	if handoff.Username() != value.Address {
		handoff.Close()
		return errors.New("daemon identity does not match the saved profile")
	}
	if decline {
		return app.DeclineIncoming(ctx, handoff, handoff.Invite())
	}
	config := media.config(value, localIdentity)
	return app.RunIncoming(ctx, config, handoff, handoff.Invite())
}

func runListen(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("termcall listen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	media := addMediaFlags(flags)
	var stunURLs stringList
	flags.Var(&stunURLs, "stun", "STUN URL; may be repeated")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errorsUsage("listen received unexpected arguments")
	}
	value, localIdentity, err := loadProfileIdentity()
	if err != nil {
		return err
	}
	config := media.config(value, localIdentity)
	config.STUNURLs = stunURLs
	return app.RunListen(ctx, config)
}

func runChat(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return errorsUsage("chat requires a canonical target address")
	}
	target := arguments[0]
	flags := flag.NewFlagSet("termcall chat", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	media := addMediaFlags(flags)
	var stunURLs stringList
	flags.Var(&stunURLs, "stun", "STUN URL; may be repeated")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errorsUsage("chat received unexpected arguments")
	}
	value, localIdentity, err := loadProfileIdentity()
	if err != nil {
		return err
	}
	config := media.config(value, localIdentity)
	config.STUNURLs = stunURLs
	return app.RunChat(ctx, config, target)
}

type mediaFlags struct {
	video               *bool
	camera              *string
	columns, rows, fps  *int
	audio               *bool
	microphone, speaker *string
	bitrate             *int
	relayOnly           *bool
}

func addMediaFlags(flags *flag.FlagSet) mediaFlags {
	return mediaFlags{
		video: flags.Bool("video", true, "capture and send ASCII camera video"), camera: flags.String("camera", "", "camera device (/dev path on Linux, numeric index on macOS)"),
		columns: flags.Int("video-columns", 100, "maximum ASCII video columns"), rows: flags.Int("video-rows", 34, "maximum ASCII video rows"), fps: flags.Int("video-fps", 15, "maximum ASCII video FPS"),
		audio: flags.Bool("audio", true, "send and play Opus audio"), microphone: flags.String("microphone", "", "microphone device"), speaker: flags.String("speaker", "", "speaker device"), bitrate: flags.Int("audio-bitrate", 32000, "Opus bitrate"), relayOnly: flags.Bool("relay-only", false, "require TURN relay"),
	}
}
func (m mediaFlags) config(value profile.Profile, localIdentity *identity.Identity) app.Config {
	return app.Config{
		Address: value.Address, ServerURL: value.ServerURL, AccessKey: value.AccessKey, Identity: localIdentity,
		Video: *m.video, CameraDevice: *m.camera, VideoColumns: *m.columns, VideoRows: *m.rows, VideoFPS: *m.fps,
		Audio: *m.audio, Microphone: *m.microphone, Speaker: *m.speaker, AudioBitrate: *m.bitrate, RelayOnly: *m.relayOnly,
		Input: os.Stdin, Output: os.Stdout, ErrorOutput: os.Stderr,
	}
}

func loadProfileIdentity() (profile.Profile, *identity.Identity, error) {
	value, err := profile.Load()
	if err != nil {
		return profile.Profile{}, nil, err
	}
	localIdentity, err := identity.Load()
	if err != nil {
		return profile.Profile{}, nil, err
	}
	if err := profile.ValidateWithIdentity(value, localIdentity); err != nil {
		return profile.Profile{}, nil, err
	}
	return value, localIdentity, nil
}
func savedServerURL() string {
	value, err := profile.Load()
	if err == nil {
		return value.ServerURL
	}
	return defaultServerURL
}
func validateServerURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil {
		return errors.New("server must be a valid ws:// or wss:// URL")
	}
	return nil
}
func isLocalServer(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
func readSecret(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if xterm.IsTerminal(int(os.Stdin.Fd())) {
		value, err := xterm.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(value), err
	}
	value, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) && value != "" {
		err = nil
	}
	return strings.TrimRight(value, "\r\n"), err
}

type usageError string

func (err usageError) Error() string   { return string(err) }
func errorsUsage(message string) error { return usageError(message) }
func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  termcall init <base-name> [--server <ws-url>]
  termcall identity
  termcall trust|untrust|block|unblock <fingerprint-or-known-prefix>
  termcall daemon [--incoming open-terminal|print|ignore] [--do-not-disturb]
  termcall answer|decline <call-id>
  termcall listen [media flags]
  termcall chat <canonical-address> [media flags]
  termcall devices | doctor | --version`)
}
