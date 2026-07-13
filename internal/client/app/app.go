package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"termcall/internal/client/peer"
	"termcall/internal/client/signaling"
	"termcall/internal/client/terminal"
	"termcall/internal/protocol"
)

type Config struct {
	Username    string
	ServerURL   string
	STUNURLs    []string
	Input       io.Reader
	Output      io.Writer
	ErrorOutput io.Writer
}

func RunChat(ctx context.Context, config Config, target string) error {
	if !protocol.ValidUsername(target) {
		return fmt.Errorf("invalid target username %q", target)
	}
	ui := terminal.New(config.Input, config.Output, config.ErrorOutput)
	defer ui.Restore()
	lines := ui.Lines(ctx)
	signals, err := connect(ctx, config)
	if err != nil {
		return err
	}
	defer signals.Close()

	ui.System("locating %s...", target)
	if err := sendSignal(ctx, signals, protocol.SignalPresenceQuery, target, "", nil); err != nil {
		return err
	}
	for {
		message, err := nextSignal(ctx, signals)
		if err != nil {
			return err
		}
		switch message.Type {
		case protocol.SignalPresenceResult:
			var payload protocol.PresencePayload
			if err := json.Unmarshal(message.Payload, &payload); err != nil {
				return fmt.Errorf("decode presence result: %w", err)
			}
			if !payload.Online {
				return fmt.Errorf("%s is offline", target)
			}
			ui.System("%s is online", target)
			goto invite
		case protocol.SignalSessionError:
			return signalingError(message)
		}
	}

invite:
	callID := uuid.NewString()
	if err := sendSignal(ctx, signals, protocol.SignalCallInvite, target, callID, nil); err != nil {
		return err
	}
	ui.System("chat invitation sent")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-signals.Errors():
			return err
		case line, ok := <-lines:
			if !ok || (line.Err == nil && strings.TrimSpace(line.Text) == "/quit") {
				if err := sendSignal(ctx, signals, protocol.SignalCallCancel, target, callID, nil); err != nil {
					return err
				}
				ui.System("invitation canceled")
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			ui.System("waiting for %s; use /quit to cancel", target)
		case message := <-signals.Events():
			if message.CallID != "" && message.CallID != callID {
				continue
			}
			switch message.Type {
			case protocol.SignalCallRinging:
				ui.System("%s is deciding...; use /quit to cancel", target)
			case protocol.SignalCallAccept:
				ui.System("invitation accepted; negotiating peer connection...")
				return runSession(ctx, config, ui, lines, signals, callID, target, peer.RoleCaller)
			case protocol.SignalCallDecline:
				ui.System("%s declined", target)
				return nil
			case protocol.SignalCallBusy:
				return fmt.Errorf("%s is busy", target)
			case protocol.SignalCallTimeout:
				return errors.New("invitation timed out")
			case protocol.SignalSessionError:
				return signalingError(message)
			}
		}
	}
}

func RunListen(ctx context.Context, config Config) error {
	ui := terminal.New(config.Input, config.Output, config.ErrorOutput)
	defer ui.Restore()
	lines := ui.Lines(ctx)
	signals, err := connect(ctx, config)
	if err != nil {
		return err
	}
	defer signals.Close()
	ui.System("listening as %s", config.Username)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-signals.Errors():
			return err
		case message := <-signals.Events():
			switch message.Type {
			case protocol.SignalSessionError:
				return signalingError(message)
			case protocol.SignalCallInvite:
				return answerInvitation(ctx, config, ui, lines, signals, message)
			}
		}
	}
}

func answerInvitation(
	ctx context.Context,
	config Config,
	ui *terminal.UI,
	lines <-chan terminal.Line,
	signals *signaling.Client,
	invite protocol.SignalMessage,
) error {
	ui.System("%s is inviting you to chat", invite.From)
	ui.Prompt("Accept? [y/N] ")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-signals.Errors():
			return err
		case message := <-signals.Events():
			if message.CallID != invite.CallID {
				continue
			}
			switch message.Type {
			case protocol.SignalCallCancel, protocol.SignalCallEnd, protocol.SignalCallTimeout:
				ui.System("invitation is no longer active")
				return nil
			}
		case line, ok := <-lines:
			if !ok {
				_ = sendSignal(ctx, signals, protocol.SignalCallDecline, invite.From, invite.CallID, nil)
				ui.System("input closed; declined")
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			answer := strings.ToLower(strings.TrimSpace(line.Text))
			if answer != "y" && answer != "yes" {
				if err := sendSignal(ctx, signals, protocol.SignalCallDecline, invite.From, invite.CallID, nil); err != nil {
					return err
				}
				ui.System("declined")
				return nil
			}
			if err := sendSignal(ctx, signals, protocol.SignalCallAccept, invite.From, invite.CallID, nil); err != nil {
				return err
			}
			ui.System("accepted; negotiating peer connection...")
			return runSession(ctx, config, ui, lines, signals, invite.CallID, invite.From, peer.RoleCallee)
		}
	}
}

func runSession(
	ctx context.Context,
	config Config,
	ui *terminal.UI,
	lines <-chan terminal.Line,
	signals *signaling.Client,
	callID, remote string,
	role peer.Role,
) (returnErr error) {
	peerConnection, err := peer.New(ctx, role, peer.Config{ICEServers: iceServers(config.STUNURLs)})
	if err != nil {
		return err
	}
	defer peerConnection.Close()
	ended := false
	defer func() {
		if !ended {
			endContext, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = sendSignal(endContext, signals, protocol.SignalCallEnd, remote, callID, nil)
		}
	}()
	if role == peer.RoleCaller {
		if err := peerConnection.Start(); err != nil {
			return err
		}
	}

	controlOpen := false
	route := "unknown"
	var pendingPeerError error
	var peerErrorDeadline <-chan time.Time
	ui.System("type messages after the peer channel opens; /status shows connection details; /quit ends chat")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-signals.Errors():
			return err
		case <-peerErrorDeadline:
			return pendingPeerError
		case outboundSignal := <-peerConnection.Signals():
			if err := forwardPeerSignal(ctx, signals, outboundSignal, remote, callID); err != nil {
				return err
			}
		case message := <-signals.Events():
			if message.CallID != "" && message.CallID != callID {
				continue
			}
			switch message.Type {
			case protocol.SignalWebRTCOffer, protocol.SignalWebRTCAnswer,
				protocol.SignalWebRTCICE, protocol.SignalWebRTCICEComplete:
				peerSignal, err := decodePeerSignal(message)
				if err != nil {
					return err
				}
				if err := peerConnection.ApplySignal(peerSignal); err != nil {
					return err
				}
			case protocol.SignalCallEnd:
				ended = true
				ui.System("%s ended the chat", remote)
				return nil
			case protocol.SignalCallTimeout:
				ended = true
				return errors.New("peer negotiation timed out")
			case protocol.SignalSessionError:
				return signalingError(message)
			}
		case event := <-peerConnection.Events():
			switch event.Type {
			case peer.EventControlOpen:
				controlOpen = true
				if err := peerConnection.Send(ctx, newControl(protocol.ControlPeerHello, protocol.PeerHelloPayload{
					Capabilities: protocol.Capabilities{TextChat: true},
				})); err != nil {
					return err
				}
				ui.System("peer channel open")
			case peer.EventControlMessage:
				switch event.Control.Type {
				case protocol.ControlPeerHello:
					ui.System("text chat ready (%s)", route)
				case protocol.ControlChatMessage:
					var payload protocol.ChatPayload
					if err := json.Unmarshal(event.Control.Payload, &payload); err != nil {
						return err
					}
					ui.Remote(remote, payload.Text)
				case protocol.ControlSessionHangup:
					ui.System("%s hung up", remote)
					if err := sendSignal(ctx, signals, protocol.SignalCallEnd, remote, callID, nil); err == nil {
						ended = true
					}
					return nil
				}
			case peer.EventConnectionState:
				route = event.Route
				switch event.Connection {
				case webrtc.PeerConnectionStateConnected:
					ui.System("connected (%s)", route)
				case webrtc.PeerConnectionStateFailed:
					return errors.New("peer connection failed")
				case webrtc.PeerConnectionStateClosed:
					if pendingPeerError == nil {
						pendingPeerError = errors.New("peer connection closed")
						peerErrorDeadline = time.After(500 * time.Millisecond)
					}
				}
			case peer.EventError:
				if pendingPeerError == nil {
					pendingPeerError = event.Err
					peerErrorDeadline = time.After(500 * time.Millisecond)
				}
			}
		case line, ok := <-lines:
			if !ok {
				ui.System("input closed; ending chat")
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			text := line.Text
			switch text {
			case "":
				continue
			case "/status":
				state, currentRoute := peerConnection.Status()
				ui.System("state: %s; route: %s", state, currentRoute)
			case "/quit":
				if controlOpen {
					_ = peerConnection.Send(ctx, newControl(protocol.ControlSessionHangup, nil))
					flushContext, cancel := context.WithTimeout(ctx, time.Second)
					_ = peerConnection.Flush(flushContext)
					cancel()
				}
				if err := sendSignal(ctx, signals, protocol.SignalCallEnd, remote, callID, nil); err != nil {
					return err
				}
				ended = true
				ui.System("chat ended")
				return nil
			default:
				if !controlOpen {
					ui.System("peer channel is not open yet")
					continue
				}
				if len(text) > protocol.MaxChatTextSize {
					ui.Error("message is too long (maximum %d UTF-8 bytes)", protocol.MaxChatTextSize)
					continue
				}
				if err := peerConnection.Send(ctx, newControl(protocol.ControlChatMessage, protocol.ChatPayload{Text: text})); err != nil {
					return err
				}
				ui.Local(text)
			}
		}
	}
}

func connect(ctx context.Context, config Config) (*signaling.Client, error) {
	return signaling.Connect(ctx, signaling.Config{URL: config.ServerURL, Username: config.Username})
}

func sendSignal(ctx context.Context, client *signaling.Client, messageType protocol.SignalType, to, callID string, payload any) error {
	message, err := client.NewMessage(messageType, to, callID, payload)
	if err != nil {
		return err
	}
	return client.Send(ctx, message)
}

func nextSignal(ctx context.Context, client *signaling.Client) (protocol.SignalMessage, error) {
	select {
	case <-ctx.Done():
		return protocol.SignalMessage{}, ctx.Err()
	case err := <-client.Errors():
		return protocol.SignalMessage{}, err
	case message := <-client.Events():
		return message, nil
	}
}

func newControl(messageType protocol.ControlType, payload any) protocol.ControlMessage {
	message := protocol.ControlMessage{
		Version: protocol.ProtocolVersion, ID: uuid.NewString(), Type: messageType, SentAt: time.Now().UTC(),
	}
	if payload != nil {
		message.Payload, _ = json.Marshal(payload)
	}
	return message
}

func iceServers(urls []string) []webrtc.ICEServer {
	if len(urls) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: urls}}
}

func forwardPeerSignal(ctx context.Context, client *signaling.Client, signal peer.Signal, remote, callID string) error {
	switch {
	case signal.Description != nil:
		messageType := protocol.SignalWebRTCOffer
		if signal.Description.Type == webrtc.SDPTypeAnswer {
			messageType = protocol.SignalWebRTCAnswer
		}
		return sendSignal(ctx, client, messageType, remote, callID, protocol.SDPPayload{SDP: signal.Description.SDP})
	case signal.Candidate != nil:
		return sendSignal(ctx, client, protocol.SignalWebRTCICE, remote, callID, protocol.ICEPayload{
			Candidate: signal.Candidate.Candidate, SDPMid: signal.Candidate.SDPMid,
			SDPMLineIndex: signal.Candidate.SDPMLineIndex,
		})
	case signal.ICEComplete:
		return sendSignal(ctx, client, protocol.SignalWebRTCICEComplete, remote, callID, nil)
	default:
		return peer.ErrUnexpectedSignal
	}
}

func decodePeerSignal(message protocol.SignalMessage) (peer.Signal, error) {
	switch message.Type {
	case protocol.SignalWebRTCOffer, protocol.SignalWebRTCAnswer:
		var payload protocol.SDPPayload
		if err := json.Unmarshal(message.Payload, &payload); err != nil {
			return peer.Signal{}, err
		}
		descriptionType := webrtc.SDPTypeOffer
		if message.Type == protocol.SignalWebRTCAnswer {
			descriptionType = webrtc.SDPTypeAnswer
		}
		description := webrtc.SessionDescription{Type: descriptionType, SDP: payload.SDP}
		return peer.Signal{Description: &description}, nil
	case protocol.SignalWebRTCICE:
		var payload protocol.ICEPayload
		if err := json.Unmarshal(message.Payload, &payload); err != nil {
			return peer.Signal{}, err
		}
		candidate := webrtc.ICECandidateInit{
			Candidate: payload.Candidate, SDPMid: payload.SDPMid, SDPMLineIndex: payload.SDPMLineIndex,
		}
		return peer.Signal{Candidate: &candidate}, nil
	case protocol.SignalWebRTCICEComplete:
		return peer.Signal{ICEComplete: true}, nil
	default:
		return peer.Signal{}, peer.ErrUnexpectedSignal
	}
}

func signalingError(message protocol.SignalMessage) error {
	var payload protocol.ErrorPayload
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		return errors.New("signaling service returned an error")
	}
	return fmt.Errorf("signaling %s: %s", payload.Code, payload.Message)
}
