package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"termcall/internal/identity"
	"termcall/internal/protocol"
	"termcall/internal/server/access"
	"termcall/internal/server/calls"
	"termcall/internal/server/httpapi"
	"termcall/internal/server/presence"
	turncredentials "termcall/internal/server/turn"
)

var (
	ErrSlowClient   = errors.New("client outbound queue is full")
	ErrUnauthorized = errors.New("unauthorized signaling message")
	ErrRateLimited  = errors.New("call invitation rate limit exceeded")
	ErrPeerOffline  = errors.New("recipient is offline")
)

type Config struct {
	QueueSize          int
	HelloTimeout       time.Duration
	WriteTimeout       time.Duration
	PingInterval       time.Duration
	IdleTimeout        time.Duration
	SweepInterval      time.Duration
	RingTimeout        time.Duration
	NegotiationTimeout time.Duration
	CleanupAfter       time.Duration
	InviteLimit        int
	InviteWindow       time.Duration
	Now                func() time.Time
	Access             *access.Gate
	STUNURLs           []string
	TURN               *turncredentials.Issuer
}

func DefaultConfig() Config {
	return Config{
		QueueSize:          64,
		HelloTimeout:       10 * time.Second,
		WriteTimeout:       5 * time.Second,
		PingInterval:       20 * time.Second,
		IdleTimeout:        60 * time.Second,
		SweepInterval:      time.Second,
		RingTimeout:        45 * time.Second,
		NegotiationTimeout: 30 * time.Second,
		CleanupAfter:       time.Minute,
		InviteLimit:        5,
		InviteWindow:       time.Minute,
		Now:                time.Now,
	}
}

type Server struct {
	config      Config
	logger      *slog.Logger
	validator   protocol.Validator
	presence    *presence.Registry
	calls       *calls.Manager
	userInvites *keyedFixedWindowLimiter
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	identityMu  sync.Mutex
	suffixes    map[string]string
	replays     identity.ReplayGuard
}

func New(config Config, logger *slog.Logger) *Server {
	defaults := DefaultConfig()
	if config.QueueSize <= 0 {
		config.QueueSize = defaults.QueueSize
	}
	if config.HelloTimeout <= 0 {
		config.HelloTimeout = defaults.HelloTimeout
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaults.WriteTimeout
	}
	if config.PingInterval <= 0 {
		config.PingInterval = defaults.PingInterval
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaults.IdleTimeout
	}
	if config.SweepInterval <= 0 {
		config.SweepInterval = defaults.SweepInterval
	}
	if config.RingTimeout <= 0 {
		config.RingTimeout = defaults.RingTimeout
	}
	if config.NegotiationTimeout <= 0 {
		config.NegotiationTimeout = defaults.NegotiationTimeout
	}
	if config.CleanupAfter <= 0 {
		config.CleanupAfter = defaults.CleanupAfter
	}
	if config.InviteLimit <= 0 {
		config.InviteLimit = defaults.InviteLimit
	}
	if config.InviteWindow <= 0 {
		config.InviteWindow = defaults.InviteWindow
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	ctx, cancel := context.WithCancel(context.Background())
	validator := protocol.NewValidator()
	validator.Now = config.Now
	server := &Server{
		config: config, logger: logger, validator: validator,
		presence: presence.New(),
		calls: calls.New(calls.Config{
			RingTimeout: config.RingTimeout, NegotiationTimeout: config.NegotiationTimeout,
			CleanupAfter: config.CleanupAfter,
		}),
		userInvites: newKeyedFixedWindowLimiter(config.InviteLimit, config.InviteWindow),
		suffixes:    make(map[string]string),
		ctx:         ctx, cancel: cancel,
	}
	server.wg.Add(1)
	go server.sweepLoop()
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	httpapi.RegisterTURN(mux, s.config.Access, s.config.TURN)
	mux.HandleFunc("GET /v1/ws", s.handleWebSocket)
	mux.HandleFunc("GET /healthz", statusHandler)
	mux.HandleFunc("GET /readyz", statusHandler)
	return mux
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	s.presence.CloseAll()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func statusHandler(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("{\"status\":\"ok\"}\n"))
}

func (s *Server) handleWebSocket(writer http.ResponseWriter, request *http.Request) {
	if !s.config.Access.Authorize(request) {
		http.Error(writer, "valid bearer access key is required", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.logger.Warn("websocket upgrade failed", "error", err)
		return
	}
	conn.SetReadLimit(protocol.MaxSDPMessageSize)
	client := newClientConnection(s, conn)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serve(client)
	}()
}

func (s *Server) serve(client *clientConnection) {
	defer func() {
		s.disconnect(client)
		client.wg.Wait()
	}()
	client.wg.Add(1)
	go client.writeLoop()

	helloContext, cancel := context.WithTimeout(s.ctx, s.config.HelloTimeout)
	messageType, data, err := client.conn.Read(helloContext)
	cancel()
	if err != nil || messageType != websocket.MessageText {
		return
	}
	hello, err := protocol.DecodeSignal(data, s.validator)
	if err != nil || hello.Type != protocol.SignalSessionHello {
		return
	}
	proof, _, fingerprint, err := identity.Verify(hello, s.now())
	if err != nil || !s.replays.Accept(fingerprint, proof.Nonce, proof.ExpiresAt, s.now()) {
		_ = client.conn.Close(websocket.StatusPolicyViolation, "identity proof failed")
		return
	}
	suffix := fingerprint[:protocol.AddressSuffixLength]
	s.identityMu.Lock()
	knownFingerprint, collision := s.suffixes[suffix]
	if !collision {
		s.suffixes[suffix] = fingerprint
	}
	s.identityMu.Unlock()
	if collision && knownFingerprint != fingerprint {
		_ = client.conn.Close(websocket.StatusPolicyViolation, "identity suffix collision")
		return
	}
	client.username = hello.From
	client.fingerprint = fingerprint
	if err := s.presence.Register(client); err != nil {
		_ = client.conn.Close(websocket.StatusPolicyViolation, "username already connected")
		return
	}
	client.registered.Store(true)
	client.touch(s.now())
	if err := client.Deliver(s.message(protocol.SignalSessionReady, "server", client.username, "", protocol.SessionReadyPayload{STUNURLs: s.config.STUNURLs})); err != nil {
		return
	}
	s.logger.Info("signaling client connected", "user", client.username)

	client.wg.Add(1)
	go client.heartbeatLoop()
	for {
		messageType, data, err = client.conn.Read(s.ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			s.sendError(client, "invalid_message", "only text WebSocket messages are accepted")
			continue
		}
		message, decodeErr := protocol.DecodeSignal(data, s.validator)
		if decodeErr != nil {
			s.sendError(client, "invalid_message", decodeErr.Error())
			continue
		}
		client.touch(s.now())
		if client.duplicate(message.ID) {
			continue
		}
		if err := s.dispatch(client, message); err != nil {
			s.sendError(client, errorCode(err), err.Error())
		}
	}
}

func (s *Server) dispatch(client *clientConnection, message protocol.SignalMessage) error {
	if message.From != client.username {
		return ErrUnauthorized
	}
	if serverOnly(message.Type) {
		return ErrUnauthorized
	}
	now := s.now()
	switch message.Type {
	case protocol.SignalSessionPong:
		if message.To != "server" {
			return ErrUnauthorized
		}
		return nil
	case protocol.SignalSessionPing:
		if message.To != "server" {
			return ErrUnauthorized
		}
		return client.Deliver(s.message(protocol.SignalSessionPong, "server", client.username, "", nil))
	case protocol.SignalPresenceQuery:
		_, online := s.presence.Get(message.To)
		return client.Deliver(s.message(protocol.SignalPresenceResult, "server", client.username, "", protocol.PresencePayload{Online: online}))
	case protocol.SignalCallInvite:
		if err := s.verifyBoundProof(client, message); err != nil {
			return err
		}
		if !client.invites.Allow(now) || !s.userInvites.Allow(client.username, now) {
			return ErrRateLimited
		}
		target, online := s.presence.Get(message.To)
		if !online {
			return ErrPeerOffline
		}
		if _, err := s.calls.Invite(message.CallID, client.username, message.To, now); err != nil {
			if errors.Is(err, calls.ErrUserBusy) {
				return client.Deliver(s.message(protocol.SignalCallBusy, message.To, client.username, message.CallID, nil))
			}
			return err
		}
		if err := target.Deliver(message); err != nil {
			target.Close()
			s.calls.Disconnect(message.To, now)
			return ErrPeerOffline
		}
		return client.Deliver(s.message(protocol.SignalCallRinging, message.To, client.username, message.CallID, nil))
	case protocol.SignalCallAccept:
		if err := s.verifyBoundProof(client, message); err != nil {
			return err
		}
		if _, err := s.calls.Transition(message.Type, message.CallID, client.username, message.To, now); err != nil {
			return err
		}
		return s.deliverTo(message.To, message)
	case protocol.SignalCallDecline, protocol.SignalCallCancel,
		protocol.SignalCallEnd, protocol.SignalWebRTCOffer, protocol.SignalWebRTCAnswer,
		protocol.SignalWebRTCICE, protocol.SignalWebRTCICEComplete:
		if _, err := s.calls.Transition(message.Type, message.CallID, client.username, message.To, now); err != nil {
			return err
		}
		return s.deliverTo(message.To, message)
	default:
		return ErrUnauthorized
	}
}

func (s *Server) verifyBoundProof(client *clientConnection, message protocol.SignalMessage) error {
	proof, _, fingerprint, err := identity.Verify(message, s.now())
	if err != nil || fingerprint != client.fingerprint || !s.replays.Accept(fingerprint, proof.Nonce, proof.ExpiresAt, s.now()) {
		return fmt.Errorf("%w: invalid or replayed identity proof", ErrUnauthorized)
	}
	return nil
}

func (s *Server) deliverTo(username string, message protocol.SignalMessage) error {
	target, online := s.presence.Get(username)
	if !online {
		return ErrPeerOffline
	}
	if err := target.Deliver(message); err != nil {
		target.Close()
		return ErrPeerOffline
	}
	return nil
}

func (s *Server) disconnect(client *clientConnection) {
	client.Close()
	if !client.registered.Load() {
		return
	}
	s.presence.Unregister(client)
	if s.ctx.Err() != nil {
		s.logger.Info("signaling client disconnected during shutdown", "user", client.username)
		return
	}
	for _, call := range s.calls.Disconnect(client.username, s.now()) {
		other, err := call.Other(client.username)
		if err == nil {
			_ = s.deliverTo(other, s.message(protocol.SignalCallEnd, client.username, other, call.ID, nil))
		}
	}
	s.logger.Info("signaling client disconnected", "user", client.username)
}

func (s *Server) sweepLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.config.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, call := range s.calls.Expire(s.now()) {
				_ = s.deliverTo(call.Caller, s.message(protocol.SignalCallTimeout, call.Callee, call.Caller, call.ID, nil))
				_ = s.deliverTo(call.Callee, s.message(protocol.SignalCallTimeout, call.Caller, call.Callee, call.ID, nil))
			}
		}
	}
}

func (s *Server) sendError(client *clientConnection, code, text string) {
	_ = client.Deliver(s.message(protocol.SignalSessionError, "server", client.username, "", protocol.ErrorPayload{Code: code, Message: text}))
}

func (s *Server) message(messageType protocol.SignalType, from, to, callID string, payload any) protocol.SignalMessage {
	message := protocol.SignalMessage{
		Version: protocol.ProtocolVersion, ID: uuid.NewString(), Type: messageType,
		Timestamp: s.now().UTC(), From: from, To: to, CallID: callID,
	}
	if payload != nil {
		message.Payload, _ = json.Marshal(payload)
	}
	return message
}

func (s *Server) now() time.Time { return s.config.Now() }

func serverOnly(messageType protocol.SignalType) bool {
	switch messageType {
	case protocol.SignalSessionHello, protocol.SignalSessionReady, protocol.SignalSessionError,
		protocol.SignalPresenceResult, protocol.SignalCallRinging, protocol.SignalCallBusy,
		protocol.SignalCallTimeout:
		return true
	default:
		return false
	}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, calls.ErrNotParticipant):
		return "unauthorized"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrPeerOffline):
		return "peer_offline"
	case errors.Is(err, calls.ErrUserBusy):
		return "busy"
	case errors.Is(err, calls.ErrInvalidTransition):
		return "invalid_transition"
	case errors.Is(err, calls.ErrCandidateLimit):
		return "candidate_limit"
	default:
		return "invalid_message"
	}
}

type clientConnection struct {
	server      *Server
	conn        *websocket.Conn
	username    string
	fingerprint string
	send        chan []byte
	done        chan struct{}
	closeOnce   sync.Once
	registered  atomic.Bool
	lastSeen    atomic.Int64
	seenMu      sync.Mutex
	seenIDs     map[string]struct{}
	seenOrder   []string
	seenLimit   int
	invites     *fixedWindowLimiter
	wg          sync.WaitGroup
}

func newClientConnection(server *Server, conn *websocket.Conn) *clientConnection {
	return &clientConnection{
		server: server, conn: conn, send: make(chan []byte, server.config.QueueSize),
		done: make(chan struct{}), seenIDs: make(map[string]struct{}, server.config.QueueSize*4),
		seenLimit: server.config.QueueSize * 4,
		invites:   newFixedWindowLimiter(server.config.InviteLimit, server.config.InviteWindow),
	}
}

func (c *clientConnection) Username() string { return c.username }

func (c *clientConnection) Deliver(message protocol.SignalMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode signaling message: %w", err)
	}
	select {
	case c.send <- encoded:
		return nil
	case <-c.done:
		return ErrPeerOffline
	default:
		go c.Close()
		return ErrSlowClient
	}
}

func (c *clientConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.CloseNow()
		}
	})
}

func (c *clientConnection) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case data := <-c.send:
			ctx, cancel := context.WithTimeout(c.server.ctx, c.server.config.WriteTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *clientConnection) heartbeatLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.server.config.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			now := c.server.now()
			lastSeen := time.Unix(0, c.lastSeen.Load())
			if now.Sub(lastSeen) > c.server.config.IdleTimeout {
				c.Close()
				return
			}
			if err := c.Deliver(c.server.message(protocol.SignalSessionPing, "server", c.username, "", nil)); err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *clientConnection) touch(now time.Time) { c.lastSeen.Store(now.UnixNano()) }

func (c *clientConnection) duplicate(id string) bool {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if _, exists := c.seenIDs[id]; exists {
		return true
	}
	c.seenIDs[id] = struct{}{}
	c.seenOrder = append(c.seenOrder, id)
	if len(c.seenOrder) > c.seenLimit {
		oldest := c.seenOrder[0]
		c.seenOrder = c.seenOrder[1:]
		delete(c.seenIDs, oldest)
	}
	return false
}
