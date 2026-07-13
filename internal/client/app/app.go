package app

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"termcall/internal/client/asciivideo"
	"termcall/internal/client/media"
	"termcall/internal/client/media/gstreamer"
	"termcall/internal/client/peer"
	"termcall/internal/client/signaling"
	"termcall/internal/client/terminal"
	clientturn "termcall/internal/client/turn"
	"termcall/internal/identity"
	"termcall/internal/protocol"
)

type Config struct {
	Address      string
	ServerURL    string
	STUNURLs     []string
	Video        bool
	CameraDevice string
	VideoColumns int
	VideoRows    int
	VideoFPS     int
	Audio        bool
	Microphone   string
	Speaker      string
	AudioBitrate int
	AccessKey    string
	Identity     *identity.Identity
	TrustStore   *identity.TrustStore
	ICEServers   []webrtc.ICEServer
	RelayOnly    bool
	// ReconnectTimeout is how long a disrupted peer route may recover before
	// the call is ended. Zero uses the default of 20 seconds.
	ReconnectTimeout time.Duration
	Input            io.Reader
	Output           io.Writer
	ErrorOutput      io.Writer
}

var peerReplays identity.ReplayGuard

// SignalSession is the signaling transport used by an interactive call. The
// daemon's local handoff implements the same contract as the direct WebSocket
// client, allowing one authenticated server connection per user.
type SignalSession interface {
	NewMessage(protocol.SignalType, string, string, any) (protocol.SignalMessage, error)
	Send(context.Context, protocol.SignalMessage) error
	Events() <-chan protocol.SignalMessage
	Errors() <-chan error
	STUNURLs() []string
	Close()
}

func RunChat(ctx context.Context, config Config, target string) error {
	if !protocol.ValidAddress(target) {
		return fmt.Errorf("invalid target canonical address %q", target)
	}
	ui := terminal.New(config.Input, config.Output, config.ErrorOutput)
	defer ui.Restore()
	lines := ui.Lines(ctx)
	signals, err := connect(ctx, config)
	if err != nil {
		return err
	}
	defer signals.Close()
	if len(config.STUNURLs) == 0 {
		config.STUNURLs = signals.STUNURLs()
	}

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
	var invitePayload any
	if config.Identity != nil {
		invitePayload, err = config.Identity.Sign(protocol.SignalCallInvite, callID, config.Address, target, time.Now())
		if err != nil {
			return err
		}
	}
	if err := sendSignal(ctx, signals, protocol.SignalCallInvite, target, callID, invitePayload); err != nil {
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
				remoteKey, record, err := verifyAndObserve(config, message)
				if err != nil {
					return fmt.Errorf("reject unauthenticated acceptance: %w", err)
				}
				showTrust(ui, record)
				showSafetyCode(ui, config.Identity, remoteKey, callID)
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
	if len(config.STUNURLs) == 0 {
		config.STUNURLs = signals.STUNURLs()
	}
	ui.System("listening as %s", config.Address)

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

// RunIncoming displays a ringing invitation received through the background
// daemon and starts media only after the user explicitly accepts it.
func RunIncoming(ctx context.Context, config Config, signals SignalSession, invite protocol.SignalMessage) error {
	ui := terminal.New(config.Input, config.Output, config.ErrorOutput)
	defer ui.Restore()
	defer signals.Close()
	if len(config.STUNURLs) == 0 {
		config.STUNURLs = signals.STUNURLs()
	}
	return answerInvitation(ctx, config, ui, ui.Lines(ctx), signals, invite)
}

// DeclineIncoming declines a daemon-delivered invitation without opening media.
func DeclineIncoming(ctx context.Context, signals SignalSession, invite protocol.SignalMessage) error {
	defer signals.Close()
	return sendSignal(ctx, signals, protocol.SignalCallDecline, invite.From, invite.CallID, nil)
}

func answerInvitation(
	ctx context.Context,
	config Config,
	ui *terminal.UI,
	lines <-chan terminal.Line,
	signals SignalSession,
	invite protocol.SignalMessage,
) error {
	remoteKey, record, err := verifyAndObserve(config, invite)
	if err != nil {
		_ = sendSignal(ctx, signals, protocol.SignalCallDecline, invite.From, invite.CallID, nil)
		return fmt.Errorf("reject unauthenticated invitation: %w", err)
	}
	if record.Blocked {
		_ = sendSignal(ctx, signals, protocol.SignalCallDecline, invite.From, invite.CallID, nil)
		return fmt.Errorf("blocked identity %s", record.Fingerprint)
	}
	showTrust(ui, record)
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
			var acceptPayload any
			if config.Identity != nil {
				acceptPayload, err = config.Identity.Sign(protocol.SignalCallAccept, invite.CallID, config.Address, invite.From, time.Now())
				if err != nil {
					return err
				}
			}
			if err := sendSignal(ctx, signals, protocol.SignalCallAccept, invite.From, invite.CallID, acceptPayload); err != nil {
				return err
			}
			showSafetyCode(ui, config.Identity, remoteKey, invite.CallID)
			ui.System("accepted; negotiating peer connection...")
			return runSession(ctx, config, ui, lines, signals, invite.CallID, invite.From, peer.RoleCallee)
		}
	}
}

func verifyAndObserve(config Config, message protocol.SignalMessage) (ed25519.PublicKey, identity.TrustRecord, error) {
	proof, publicKey, fingerprint, err := identity.Verify(message, time.Now())
	if err != nil {
		return nil, identity.TrustRecord{}, err
	}
	if !peerReplays.Accept(fingerprint, proof.Nonce, proof.ExpiresAt, time.Now()) {
		return nil, identity.TrustRecord{}, errors.New("replayed identity proof")
	}
	store := config.TrustStore
	if store == nil {
		store, err = identity.OpenTrustStore()
	}
	if err != nil {
		return nil, identity.TrustRecord{}, err
	}
	record, reused, err := store.Observe(config.ServerURL, message.From, publicKey)
	if err != nil {
		return nil, identity.TrustRecord{}, err
	}
	if len(reused) != 0 {
		output := config.ErrorOutput
		if output == nil {
			output = io.Discard
		}
		fmt.Fprintf(output, "WARNING: alias %s was previously observed with fingerprint %s; treating %s as UNKNOWN\n", message.From, strings.Join(reused, ", "), fingerprint)
	}
	return publicKey, record, nil
}

func showTrust(ui *terminal.UI, record identity.TrustRecord) {
	if record.Trusted {
		ui.System("TRUSTED identity %s", record.Fingerprint)
		if len(record.Aliases) > 1 {
			previous := record.Aliases[:len(record.Aliases)-1]
			ui.System("previously seen under %d other server/address alias(es)", len(previous))
		}
		return
	}
	ui.System("UNKNOWN identity %s", record.Fingerprint)
}

func showSafetyCode(ui *terminal.UI, device *identity.Device, remoteKey ed25519.PublicKey, callID string) {
	if device == nil || len(remoteKey) == 0 {
		return
	}
	ui.System("SECURITY CODE: %s", identity.SafetyCode(device.PublicKey, remoteKey, callID))
}

func runSession(
	ctx context.Context,
	config Config,
	ui *terminal.UI,
	lines <-chan terminal.Line,
	signals SignalSession,
	callID, remote string,
	role peer.Role,
) (returnErr error) {
	if err := prepareICEServers(ctx, &config); err != nil {
		return err
	}
	peerConnection, err := peer.New(ctx, role, peer.Config{ICEServers: config.ICEServers, Audio: config.Audio, RelayOnly: config.RelayOnly})
	if err != nil {
		return err
	}
	defer peerConnection.Close()
	var microphone *gstreamer.AudioInput
	var audioPackets <-chan *rtp.Packet
	var speaker *gstreamer.AudioOutput
	muted := false
	if config.Audio {
		microphone = new(gstreamer.AudioInput)
		audioPackets, err = microphone.Start(ctx, media.AudioConfig{Device: config.Microphone, Bitrate: config.AudioBitrate})
		if err != nil {
			microphone = nil
			ui.Error("microphone unavailable: %v", err)
		}
		speaker = new(gstreamer.AudioOutput)
		if err := speaker.Start(ctx, media.AudioConfig{Device: config.Speaker}, peerConnection.AudioPackets()); err != nil {
			speaker = nil
			ui.Error("speaker unavailable: %v", err)
		}
	}
	defer func() {
		if microphone != nil {
			_ = microphone.Close()
		}
		if speaker != nil {
			_ = speaker.Close()
		}
	}()
	ended := false
	if role == peer.RoleCaller {
		if err := peerConnection.Start(); err != nil {
			return err
		}
	}

	controlOpen := false
	route := "unknown"
	localVideoCapabilities := localCapabilities(config)
	if localVideoCapabilities.ASCIIVideo {
		localVideoCapabilities.ASCIIColumns, localVideoCapabilities.ASCIIRows = ui.VideoSize(
			localVideoCapabilities.ASCIIColumns, localVideoCapabilities.ASCIIRows,
		)
	}
	videoDecoder := new(asciivideo.Decoder)
	var videoEncoder *asciivideo.Encoder
	var videoFrames <-chan media.VideoFrame
	var camera *gstreamer.Camera
	keyframeRequested := false
	defer func() {
		if camera != nil {
			_ = camera.Close()
		}
	}()
	var pendingPeerError error
	var peerErrorDeadline <-chan time.Time
	reconnectTimeout := config.ReconnectTimeout
	if reconnectTimeout <= 0 {
		reconnectTimeout = 20 * time.Second
	}
	var reconnectTimer *time.Timer
	var reconnectDeadline <-chan time.Time
	restartAttempted := false
	signalEvents := signals.Events()
	signalErrors := signals.Errors()
	peerSignals := peerConnection.Signals()
	signalingAvailable := true
	defer func() {
		if !ended && signalingAvailable {
			endContext, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = sendSignal(endContext, signals, protocol.SignalCallEnd, remote, callID, nil)
		}
	}()
	stopReconnectTimer := func() {
		if reconnectTimer != nil {
			if !reconnectTimer.Stop() {
				select {
				case <-reconnectTimer.C:
				default:
				}
			}
			reconnectTimer = nil
			reconnectDeadline = nil
		}
	}
	startReconnectTimer := func() {
		stopReconnectTimer()
		reconnectTimer = time.NewTimer(reconnectTimeout)
		reconnectDeadline = reconnectTimer.C
	}
	defer stopReconnectTimer()
	ui.System("type messages after the peer channel opens; /mute toggles the microphone; /status shows connection details; /quit ends chat")
	if config.RelayOnly {
		ui.System("ICE policy: relay only")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-signalErrors:
			if !ok || err == nil {
				err = errors.New("signaling connection closed")
			}
			if !controlOpen {
				return err
			}
			// Once the encrypted peer channel is open, signaling is no longer on
			// the content path. A signaling restart must not tear down a healthy
			// P2P call; peer hangup remains available on the control channel.
			signalingAvailable = false
			signalErrors = nil
			signalEvents = nil
			peerSignals = nil
			ui.Error("signaling connection lost; peer session continues: %v", err)
		case <-peerErrorDeadline:
			return pendingPeerError
		case <-reconnectDeadline:
			return errors.New("peer connection did not recover before the reconnect timeout")
		case frame, ok := <-videoFrames:
			if !ok {
				videoFrames = nil
				ui.Error("camera capture stopped")
				continue
			}
			cells, err := asciivideo.FromGray(frame.Pixels, frame.Width, frame.Height, frame.Width, frame.Height/2)
			if err != nil {
				ui.Error("convert camera frame: %v", err)
				continue
			}
			encoded, err := videoEncoder.Encode(cells, frame.Timestamp)
			if err != nil {
				ui.Error("encode camera frame: %v", err)
				continue
			}
			_ = peerConnection.SendVideo(encoded)
		case packet, ok := <-audioPackets:
			if !ok {
				audioPackets = nil
				ui.Error("microphone capture stopped")
				continue
			}
			if !muted {
				if err := peerConnection.SendAudio(packet); err != nil && !errors.Is(err, peer.ErrClosed) {
					ui.Error("send microphone audio: %v", err)
				}
			}
		case outboundSignal := <-peerSignals:
			if err := forwardPeerSignal(ctx, signals, outboundSignal, remote, callID); err != nil {
				return err
			}
		case videoData := <-peerConnection.VideoFrames():
			frame, err := videoDecoder.Decode(videoData)
			if errors.Is(err, asciivideo.ErrNeedsKeyframe) {
				if controlOpen && !keyframeRequested {
					keyframeRequested = true
					_ = peerConnection.Send(ctx, newControl(protocol.ControlVideoKeyframe, nil))
				}
				continue
			}
			if err != nil {
				ui.Error("discarding invalid ASCII video frame: %v", err)
				continue
			}
			if frame.Type == asciivideo.FrameKey {
				keyframeRequested = false
			}
			ui.RenderVideo(frame.Columns, frame.Rows, frame.Cells)
		case message, ok := <-signalEvents:
			if !ok {
				if !controlOpen {
					return errors.New("signaling connection closed during negotiation")
				}
				signalingAvailable = false
				signalEvents = nil
				signalErrors = nil
				peerSignals = nil
				ui.Error("signaling connection closed; peer session continues")
				continue
			}
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
					Capabilities: localVideoCapabilities,
				})); err != nil {
					return err
				}
				ui.System("peer channel open")
			case peer.EventControlMessage:
				switch event.Control.Type {
				case protocol.ControlPeerHello:
					var hello protocol.PeerHelloPayload
					if err := json.Unmarshal(event.Control.Payload, &hello); err != nil {
						return err
					}
					if config.Video && hello.Capabilities.ASCIIVideo && videoFrames == nil && camera == nil {
						columns := min(localVideoCapabilities.ASCIIColumns, hello.Capabilities.ASCIIColumns)
						rows := min(localVideoCapabilities.ASCIIRows, hello.Capabilities.ASCIIRows)
						fps := min(localVideoCapabilities.ASCIIFPS, hello.Capabilities.ASCIIFPS)
						videoEncoder, err = asciivideo.NewEncoder(columns, rows, 2*time.Second)
						if err != nil {
							return err
						}
						camera = new(gstreamer.Camera)
						videoFrames, err = camera.Open(ctx, media.VideoConfig{
							Device: config.CameraDevice, Columns: columns, Rows: rows, FPS: fps,
						})
						if err != nil {
							camera = nil
							ui.Error("camera unavailable: %v", err)
						} else {
							ui.System("ASCII video enabled: %dx%d @ %d FPS", columns, rows, fps)
						}
					}
					ui.System("text chat ready (%s)", route)
				case protocol.ControlChatMessage:
					var payload protocol.ChatPayload
					if err := json.Unmarshal(event.Control.Payload, &payload); err != nil {
						return err
					}
					ui.Remote(remote, payload.Text)
				case protocol.ControlSessionHangup:
					ui.System("%s hung up", remote)
					if !signalingAvailable {
						ended = true
					} else if err := sendSignal(ctx, signals, protocol.SignalCallEnd, remote, callID, nil); err == nil {
						ended = true
					}
					return nil
				case protocol.ControlVideoKeyframe:
					if videoEncoder != nil {
						videoEncoder.ForceKeyframe()
					}
				case protocol.ControlAudioState:
					var state protocol.AudioStatePayload
					if err := json.Unmarshal(event.Control.Payload, &state); err != nil {
						return err
					}
					if state.Muted {
						ui.System("%s muted their microphone", remote)
					} else {
						ui.System("%s unmuted their microphone", remote)
					}
				}
			case peer.EventAudioOpen:
				ui.System("remote audio ready: Opus 48 kHz mono")
			case peer.EventConnectionState:
				route = event.Route
				switch event.Connection {
				case webrtc.PeerConnectionStateConnected:
					stopReconnectTimer()
					restartAttempted = false
					ui.System("connected (%s)", route)
				case webrtc.PeerConnectionStateDisconnected:
					if reconnectTimer == nil {
						startReconnectTimer()
						ui.System("peer route interrupted; waiting up to %s for recovery", reconnectTimeout)
					}
				case webrtc.PeerConnectionStateFailed:
					if reconnectTimer == nil {
						startReconnectTimer()
					}
					if role == peer.RoleCaller && signalingAvailable && !restartAttempted {
						restartAttempted = true
						if err := peerConnection.RestartICE(); err != nil {
							return fmt.Errorf("restart ICE: %w", err)
						}
						ui.System("peer connection failed; gathering a fresh ICE route")
					} else {
						ui.System("peer connection failed; waiting for ICE recovery")
					}
				case webrtc.PeerConnectionStateClosed:
					if pendingPeerError == nil {
						pendingPeerError = errors.New("peer connection closed")
						peerErrorDeadline = time.After(500 * time.Millisecond)
					}
				}
			case peer.EventError:
				if errors.Is(event.Err, peer.ErrVideoChannel) {
					// Video is an optional, lossy channel. Its failure must not tear
					// down a healthy control/audio session.
					ui.Error("ASCII video unavailable: %v", event.Err)
					continue
				}
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
				audioState := "off"
				if config.Audio {
					audioState = "on"
					if muted {
						audioState = "muted"
					}
				}
				ui.System("state: %s; route: %s; audio: %s", state, currentRoute, audioState)
			case "/mute", "m":
				if !config.Audio {
					ui.System("audio is disabled")
					continue
				}
				muted = !muted
				if err := peerConnection.Send(ctx, newControl(protocol.ControlAudioState, protocol.AudioStatePayload{Muted: muted})); err != nil {
					return err
				}
				if muted {
					ui.System("microphone muted")
				} else {
					ui.System("microphone unmuted")
				}
			case "/quit":
				if controlOpen {
					_ = peerConnection.Send(ctx, newControl(protocol.ControlSessionHangup, nil))
					flushContext, cancel := context.WithTimeout(ctx, time.Second)
					_ = peerConnection.Flush(flushContext)
					cancel()
					// Give the reliable SCTP stream a brief chance to deliver the
					// hangup before the deferred PeerConnection close aborts it.
					drain := time.NewTimer(100 * time.Millisecond)
					select {
					case <-drain.C:
					case <-ctx.Done():
						if !drain.Stop() {
							<-drain.C
						}
					}
				}
				if signalingAvailable {
					if err := sendSignal(ctx, signals, protocol.SignalCallEnd, remote, callID, nil); err != nil {
						return err
					}
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
	return signaling.Connect(ctx, signaling.Config{URL: config.ServerURL, Address: config.Address, AccessKey: config.AccessKey, Identity: config.Identity})
}

func sendSignal(ctx context.Context, client SignalSession, messageType protocol.SignalType, to, callID string, payload any) error {
	message, err := client.NewMessage(messageType, to, callID, payload)
	if err != nil {
		return err
	}
	return client.Send(ctx, message)
}

func nextSignal(ctx context.Context, client SignalSession) (protocol.SignalMessage, error) {
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

func prepareICEServers(ctx context.Context, config *Config) error {
	config.ICEServers = iceServers(config.STUNURLs)
	if config.AccessKey != "" {
		credentials, err := clientturn.Credentials(ctx, config.ServerURL, config.AccessKey)
		if err != nil {
			var apiError *clientturn.APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != 404 {
				return fmt.Errorf("obtain TURN credentials: %w", err)
			}
		} else {
			if !credentials.ExpiresAt.After(time.Now()) {
				return errors.New("signaling service returned expired TURN credentials")
			}
			for _, server := range credentials.ICEServers {
				turnURLs := make([]string, 0, len(server.URLs))
				for _, value := range server.URLs {
					if protocol.ValidTURNURL(value) {
						turnURLs = append(turnURLs, value)
					}
				}
				if len(turnURLs) == 0 {
					continue
				}
				if server.Username == "" || server.Credential == "" {
					return errors.New("signaling service returned incomplete TURN credentials")
				}
				config.ICEServers = append(config.ICEServers, webrtc.ICEServer{
					URLs: turnURLs, Username: server.Username, Credential: server.Credential,
					CredentialType: webrtc.ICECredentialTypePassword,
				})
			}
		}
	}
	if config.RelayOnly {
		for _, server := range config.ICEServers {
			for _, value := range server.URLs {
				if protocol.ValidTURNURL(value) {
					return nil
				}
			}
		}
		return errors.New("relay-only mode requires TURN credentials from the signaling service")
	}
	return nil
}

func forwardPeerSignal(ctx context.Context, client SignalSession, signal peer.Signal, remote, callID string) error {
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

func localCapabilities(config Config) protocol.Capabilities {
	capabilities := protocol.Capabilities{TextChat: true, ASCIIVideo: config.Video, Audio: config.Audio}
	if !config.Video {
		return capabilities
	}
	capabilities.ASCIIColumns = config.VideoColumns
	capabilities.ASCIIRows = config.VideoRows
	capabilities.ASCIIFPS = config.VideoFPS
	if capabilities.ASCIIColumns < 1 || capabilities.ASCIIColumns > asciivideo.MaxColumns {
		capabilities.ASCIIColumns = 100
	}
	if capabilities.ASCIIRows < 1 || capabilities.ASCIIRows > asciivideo.MaxRows {
		capabilities.ASCIIRows = 34
	}
	if capabilities.ASCIIFPS < 1 || capabilities.ASCIIFPS > 30 {
		capabilities.ASCIIFPS = 15
	}
	return capabilities
}
