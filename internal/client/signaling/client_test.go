package signaling

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"termcall/internal/protocol"
	serverws "termcall/internal/server/websocket"
)

func TestClientHandshakePresenceAndHeartbeat(t *testing.T) {
	server := serverws.New(serverws.Config{
		PingInterval: 10 * time.Millisecond, IdleTimeout: 250 * time.Millisecond,
		SweepInterval: time.Second,
	}, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, Config{URL: websocketURL(httpServer.URL), Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Remaining connected beyond the idle timeout proves automatic pong handling.
	time.Sleep(350 * time.Millisecond)
	query, err := client.NewMessage(protocol.SignalPresenceQuery, "bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, query); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-client.Events():
		if message.Type != protocol.SignalPresenceResult {
			t.Fatalf("received %s, want presence.result", message.Type)
		}
	case err := <-client.Errors():
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	if _, err := Connect(ctx, Config{URL: websocketURL(httpServer.URL), Username: "alice"}); err == nil {
		t.Fatal("duplicate username connection succeeded")
	}
}

func websocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/v1/ws"
}
