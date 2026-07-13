package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/google/uuid"

	"termcall/internal/protocol"
)

func TestSignalingFlow(t *testing.T) {
	server, httpServer := newTestServer(t, Config{})
	alice := connectTestClient(t, httpServer.URL, "alice")
	defer alice.close()
	bob := connectTestClient(t, httpServer.URL, "bob")
	defer bob.close()

	alice.send(protocol.SignalPresenceQuery, "bob", "", nil)
	presenceResult := alice.read(protocol.SignalPresenceResult)
	var presence protocol.PresencePayload
	if err := json.Unmarshal(presenceResult.Payload, &presence); err != nil || !presence.Online {
		t.Fatalf("presence result = %+v, %v", presence, err)
	}

	callID := uuid.NewString()
	invite := alice.message(protocol.SignalCallInvite, "bob", callID, nil)
	alice.sendMessage(invite)
	if got := bob.read(protocol.SignalCallInvite); got.CallID != callID || got.From != "alice" {
		t.Fatalf("invite = %+v", got)
	}
	if got := alice.read(protocol.SignalCallRinging); got.CallID != callID {
		t.Fatalf("ringing = %+v", got)
	}

	// A repeated message ID is ignored rather than applying the transition twice.
	alice.sendMessage(invite)
	alice.send(protocol.SignalPresenceQuery, "bob", "", nil)
	if got := alice.read(protocol.SignalPresenceResult); got.Type != protocol.SignalPresenceResult {
		t.Fatalf("message after duplicate = %s", got.Type)
	}

	bob.send(protocol.SignalCallAccept, "alice", callID, nil)
	if got := alice.read(protocol.SignalCallAccept); got.From != "bob" {
		t.Fatalf("accept = %+v", got)
	}

	alice.send(protocol.SignalWebRTCOffer, "bob", callID, protocol.SDPPayload{SDP: "v=0\r\n"})
	if got := bob.read(protocol.SignalWebRTCOffer); got.CallID != callID {
		t.Fatalf("offer = %+v", got)
	}
	bob.send(protocol.SignalWebRTCICE, "alice", callID, protocol.ICEPayload{Candidate: "candidate:1 1 udp 1 127.0.0.1 1234 typ host"})
	if got := alice.read(protocol.SignalWebRTCICE); got.From != "bob" {
		t.Fatalf("ICE = %+v", got)
	}
	bob.send(protocol.SignalWebRTCAnswer, "alice", callID, protocol.SDPPayload{SDP: "v=0\r\n"})
	if got := alice.read(protocol.SignalWebRTCAnswer); got.CallID != callID {
		t.Fatalf("answer = %+v", got)
	}

	alice.send(protocol.SignalCallEnd, "bob", callID, nil)
	if got := bob.read(protocol.SignalCallEnd); got.From != "alice" {
		t.Fatalf("end = %+v", got)
	}
	call, exists := server.calls.Get(callID)
	if !exists || !call.Terminal() {
		t.Fatalf("server call after end = %+v, exists=%t", call, exists)
	}
}

func TestSignalingRejectsSpoofingAndInvalidTransitions(t *testing.T) {
	_, httpServer := newTestServer(t, Config{})
	alice := connectTestClient(t, httpServer.URL, "alice")
	defer alice.close()
	bob := connectTestClient(t, httpServer.URL, "bob")
	defer bob.close()

	spoofed := alice.message(protocol.SignalCallInvite, "bob", uuid.NewString(), nil)
	spoofed.From = "mallory"
	alice.sendMessage(spoofed)
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "unauthorized")

	bob.send(protocol.SignalCallAccept, "alice", uuid.NewString(), nil)
	assertErrorCode(t, bob.read(protocol.SignalSessionError), "invalid_message")

	alice.send(protocol.SignalPresenceQuery, "charlie", "", nil)
	result := alice.read(protocol.SignalPresenceResult)
	var payload protocol.PresencePayload
	if err := json.Unmarshal(result.Payload, &payload); err != nil || payload.Online {
		t.Fatalf("offline presence = %+v, %v", payload, err)
	}
}

func TestRingingCallTimesOutForBothParticipants(t *testing.T) {
	_, httpServer := newTestServer(t, Config{
		RingTimeout: 40 * time.Millisecond, SweepInterval: 5 * time.Millisecond,
		PingInterval: time.Hour, IdleTimeout: 2 * time.Hour,
	})
	alice := connectTestClient(t, httpServer.URL, "alice")
	defer alice.close()
	bob := connectTestClient(t, httpServer.URL, "bob")
	defer bob.close()

	callID := uuid.NewString()
	alice.send(protocol.SignalCallInvite, "bob", callID, nil)
	bob.read(protocol.SignalCallInvite)
	alice.read(protocol.SignalCallRinging)
	if got := alice.read(protocol.SignalCallTimeout); got.CallID != callID {
		t.Fatalf("caller timeout = %+v", got)
	}
	if got := bob.read(protocol.SignalCallTimeout); got.CallID != callID {
		t.Fatalf("callee timeout = %+v", got)
	}
}

func TestCallAlternativesAndParticipantAuthorization(t *testing.T) {
	_, httpServer := newTestServer(t, Config{})
	alice := connectTestClient(t, httpServer.URL, "alice")
	defer alice.close()
	bob := connectTestClient(t, httpServer.URL, "bob")
	defer bob.close()
	charlie := connectTestClient(t, httpServer.URL, "charlie")
	defer charlie.close()

	callID := uuid.NewString()
	alice.send(protocol.SignalCallInvite, "bob", callID, nil)
	bob.read(protocol.SignalCallInvite)
	alice.read(protocol.SignalCallRinging)

	charlie.send(protocol.SignalCallInvite, "bob", uuid.NewString(), nil)
	if got := charlie.read(protocol.SignalCallBusy); got.From != "bob" {
		t.Fatalf("busy = %+v", got)
	}
	charlie.send(protocol.SignalCallAccept, "alice", callID, nil)
	assertErrorCode(t, charlie.read(protocol.SignalSessionError), "unauthorized")

	alice.send(protocol.SignalCallCancel, "bob", callID, nil)
	if got := bob.read(protocol.SignalCallCancel); got.CallID != callID {
		t.Fatalf("cancel = %+v", got)
	}

	secondCallID := uuid.NewString()
	alice.send(protocol.SignalCallInvite, "bob", secondCallID, nil)
	bob.read(protocol.SignalCallInvite)
	alice.read(protocol.SignalCallRinging)
	bob.send(protocol.SignalCallDecline, "alice", secondCallID, nil)
	if got := alice.read(protocol.SignalCallDecline); got.CallID != secondCallID {
		t.Fatalf("decline = %+v", got)
	}
}

func TestDisconnectEndsActiveCall(t *testing.T) {
	_, httpServer := newTestServer(t, Config{})
	alice := connectTestClient(t, httpServer.URL, "alice")
	bob := connectTestClient(t, httpServer.URL, "bob")
	defer bob.close()
	callID := uuid.NewString()
	alice.send(protocol.SignalCallInvite, "bob", callID, nil)
	bob.read(protocol.SignalCallInvite)
	alice.read(protocol.SignalCallRinging)
	alice.close()
	if got := bob.read(protocol.SignalCallEnd); got.CallID != callID || got.From != "alice" {
		t.Fatalf("disconnect end = %+v", got)
	}
}

func TestMalformedOversizedAndRateLimitedMessages(t *testing.T) {
	_, httpServer := newTestServer(t, Config{InviteLimit: 1, InviteWindow: time.Minute})
	alice := connectTestClient(t, httpServer.URL, "alice")
	defer alice.close()

	alice.sendRaw([]byte(`{"version":`))
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "invalid_message")

	oversized := alice.message(protocol.SignalPresenceQuery, "bob", "", nil)
	oversized.Payload = json.RawMessage(`{"padding":"` + strings.Repeat("x", protocol.MaxSignalMessageSize) + `"}`)
	encoded, err := json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}
	alice.sendRaw(encoded)
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "invalid_message")

	alice.send(protocol.SignalWebRTCICE, "bob", uuid.NewString(), protocol.ICEPayload{Candidate: ""})
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "invalid_message")

	alice.send(protocol.SignalCallInvite, "offline-one", uuid.NewString(), nil)
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "peer_offline")
	alice.send(protocol.SignalCallInvite, "offline-two", uuid.NewString(), nil)
	assertErrorCode(t, alice.read(protocol.SignalSessionError), "rate_limited")
}

func TestHealthAndReadyEndpoints(t *testing.T) {
	_, httpServer := newTestServer(t, Config{})
	for _, path := range []string{"/healthz", "/readyz"} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.StatusCode)
		}
	}
}

func TestLogsExcludeNegotiationContent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := New(Config{PingInterval: time.Hour, IdleTimeout: 2 * time.Hour}, logger)
	httpServer := httptest.NewServer(server.Handler())
	alice := connectTestClient(t, httpServer.URL, "alice")
	bob := connectTestClient(t, httpServer.URL, "bob")
	callID := uuid.NewString()
	alice.send(protocol.SignalCallInvite, "bob", callID, nil)
	bob.read(protocol.SignalCallInvite)
	alice.read(protocol.SignalCallRinging)
	bob.send(protocol.SignalCallAccept, "alice", callID, nil)
	alice.read(protocol.SignalCallAccept)
	alice.send(protocol.SignalWebRTCOffer, "bob", callID, protocol.SDPPayload{SDP: "SECRET_SDP_CONTENT"})
	bob.read(protocol.SignalWebRTCOffer)
	alice.close()
	bob.close()
	httpServer.Close()
	shutdownTestServer(t, server)
	if strings.Contains(logs.String(), "SECRET_SDP_CONTENT") {
		t.Fatalf("negotiation content appeared in logs: %s", logs.String())
	}
}

func TestBoundedConnectionQueueAndRateLimiter(t *testing.T) {
	server := New(Config{QueueSize: 1}, nil)
	defer shutdownTestServer(t, server)
	client := newClientConnection(server, nil)
	message := server.message(protocol.SignalSessionReady, "server", "alice", "", nil)
	if err := client.Deliver(message); err != nil {
		t.Fatal(err)
	}
	if err := client.Deliver(message); !errors.Is(err, ErrSlowClient) {
		t.Fatalf("full queue error = %v", err)
	}
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("slow client was not disconnected")
	}

	limiter := newFixedWindowLimiter(2, time.Minute)
	now := time.Now()
	if !limiter.Allow(now) || !limiter.Allow(now) || limiter.Allow(now) {
		t.Fatal("limiter did not enforce its window")
	}
	if !limiter.Allow(now.Add(time.Minute)) {
		t.Fatal("limiter did not reset after its window")
	}
	keyed := newKeyedFixedWindowLimiter(1, time.Minute)
	if !keyed.Allow("alice", now) || keyed.Allow("alice", now) || !keyed.Allow("bob", now) {
		t.Fatal("keyed limiter did not isolate usernames")
	}
	if !keyed.Allow("alice", now.Add(time.Minute)) {
		t.Fatal("keyed limiter did not expire old counters")
	}
}

func newTestServer(t *testing.T, config Config) (*Server, *httptest.Server) {
	t.Helper()
	if config.PingInterval == 0 {
		config.PingInterval = time.Hour
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 2 * time.Hour
	}
	server := New(config, nil)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		shutdownTestServer(t, server)
	})
	return server, httpServer
}

func shutdownTestServer(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("shutdown signaling server: %v", err)
	}
}

type testClient struct {
	t         *testing.T
	username  string
	conn      *coderws.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	validator protocol.Validator
}

func connectTestClient(t *testing.T, serverURL, username string) *testClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/ws"
	conn, _, err := coderws.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client := &testClient{t: t, username: username, conn: conn, ctx: ctx, cancel: cancel, validator: protocol.NewValidator()}
	client.send(protocol.SignalSessionHello, "", "", nil)
	if ready := client.read(protocol.SignalSessionReady); ready.To != username {
		t.Fatalf("ready = %+v", ready)
	}
	return client
}

func (c *testClient) message(messageType protocol.SignalType, to, callID string, payload any) protocol.SignalMessage {
	message := protocol.SignalMessage{
		Version: protocol.ProtocolVersion, ID: uuid.NewString(), Type: messageType,
		Timestamp: time.Now().UTC(), CallID: callID, From: c.username, To: to,
	}
	if messageType == protocol.SignalSessionHello {
		message.To = ""
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatal(err)
		}
		message.Payload = encoded
	}
	return message
}

func (c *testClient) send(messageType protocol.SignalType, to, callID string, payload any) {
	c.t.Helper()
	c.sendMessage(c.message(messageType, to, callID, payload))
}

func (c *testClient) sendMessage(message protocol.SignalMessage) {
	c.t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Write(c.ctx, coderws.MessageText, encoded); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) sendRaw(encoded []byte) {
	c.t.Helper()
	if err := c.conn.Write(c.ctx, coderws.MessageText, encoded); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) read(want protocol.SignalType) protocol.SignalMessage {
	c.t.Helper()
	for {
		messageType, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.t.Fatalf("read %s: %v", want, err)
		}
		if messageType != coderws.MessageText {
			continue
		}
		message, err := protocol.DecodeSignal(data, c.validator)
		if err != nil {
			c.t.Fatalf("decode server message: %v", err)
		}
		if message.Type == protocol.SignalSessionPing {
			c.send(protocol.SignalSessionPong, "server", "", nil)
			continue
		}
		if message.Type != want {
			c.t.Fatalf("received %s, want %s: %s", message.Type, want, data)
		}
		return message
	}
}

func (c *testClient) close() {
	_ = c.conn.Close(coderws.StatusNormalClosure, "test complete")
	c.cancel()
}

func assertErrorCode(t *testing.T, message protocol.SignalMessage, want string) {
	t.Helper()
	var payload protocol.ErrorPayload
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != want {
		t.Fatalf("error code = %q, want %q (%s)", payload.Code, want, payload.Message)
	}
}
