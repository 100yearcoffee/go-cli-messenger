package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	clientsignaling "termcall/internal/client/signaling"
	"termcall/internal/identity"
	"termcall/internal/protocol"
	"termcall/internal/server/access"
	turncredentials "termcall/internal/server/turn"
)

const testAccessKey = "correct horse battery staple"

func TestAccessGateHealthAndRemovedAccountRoutes(t *testing.T) {
	gate, _ := access.New([]byte(testAccessKey))
	var logs lockedBuffer
	server := New(Config{Access: gate}, slog.New(slog.NewJSONHandler(&logs, nil)))
	httpServer := httptest.NewServer(server.Handler())
	defer shutdownTestServer(t, server, httpServer)
	response, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", response.StatusCode)
	}
	response, err = http.Post(httpServer.URL+"/v1/users/register", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("account route = %d", response.StatusCode)
	}
	value, address := testIdentity(t, "alice")
	if _, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: address, Identity: value}); err == nil {
		t.Fatal("missing access key accepted")
	}
	if _, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: address, Identity: value, AccessKey: strings.Repeat("x", 24)}); err == nil {
		t.Fatal("wrong access key accepted")
	}
	client, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: address, Identity: value, AccessKey: testAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if strings.Contains(logs.String(), testAccessKey) {
		t.Fatal("access key appeared in logs")
	}
}

type lockedBuffer struct {
	mu    sync.Mutex
	value bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.Write(value)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.value.String() }

func TestTURNCredentialsUseAccessGateAndRandomSubject(t *testing.T) {
	gate, _ := access.New([]byte(testAccessKey))
	issuer, err := turncredentials.New(turncredentials.Config{Secret: []byte(strings.Repeat("s", 32)), URLs: []string{"turn:turn.example:3478"}})
	if err != nil {
		t.Fatal(err)
	}
	server := New(Config{Access: gate, TURN: issuer}, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer shutdownTestServer(t, server, httpServer)
	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/v1/turn-credentials", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key = %d", response.StatusCode)
	}
	var usernames []string
	for range 2 {
		request, _ = http.NewRequest(http.MethodGet, httpServer.URL+"/v1/turn-credentials", nil)
		request.Header.Set("Authorization", "Bearer "+testAccessKey)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var credentials protocol.TURNCredentials
		if err := json.NewDecoder(response.Body).Decode(&credentials); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(credentials.ICEServers) != 1 || credentials.ICEServers[0].Credential == "" {
			t.Fatalf("credentials = %+v", credentials)
		}
		usernames = append(usernames, credentials.ICEServers[0].Username)
	}
	if usernames[0] == usernames[1] {
		t.Fatal("TURN allocation subjects were reused")
	}
}

func TestSignedHandshakeInviteAcceptAndReplayProtection(t *testing.T) {
	server := New(Config{PingInterval: time.Hour, IdleTimeout: 2 * time.Hour}, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer shutdownTestServer(t, server, httpServer)
	aliceID, aliceAddress := testIdentity(t, "alice")
	bobID, bobAddress := testIdentity(t, "bob")
	alice, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: aliceAddress, Identity: aliceID})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: bobAddress, Identity: bobID})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	callID := uuid.NewString()
	invite, err := alice.NewMessage(protocol.SignalCallInvite, bobAddress, callID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(context.Background(), invite); err != nil {
		t.Fatal(err)
	}
	received := waitSignal(t, bob, protocol.SignalCallInvite)
	if _, _, fingerprint, err := identity.Verify(received, time.Now()); err != nil || fingerprint != identity.Fingerprint(aliceID.PublicKey) {
		t.Fatalf("invite proof = %s, %v", fingerprint, err)
	}
	accept, err := bob.NewMessage(protocol.SignalCallAccept, aliceAddress, callID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.Send(context.Background(), accept); err != nil {
		t.Fatal(err)
	}
	_ = waitSignal(t, alice, protocol.SignalCallRinging)
	_ = waitSignal(t, alice, protocol.SignalCallAccept)

	replayed := invite
	replayed.ID = uuid.NewString()
	replayed.Timestamp = time.Now().UTC()
	if err := alice.Send(context.Background(), replayed); err != nil {
		t.Fatal(err)
	}
	errorMessage := waitSignal(t, alice, protocol.SignalSessionError)
	var payload protocol.ErrorPayload
	_ = json.Unmarshal(errorMessage.Payload, &payload)
	if payload.Code != "unauthorized" {
		t.Fatalf("replay code = %q", payload.Code)
	}
}

func TestCopiedAddressCannotHandshakeAndV1IsRejected(t *testing.T) {
	server := New(Config{}, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer shutdownTestServer(t, server, httpServer)
	owner, address := testIdentity(t, "alice")
	impostor, _ := testIdentity(t, "mallory")
	if _, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: address, Identity: impostor}); err == nil {
		t.Fatal("copied address connected")
	}
	conn, _, err := websocket.Dial(context.Background(), wsURL(httpServer.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := protocol.SignalMessage{Version: 1, ID: uuid.NewString(), Type: protocol.SignalSessionHello, Timestamp: time.Now(), From: address}
	proof, _ := owner.Sign(protocol.SignalSessionHello, "", address, "", time.Now())
	hello.Payload, _ = json.Marshal(proof)
	encoded, _ := json.Marshal(hello)
	if err := conn.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("v1 handshake accepted")
	}
}

func TestShortSuffixCollisionIsRejected(t *testing.T) {
	server := New(Config{}, nil)
	value, address := testIdentity(t, "alice")
	fingerprint := identity.Fingerprint(value.PublicKey)
	server.suffixes[fingerprint[:protocol.AddressSuffixLength]] = strings.Repeat("a", protocol.FingerprintLength)
	httpServer := httptest.NewServer(server.Handler())
	defer shutdownTestServer(t, server, httpServer)
	if _, err := clientsignaling.Connect(context.Background(), clientsignaling.Config{URL: wsURL(httpServer.URL), Address: address, Identity: value}); err == nil {
		t.Fatal("short-suffix collision connected")
	}
}

func testIdentity(t *testing.T, name string) (*identity.Identity, string) {
	t.Helper()
	value, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	address, err := identity.CanonicalAddress(name, value.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return value, address
}
func waitSignal(t *testing.T, client *clientsignaling.Client, wanted protocol.SignalType) protocol.SignalMessage {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case message := <-client.Events():
			if message.Type == wanted {
				return message
			}
		case err := <-client.Errors():
			t.Fatal(err)
		case <-timeout.C:
			t.Fatalf("waiting for %s", wanted)
		}
	}
}
func wsURL(value string) string { return "ws" + strings.TrimPrefix(value, "http") + "/v1/ws" }
func shutdownTestServer(t *testing.T, server *Server, httpServer *httptest.Server) {
	t.Helper()
	httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Error(err)
	}
}
