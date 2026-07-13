package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"termcall/internal/client/launcher"
	"termcall/internal/client/signaling"
	"termcall/internal/protocol"
)

type Behavior string

const (
	BehaviorOpenTerminal Behavior = "open-terminal"
	BehaviorPrint        Behavior = "print"
	BehaviorIgnore       Behavior = "ignore"
)

type ConnectFunc func(context.Context) (*signaling.Client, error)

type Config struct {
	Username         string
	SocketPath       string
	Behavior         Behavior
	DoNotDisturb     bool
	Terminal         string
	InviteLimit      int
	InviteWindow     time.Duration
	ReconnectMin     time.Duration
	ReconnectMax     time.Duration
	StableConnection time.Duration
	Connect          ConnectFunc
	Output           io.Writer
	ErrorOutput      io.Writer
	LaunchTerminal   func(configured, callID string) error
	VerifyInvitation func(context.Context, protocol.SignalMessage) error
}

type pendingCall struct {
	invite  protocol.SignalMessage
	claimed bool
}

type localPeer struct {
	conn    net.Conn
	encoder *json.Encoder
	mu      sync.Mutex
}

type service struct {
	config   Config
	mu       sync.Mutex
	signal   *signaling.Client
	stunURLs []string
	pending  map[string]*pendingCall
	sessions map[string]*localPeer
	invites  map[string][]time.Time
}

func DefaultSocketPath() (string, error) {
	if runtimeDirectory := os.Getenv("XDG_RUNTIME_DIR"); runtimeDirectory != "" {
		return filepath.Join(runtimeDirectory, "termcall", "daemon.sock"), nil
	}
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDirectory, "termcall", "daemon.sock"), nil
}

func Run(ctx context.Context, config Config) error {
	if config.Connect == nil || config.Username == "" || config.SocketPath == "" {
		return errors.New("daemon username, socket path, and signaling connector are required")
	}
	if config.Behavior == "" {
		config.Behavior = BehaviorOpenTerminal
	}
	if config.Behavior != BehaviorOpenTerminal && config.Behavior != BehaviorPrint && config.Behavior != BehaviorIgnore {
		return fmt.Errorf("invalid incoming-call behavior %q", config.Behavior)
	}
	if config.InviteLimit <= 0 {
		config.InviteLimit = 3
	}
	if config.InviteWindow <= 0 {
		config.InviteWindow = time.Minute
	}
	if config.ReconnectMin <= 0 {
		config.ReconnectMin = time.Second
	}
	if config.ReconnectMax < config.ReconnectMin {
		config.ReconnectMax = 30 * time.Second
	}
	if config.StableConnection <= 0 {
		config.StableConnection = time.Minute
	}
	if config.Output == nil {
		config.Output = io.Discard
	}
	if config.ErrorOutput == nil {
		config.ErrorOutput = io.Discard
	}
	if config.LaunchTerminal == nil {
		config.LaunchTerminal = launcher.Launch
	}

	listener, err := listen(config.SocketPath)
	if err != nil {
		return err
	}
	defer func() {
		listener.Close()
		_ = os.Remove(config.SocketPath)
	}()
	s := &service{config: config, pending: make(map[string]*pendingCall), sessions: make(map[string]*localPeer), invites: make(map[string][]time.Time)}
	go s.acceptLoop(ctx, listener)
	fmt.Fprintf(config.Output, "termcall daemon listening as %s\n", config.Username)

	backoff := config.ReconnectMin
	for {
		client, err := config.Connect(ctx)
		if err != nil {
			fmt.Fprintf(config.ErrorOutput, "termcall daemon: signaling connection failed: %v; retrying in %s\n", err, backoff)
			if err := waitForReconnect(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, config.ReconnectMax)
			continue
		}
		connectedAt := time.Now()
		s.setSignal(client)
		fmt.Fprintln(config.Output, "termcall daemon connected to signaling")
		err = s.runConnected(ctx, client)
		client.Close()
		s.clearSignal(client, err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(connectedAt) >= config.StableConnection {
			backoff = config.ReconnectMin
		}
		fmt.Fprintf(config.ErrorOutput, "termcall daemon: signaling connection lost: %v; retrying in %s\n", err, backoff)
		if err := waitForReconnect(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, config.ReconnectMax)
	}
}

func waitForReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func listen(path string) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create daemon socket directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("daemon socket path exists and is not a socket")
		}
		connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			connection.Close()
			return nil, errors.New("another termcall daemon is already running")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func (s *service) runConnected(ctx context.Context, client *signaling.Client) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-client.Errors():
			return err
		case message, ok := <-client.Events():
			if !ok {
				return errors.New("signaling connection closed")
			}
			s.handleSignal(ctx, client, message)
		}
	}
}

func (s *service) handleSignal(ctx context.Context, client *signaling.Client, message protocol.SignalMessage) {
	if message.Type == protocol.SignalSessionError {
		fmt.Fprintf(s.config.ErrorOutput, "termcall daemon: signaling error: %s\n", string(message.Payload))
		s.mu.Lock()
		peers := make([]*localPeer, 0, len(s.sessions))
		for _, peer := range s.sessions {
			peers = append(peers, peer)
		}
		s.mu.Unlock()
		for _, peer := range peers {
			if err := peer.send(wireMessage{Kind: "signal", Signal: message}); err != nil {
				peer.conn.Close()
			}
		}
		return
	}
	if message.Type == protocol.SignalCallInvite {
		s.handleInvite(ctx, client, message)
		return
	}
	s.mu.Lock()
	peer := s.sessions[message.CallID]
	if isTerminalSignal(message.Type) {
		delete(s.pending, message.CallID)
		delete(s.sessions, message.CallID)
	}
	s.mu.Unlock()
	if peer != nil {
		if err := peer.send(wireMessage{Kind: "signal", Signal: message}); err != nil {
			peer.conn.Close()
		}
	}
}

func (s *service) handleInvite(ctx context.Context, client *signaling.Client, invite protocol.SignalMessage) {
	if s.config.VerifyInvitation != nil {
		if err := s.config.VerifyInvitation(ctx, invite); err != nil {
			fmt.Fprintf(s.config.ErrorOutput, "termcall daemon: rejected invitation from %s: %v\n", invite.From, err)
			_ = send(client, ctx, protocol.SignalCallDecline, s.config.Username, invite.From, invite.CallID)
			return
		}
	}
	now := time.Now()
	s.mu.Lock()
	allowed := s.allowInviteLocked(invite.From, now)
	_, duplicate := s.pending[invite.CallID]
	if allowed && !duplicate && !s.config.DoNotDisturb && s.config.Behavior != BehaviorIgnore {
		s.pending[invite.CallID] = &pendingCall{invite: invite}
	}
	s.mu.Unlock()
	if duplicate {
		return
	}
	if !allowed || s.config.DoNotDisturb || s.config.Behavior == BehaviorIgnore {
		_ = send(client, ctx, protocol.SignalCallDecline, s.config.Username, invite.From, invite.CallID)
		return
	}
	fmt.Fprintf(s.config.Output, "incoming call from %s (%s)\n", invite.From, invite.CallID)
	if s.config.Behavior == BehaviorPrint {
		fmt.Fprintf(s.config.Output, "run: termcall answer %s\n", invite.CallID)
		return
	}
	if err := s.config.LaunchTerminal(s.config.Terminal, invite.CallID); err != nil {
		fmt.Fprintf(s.config.ErrorOutput, "termcall daemon: cannot open terminal: %v\nrun: termcall answer %s\n", err, invite.CallID)
	}
}

func (s *service) acceptLoop(ctx context.Context, listener net.Listener) {
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go s.handleLocal(connection)
	}
}

func (s *service) handleLocal(connection net.Conn) {
	peer := &localPeer{conn: connection, encoder: json.NewEncoder(connection)}
	decoder := json.NewDecoder(connection)
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	var attach wireMessage
	if err := decoder.Decode(&attach); err != nil || attach.Kind != "attach" || attach.CallID == "" {
		_ = peer.send(wireMessage{Kind: "error", Error: "invalid daemon handoff request"})
		connection.Close()
		return
	}
	_ = connection.SetReadDeadline(time.Time{})

	s.mu.Lock()
	pending := s.pending[attach.CallID]
	if pending == nil || pending.claimed || s.signal == nil {
		s.mu.Unlock()
		_ = peer.send(wireMessage{Kind: "error", Error: "incoming call is no longer available"})
		connection.Close()
		return
	}
	pending.claimed = true
	s.sessions[attach.CallID] = peer
	ready := wireMessage{Kind: "ready", Username: s.config.Username, STUNURLs: append([]string(nil), s.stunURLs...), Invite: pending.invite}
	s.mu.Unlock()
	if err := peer.send(ready); err != nil {
		s.release(attach.CallID, peer)
		connection.Close()
		return
	}
	defer func() {
		s.release(attach.CallID, peer)
		connection.Close()
	}()
	for {
		var incoming wireMessage
		if err := decoder.Decode(&incoming); err != nil {
			return
		}
		if incoming.Kind != "signal" || !s.validLocalSignal(attach.CallID, incoming.Signal) {
			_ = peer.send(wireMessage{Kind: "error", Error: "invalid signaling message from local call window"})
			continue
		}
		s.mu.Lock()
		client := s.signal
		invite := s.pending[attach.CallID]
		s.mu.Unlock()
		if client == nil || invite == nil {
			return
		}
		if err := client.Send(context.Background(), incoming.Signal); err != nil {
			_ = peer.send(wireMessage{Kind: "error", Error: err.Error()})
			return
		}
		if incoming.Signal.Type == protocol.SignalCallDecline || incoming.Signal.Type == protocol.SignalCallEnd {
			s.mu.Lock()
			delete(s.pending, attach.CallID)
			s.mu.Unlock()
			return
		}
	}
}

func (s *service) validLocalSignal(callID string, message protocol.SignalMessage) bool {
	if protocol.NewValidator().ValidateSignal(message) != nil || message.CallID != callID || message.From != s.config.Username {
		return false
	}
	s.mu.Lock()
	pending := s.pending[callID]
	s.mu.Unlock()
	if pending == nil || message.To != pending.invite.From {
		return false
	}
	switch message.Type {
	case protocol.SignalCallAccept, protocol.SignalCallDecline, protocol.SignalCallEnd,
		protocol.SignalWebRTCOffer, protocol.SignalWebRTCAnswer, protocol.SignalWebRTCICE,
		protocol.SignalWebRTCICEComplete:
		return true
	default:
		return false
	}
}

func (s *service) release(callID string, peer *localPeer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[callID] == peer {
		delete(s.sessions, callID)
		if pending := s.pending[callID]; pending != nil {
			pending.claimed = false
		}
	}
}

func (s *service) setSignal(client *signaling.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signal = client
	s.stunURLs = client.STUNURLs()
}

func (s *service) clearSignal(client *signaling.Client, reason error) {
	s.mu.Lock()
	if s.signal != client {
		s.mu.Unlock()
		return
	}
	s.signal = nil
	s.stunURLs = nil
	peers := make([]*localPeer, 0, len(s.sessions))
	for _, peer := range s.sessions {
		peers = append(peers, peer)
	}
	s.pending = make(map[string]*pendingCall)
	s.sessions = make(map[string]*localPeer)
	s.mu.Unlock()
	for _, peer := range peers {
		_ = peer.send(wireMessage{Kind: "error", Error: fmt.Sprintf("signaling connection lost: %v", reason)})
		peer.conn.Close()
	}
}

func (s *service) allowInviteLocked(caller string, now time.Time) bool {
	cutoff := now.Add(-s.config.InviteWindow)
	values := s.invites[caller][:0]
	for _, value := range s.invites[caller] {
		if value.After(cutoff) {
			values = append(values, value)
		}
	}
	if len(values) >= s.config.InviteLimit {
		s.invites[caller] = values
		return false
	}
	s.invites[caller] = append(values, now)
	return true
}

func (p *localPeer) send(message wireMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.encoder.Encode(message)
}

func send(client *signaling.Client, ctx context.Context, messageType protocol.SignalType, from, to, callID string) error {
	message, err := client.NewMessage(messageType, to, callID, nil)
	if err != nil {
		return err
	}
	message.From = from
	return client.Send(ctx, message)
}

func isTerminalSignal(messageType protocol.SignalType) bool {
	switch messageType {
	case protocol.SignalCallCancel, protocol.SignalCallDecline, protocol.SignalCallTimeout, protocol.SignalCallEnd:
		return true
	default:
		return false
	}
}
