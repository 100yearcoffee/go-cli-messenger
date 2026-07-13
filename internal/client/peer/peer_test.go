package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	pionturn "github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"

	"termcall/internal/protocol"
)

func TestPeersExchangeControlMessagesInOrder(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	caller, err := New(ctx, RoleCaller, Config{QueueSize: 16, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	callee, err := New(ctx, RoleCallee, Config{QueueSize: 16, Audio: true})
	if err != nil {
		_ = caller.Close()
		t.Fatal(err)
	}
	defer closePeers(t, caller, callee)

	bridgeErrors := bridgePeers(ctx, caller, callee)
	if err := caller.Start(); err != nil {
		t.Fatalf("start caller: %v", err)
	}
	waitForChannels(t, ctx, caller, bridgeErrors)
	waitForChannels(t, ctx, callee, bridgeErrors)

	video := []byte("versioned-ascii-frame")
	if sent := caller.SendVideo(video); !sent {
		t.Fatal("caller did not queue video frame")
	}
	if got := waitForVideo(t, ctx, callee, bridgeErrors); string(got) != string(video) {
		t.Fatalf("video frame = %q, want %q", got, video)
	}
	audio := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 1, Timestamp: 960, SSRC: 1234}, Payload: []byte{0xf8, 0xff, 0xfe}}
	if err := caller.SendAudio(audio); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	if got := waitForAudio(t, ctx, callee, bridgeErrors); got.SequenceNumber != audio.SequenceNumber || string(got.Payload) != string(audio.Payload) {
		t.Fatalf("audio packet = %#v, want %#v", got, audio)
	}
	replyAudio := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 2, Timestamp: 1920, SSRC: 5678}, Payload: []byte{0xf8, 0xff, 0xfe}}
	if err := callee.SendAudio(replyAudio); err != nil {
		t.Fatalf("send reply audio: %v", err)
	}
	if got := waitForAudio(t, ctx, caller, bridgeErrors); got.SequenceNumber != replyAudio.SequenceNumber || string(got.Payload) != string(replyAudio.Payload) {
		t.Fatalf("reply audio packet = %#v, want %#v", got, replyAudio)
	}

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

func TestPeersConnectThroughTURNOnly(t *testing.T) {
	const username, password, realm = "relay-test", "relay-password", "termcall.test"
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *pionturn.RequestAttributes) (string, []byte, bool) {
			if attributes.Username != username {
				return "", nil, false
			}
			return username, pionturn.GenerateAuthKey(username, realm, password), true
		},
		PacketConnConfigs: []pionturn.PacketConnConfig{{
			PacketConn: listener,
			RelayAddressGenerator: &pionturn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1",
			},
		}},
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	defer server.Close()

	iceServer := webrtc.ICEServer{
		URLs:     []string{"turn:" + listener.LocalAddr().String() + "?transport=udp"},
		Username: username, Credential: password, CredentialType: webrtc.ICECredentialTypePassword,
	}
	assertTURNOnlyConnection(t, iceServer, "relay/udp")
}

func TestPeersConnectThroughTURNOnlyTCP(t *testing.T) {
	const username, password, realm = "relay-test", "relay-password", "termcall.test"
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *pionturn.RequestAttributes) (string, []byte, bool) {
			if attributes.Username != username {
				return "", nil, false
			}
			return username, pionturn.GenerateAuthKey(username, realm, password), true
		},
		ListenerConfigs: []pionturn.ListenerConfig{{
			Listener: listener,
			RelayAddressGenerator: &pionturn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1",
			},
		}},
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	defer server.Close()
	iceServer := webrtc.ICEServer{
		URLs:     []string{"turn:" + listener.Addr().String() + "?transport=tcp"},
		Username: username, Credential: password, CredentialType: webrtc.ICECredentialTypePassword,
	}
	assertTURNOnlyConnection(t, iceServer, "relay/tcp")
}

func assertTURNOnlyConnection(t *testing.T, iceServer webrtc.ICEServer, wantRoute string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	caller, err := New(ctx, RoleCaller, Config{ICEServers: []webrtc.ICEServer{iceServer}, RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	callee, err := New(ctx, RoleCallee, Config{ICEServers: []webrtc.ICEServer{iceServer}, RelayOnly: true})
	if err != nil {
		_ = caller.Close()
		t.Fatal(err)
	}
	defer closePeers(t, caller, callee)
	bridgeErrors := bridgePeers(ctx, caller, callee)
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	waitForChannels(t, ctx, caller, bridgeErrors)
	waitForChannels(t, ctx, callee, bridgeErrors)
	if _, route := caller.Status(); route != wantRoute {
		t.Fatalf("caller route = %q, want %s", route, wantRoute)
	}
	if _, route := callee.Status(); route != wantRoute {
		t.Fatalf("callee route = %q, want %s", route, wantRoute)
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

func TestQueueLatestDropsStaleVideoFrame(t *testing.T) {
	t.Parallel()
	queue := make(chan []byte, 1)
	if !queueLatest(queue, []byte("stale")) || !queueLatest(queue, []byte("newest")) {
		t.Fatal("latest-frame queue rejected a frame")
	}
	if got := string(<-queue); got != "newest" {
		t.Fatalf("waiting frame = %q, want newest", got)
	}
	if len(queue) != 0 {
		t.Fatal("latest-frame queue retained more than one frame")
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
	if err := callee.RestartICE(); !errors.Is(err, ErrUnexpectedSignal) {
		t.Fatalf("callee RestartICE error = %v, want ErrUnexpectedSignal", err)
	}
	if err := callee.ApplySignal(Signal{}); !errors.Is(err, ErrUnexpectedSignal) {
		t.Fatalf("empty signal error = %v, want ErrUnexpectedSignal", err)
	}
	invalid := controlMessage(t, "0191bdb0-0000-7000-8000-000000000201", protocol.ControlChatMessage, protocol.ChatPayload{Text: ""})
	if err := callee.Send(context.Background(), invalid); !errors.Is(err, protocol.ErrInvalidMessage) {
		t.Fatalf("invalid control error = %v, want ErrInvalidMessage", err)
	}
}

func TestCallerRestartsICEWithoutReplacingChannels(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	caller, err := New(ctx, RoleCaller, Config{})
	if err != nil {
		t.Fatal(err)
	}
	callee, err := New(ctx, RoleCallee, Config{})
	if err != nil {
		_ = caller.Close()
		t.Fatal(err)
	}
	defer closePeers(t, caller, callee)
	bridgeErrors := bridgePeers(ctx, caller, callee)
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	waitForChannels(t, ctx, caller, bridgeErrors)
	waitForChannels(t, ctx, callee, bridgeErrors)
	if err := caller.RestartICE(); err != nil {
		t.Fatalf("restart ICE: %v", err)
	}

	message := controlMessage(t, "0191bdb0-0000-7000-8000-000000000250", protocol.ControlChatMessage, protocol.ChatPayload{Text: "after restart"})
	mustSend(t, ctx, caller, message)
	if got := waitForControl(t, ctx, callee, bridgeErrors); got.ID != message.ID {
		t.Fatalf("message after restart = %q, want %q", got.ID, message.ID)
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

func waitForChannels(t *testing.T, ctx context.Context, peer *Peer, bridgeErrors <-chan error) {
	t.Helper()
	controlOpen, videoOpen := false, false
	for {
		if controlOpen && videoOpen {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for data channels: %v", ctx.Err())
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case event := <-peer.Events():
			switch event.Type {
			case EventControlOpen:
				controlOpen = true
			case EventVideoOpen:
				videoOpen = true
			case EventError:
				t.Fatalf("peer event: %v", event.Err)
			}
		}
	}
}

func waitForVideo(t *testing.T, ctx context.Context, peer *Peer, bridgeErrors <-chan error) []byte {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for video frame: %v", ctx.Err())
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case frame := <-peer.VideoFrames():
			return frame
		case event := <-peer.Events():
			if event.Type == EventError {
				t.Fatalf("peer event: %v", event.Err)
			}
		}
	}
}

func waitForAudio(t *testing.T, ctx context.Context, peer *Peer, bridgeErrors <-chan error) *rtp.Packet {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for audio packet: %v", ctx.Err())
		case err := <-bridgeErrors:
			t.Fatalf("signaling bridge: %v", err)
		case packet := <-peer.AudioPackets():
			return packet
		case event := <-peer.Events():
			if event.Type == EventError {
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
