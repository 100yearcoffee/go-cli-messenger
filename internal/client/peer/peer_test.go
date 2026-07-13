package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"termcall/internal/protocol"
)

func TestPeersExchangeControlMessagesInOrder(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	caller, err := New(ctx, RoleCaller, Config{QueueSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	callee, err := New(ctx, RoleCallee, Config{QueueSize: 16})
	if err != nil {
		_ = caller.Close()
		t.Fatal(err)
	}
	defer closePeers(t, caller, callee)

	bridgeErrors := bridgePeers(ctx, caller, callee)
	if err := caller.Start(); err != nil {
		t.Fatalf("start caller: %v", err)
	}
	waitForOpen(t, ctx, caller, bridgeErrors)
	waitForOpen(t, ctx, callee, bridgeErrors)

	callerHello := controlMessage(t, "0191bdb0-0000-7000-8000-000000000001", protocol.ControlPeerHello, protocol.PeerHelloPayload{
		Capabilities: protocol.Capabilities{TextChat: true},
	})
	calleeHello := controlMessage(t, "0191bdb0-0000-7000-8000-000000000002", protocol.ControlPeerHello, protocol.PeerHelloPayload{
		Capabilities: protocol.Capabilities{TextChat: true},
	})
	mustSend(t, ctx, caller, callerHello)
	mustSend(t, ctx, callee, calleeHello)
	if got := waitForControl(t, ctx, callee, bridgeErrors); got.Type != protocol.ControlPeerHello {
		t.Fatalf("callee received %s, want peer.hello", got.Type)
	}
	if got := waitForControl(t, ctx, caller, bridgeErrors); got.Type != protocol.ControlPeerHello {
		t.Fatalf("caller received %s, want peer.hello", got.Type)
	}

	want := []string{"first", "second", "third"}
	for index, text := range want {
		message := controlMessage(t, fmt.Sprintf("0191bdb0-0000-7000-8000-%012d", index+10), protocol.ControlChatMessage, protocol.ChatPayload{Text: text})
		mustSend(t, ctx, caller, message)
	}
	for index, text := range want {
		got := waitForControl(t, ctx, callee, bridgeErrors)
		var payload protocol.ChatPayload
		if err := json.Unmarshal(got.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Text != text {
			t.Fatalf("message %d = %q, want %q", index, payload.Text, text)
		}
	}

	reply := controlMessage(t, "0191bdb0-0000-7000-8000-000000000020", protocol.ControlChatMessage, protocol.ChatPayload{Text: "reply"})
	mustSend(t, ctx, callee, reply)
	if got := waitForControl(t, ctx, caller, bridgeErrors); got.ID != reply.ID {
		t.Fatalf("caller received ID %q, want %q", got.ID, reply.ID)
	}

	// Re-sending the same message ID is safely ignored by the receiver.
	mustSend(t, ctx, callee, reply)
	assertNoControl(t, caller, 150*time.Millisecond, bridgeErrors)

	hangup := controlMessage(t, "0191bdb0-0000-7000-8000-000000000030", protocol.ControlSessionHangup, nil)
	mustSend(t, ctx, callee, hangup)
	if got := waitForControl(t, ctx, caller, bridgeErrors); got.Type != protocol.ControlSessionHangup {
		t.Fatalf("caller received %s, want session.hangup", got.Type)
	}
}

func TestSendAppliesBoundedBackpressureBeforeConnection(t *testing.T) {
	t.Parallel()

	peer, err := New(context.Background(), RoleCaller, Config{QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closePeers(t, peer)

	first := controlMessage(t, "0191bdb0-0000-7000-8000-000000000101", protocol.ControlChatMessage, protocol.ChatPayload{Text: "first"})
	second := controlMessage(t, "0191bdb0-0000-7000-8000-000000000102", protocol.ControlChatMessage, protocol.ChatPayload{Text: "second"})
	third := controlMessage(t, "0191bdb0-0000-7000-8000-000000000103", protocol.ControlChatMessage, protocol.ChatPayload{Text: "third"})
	mustSend(t, context.Background(), peer, first)

	deadline := time.Now().Add(time.Second)
	for len(peer.outbound) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	mustSend(t, context.Background(), peer, second)

	sendContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := peer.Send(sendContext, third); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third Send error = %v, want context deadline", err)
	}
}

func TestPeerRejectsUnexpectedSignalsAndInvalidMessages(t *testing.T) {
	t.Parallel()

	callee, err := New(context.Background(), RoleCallee, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closePeers(t, callee)

	if err := callee.Start(); !errors.Is(err, ErrUnexpectedSignal) {
		t.Fatalf("callee Start error = %v, want ErrUnexpectedSignal", err)
	}
	if err := callee.ApplySignal(Signal{}); !errors.Is(err, ErrUnexpectedSignal) {
		t.Fatalf("empty signal error = %v, want ErrUnexpectedSignal", err)
	}
	invalid := controlMessage(t, "0191bdb0-0000-7000-8000-000000000201", protocol.ControlChatMessage, protocol.ChatPayload{Text: ""})
	if err := callee.Send(context.Background(), invalid); !errors.Is(err, protocol.ErrInvalidMessage) {
		t.Fatalf("invalid control error = %v, want ErrInvalidMessage", err)
	}
}

func bridgePeers(ctx context.Context, caller, callee *Peer) <-chan error {
	errorsChannel := make(chan error, 2)
	bridge := func(source, destination *Peer) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-source.Done():
				return
			case signal := <-source.Signals():
				if err := destination.ApplySignal(signal); err != nil && !errors.Is(err, ErrClosed) {
					select {
					case errorsChannel <- err:
					default:
					}
					return
				}
			}
		}
	}
	go bridge(caller, callee)
	go bridge(callee, caller)
	return errorsChannel
}

func waitForOpen(t *testing.T, ctx context.Context, peer *Peer, bridgeErrors <-chan error) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for control channel: %v", ctx.Err())
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case event := <-peer.Events():
			switch event.Type {
			case EventControlOpen:
				return
			case EventError:
				t.Fatalf("peer event: %v", event.Err)
			}
		}
	}
}

func waitForControl(t *testing.T, ctx context.Context, peer *Peer, bridgeErrors <-chan error) protocol.ControlMessage {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for control message: %v", ctx.Err())
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case event := <-peer.Events():
			switch event.Type {
			case EventControlMessage:
				return event.Control
			case EventError:
				t.Fatalf("peer event: %v", event.Err)
			}
		}
	}
}

func assertNoControl(t *testing.T, peer *Peer, duration time.Duration, bridgeErrors <-chan error) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case event := <-peer.Events():
			if event.Type == EventControlMessage {
				t.Fatalf("unexpected duplicate control message %s", event.Control.ID)
			}
			if event.Type == EventError {
				t.Fatalf("peer event: %v", event.Err)
			}
		}
	}
}

func controlMessage(t *testing.T, id string, messageType protocol.ControlType, payload any) protocol.ControlMessage {
	t.Helper()
	message := protocol.ControlMessage{
		Version: protocol.ProtocolVersion,
		ID:      id,
		Type:    messageType,
		SentAt:  time.Now().UTC(),
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		message.Payload = encoded
	}
	return message
}

func mustSend(t *testing.T, ctx context.Context, peer *Peer, message protocol.ControlMessage) {
	t.Helper()
	if err := peer.Send(ctx, message); err != nil {
		t.Fatalf("send %s: %v", message.Type, err)
	}
}

func closePeers(t *testing.T, peers ...*Peer) {
	t.Helper()
	var wait sync.WaitGroup
	for _, item := range peers {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := item.Close(); err != nil {
				t.Errorf("close peer: %v", err)
			}
			select {
			case <-item.Done():
			case <-time.After(2 * time.Second):
				t.Errorf("peer did not finish closing")
			}
		}()
	}
	wait.Wait()
}
