package app

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	serverws "termcall/internal/server/websocket"
)

func TestForegroundClientsExchangeChat(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	server := serverws.New(serverws.Config{
		PingInterval: time.Hour, IdleTimeout: 2 * time.Hour, SweepInterval: 10 * time.Millisecond,
	}, nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			t.Error(err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/ws"
	bobReader, bobWriter := io.Pipe()
	defer bobReader.Close()
	defer bobWriter.Close()
	aliceReader, aliceWriter := io.Pipe()
	defer aliceReader.Close()
	defer aliceWriter.Close()
	var bobOutput, aliceOutput syncBuffer
	bobResult := make(chan error, 1)
	aliceResult := make(chan error, 1)

	go func() {
		bobResult <- RunListen(ctx, Config{
			Username: "bob", ServerURL: serverURL, Input: bobReader,
			Output: &bobOutput, ErrorOutput: &bobOutput,
		})
	}()
	waitForText(t, ctx, &bobOutput, "listening as bob")

	go func() {
		aliceResult <- RunChat(ctx, Config{
			Username: "alice", ServerURL: serverURL, Input: aliceReader,
			Output: &aliceOutput, ErrorOutput: &aliceOutput,
		}, "bob")
	}()
	waitForText(t, ctx, &bobOutput, "Accept? [y/N]")
	writeLine(t, bobWriter, "y")
	waitForText(t, ctx, &aliceOutput, "[system] peer channel open\n")
	waitForText(t, ctx, &bobOutput, "[system] peer channel open\n")

	writeLine(t, aliceWriter, "hello bob")
	waitForTextOrEarlyResult(t, ctx, &aliceOutput, "hello bob <you", aliceResult, bobResult)
	waitForTextOrEarlyResult(t, ctx, &bobOutput, "alice> hello bob", aliceResult, bobResult)
	writeLine(t, bobWriter, "hello alice")
	waitForText(t, ctx, &aliceOutput, "bob> hello alice")
	writeLine(t, aliceWriter, "/status")
	waitForText(t, ctx, &aliceOutput, "route: direct/")
	writeLine(t, aliceWriter, "/quit")

	waitForResult(t, ctx, "alice", aliceResult)
	waitForResult(t, ctx, "bob", bobResult)
	if !strings.Contains(aliceOutput.String(), "chat ended") {
		t.Fatalf("alice output missing clean end:\n%s", aliceOutput.String())
	}
}

func TestInvitationCanBeDeclinedOrCanceled(t *testing.T) {
	for _, test := range []struct {
		name      string
		act       func(*testing.T, io.Writer, io.Writer)
		aliceText string
		bobText   string
	}{
		{
			name:      "declined by listener",
			act:       func(t *testing.T, _ io.Writer, bob io.Writer) { writeLine(t, bob, "n") },
			aliceText: "bob declined", bobText: "declined",
		},
		{
			name:      "canceled by caller",
			act:       func(t *testing.T, alice, _ io.Writer) { writeLine(t, alice, "/quit") },
			aliceText: "invitation canceled", bobText: "invitation is no longer active",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			server := serverws.New(serverws.Config{PingInterval: time.Hour, IdleTimeout: 2 * time.Hour}, nil)
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			defer func() {
				shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
				defer shutdownCancel()
				if err := server.Shutdown(shutdownContext); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			serverURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/ws"
			bobReader, bobWriter := io.Pipe()
			defer bobReader.Close()
			defer bobWriter.Close()
			aliceReader, aliceWriter := io.Pipe()
			defer aliceReader.Close()
			defer aliceWriter.Close()
			var bobOutput, aliceOutput syncBuffer
			bobResult := make(chan error, 1)
			aliceResult := make(chan error, 1)
			go func() {
				bobResult <- RunListen(ctx, Config{Username: "bob", ServerURL: serverURL, Input: bobReader, Output: &bobOutput, ErrorOutput: &bobOutput})
			}()
			waitForText(t, ctx, &bobOutput, "listening as bob")
			go func() {
				aliceResult <- RunChat(ctx, Config{Username: "alice", ServerURL: serverURL, Input: aliceReader, Output: &aliceOutput, ErrorOutput: &aliceOutput}, "bob")
			}()
			waitForText(t, ctx, &bobOutput, "Accept? [y/N]")
			test.act(t, aliceWriter, bobWriter)
			waitForResult(t, ctx, "alice", aliceResult)
			waitForResult(t, ctx, "bob", bobResult)
			if !strings.Contains(aliceOutput.String(), test.aliceText) {
				t.Fatalf("alice output missing %q:\n%s", test.aliceText, aliceOutput.String())
			}
			if !strings.Contains(bobOutput.String(), test.bobText) {
				t.Fatalf("bob output missing %q:\n%s", test.bobText, bobOutput.String())
			}
		})
	}
}

type syncBuffer struct {
	mu sync.RWMutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.b.String()
}

func waitForText(t *testing.T, ctx context.Context, output *syncBuffer, text string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(output.String(), text) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v\noutput:\n%s", text, ctx.Err(), output.String())
		case <-ticker.C:
		}
	}
}

func waitForTextOrEarlyResult(
	t *testing.T,
	ctx context.Context,
	output *syncBuffer,
	text string,
	aliceResult, bobResult <-chan error,
) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(output.String(), text) {
			return
		}
		select {
		case err := <-aliceResult:
			t.Fatalf("alice returned before %q: %v", text, err)
		case err := <-bobResult:
			t.Fatalf("bob returned before %q: %v", text, err)
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v\noutput:\n%s", text, ctx.Err(), output.String())
		case <-ticker.C:
		}
	}
}

func waitForResult(t *testing.T, ctx context.Context, name string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s returned: %v", name, err)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", name, ctx.Err())
	}
}

func writeLine(t *testing.T, writer io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(writer, line+"\n"); err != nil {
		t.Fatal(err)
	}
}
