package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"termcall/internal/protocol"
)

const (
	ControlChannelLabel = "control"
	defaultQueueSize    = 32
)

var (
	ErrClosed             = errors.New("peer is closed")
	ErrInvalidRole        = errors.New("invalid peer role")
	ErrUnexpectedSignal   = errors.New("unexpected WebRTC signal")
	ErrInvalidDataChannel = errors.New("invalid control data channel")
)

type Role uint8

const (
	RoleCaller Role = iota + 1
	RoleCallee
)

type EventType uint8

const (
	EventControlOpen EventType = iota + 1
	EventControlMessage
	EventConnectionState
	EventError
)

// Signal contains WebRTC negotiation data. Exactly one field is set.
// The signaling transport introduced in the next phase will serialize these
// values into the shared signaling protocol.
type Signal struct {
	Description *webrtc.SessionDescription
	Candidate   *webrtc.ICECandidateInit
	ICEComplete bool
}

type Event struct {
	Type       EventType
	Control    protocol.ControlMessage
	Connection webrtc.PeerConnectionState
	Route      string
	Err        error
}

type Config struct {
	ICEServers []webrtc.ICEServer
	QueueSize  int
	Validator  protocol.Validator
}

// Peer owns one Pion PeerConnection and its reliable, ordered control channel.
type Peer struct {
	role      Role
	pc        *webrtc.PeerConnection
	validator protocol.Validator

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	signals  chan Signal
	events   chan Event
	outbound chan outboundMessage
	ready    chan struct{}

	channelMu sync.RWMutex
	channel   *webrtc.DataChannel
	readyOnce sync.Once

	remoteMu          sync.Mutex
	remoteDescription bool
	pendingCandidates []webrtc.ICECandidateInit
	seenMu            sync.Mutex
	seenIDs           map[string]struct{}
	seenOrder         []string
	seenLimit         int

	closeOnce sync.Once
	wg        sync.WaitGroup
	statusMu  sync.RWMutex
	state     webrtc.PeerConnectionState
	route     string
}

type outboundMessage struct {
	data  []byte
	flush chan struct{}
}

func New(ctx context.Context, role Role, config Config) (*Peer, error) {
	if role != RoleCaller && role != RoleCallee {
		return nil, ErrInvalidRole
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queueSize := config.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	validator := config.Validator
	if validator.Now == nil {
		validator = protocol.NewValidator()
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: config.ICEServers})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	peerContext, cancel := context.WithCancel(ctx)
	p := &Peer{
		role:      role,
		pc:        pc,
		validator: validator,
		ctx:       peerContext,
		cancel:    cancel,
		done:      make(chan struct{}),
		signals:   make(chan Signal, queueSize),
		events:    make(chan Event, queueSize),
		outbound:  make(chan outboundMessage, queueSize),
		ready:     make(chan struct{}),
		seenIDs:   make(map[string]struct{}, queueSize*4),
		seenLimit: queueSize * 4,
	}
	p.installPeerHandlers()

	if role == RoleCaller {
		ordered := true
		channel, createErr := pc.CreateDataChannel(ControlChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
		if createErr != nil {
			cancel()
			_ = pc.Close()
			return nil, fmt.Errorf("create control data channel: %w", createErr)
		}
		if err := p.attachControlChannel(channel); err != nil {
			cancel()
			_ = pc.Close()
			return nil, err
		}
	} else {
		pc.OnDataChannel(func(channel *webrtc.DataChannel) {
			if err := p.attachControlChannel(channel); err != nil {
				p.emitEvent(Event{Type: EventError, Err: err})
			}
		})
	}

	p.wg.Add(1)
	go p.sendLoop()
	go func() {
		<-peerContext.Done()
		_ = p.Close()
	}()

	return p, nil
}

// Start creates the caller's offer. The callee starts when it receives that offer.
func (p *Peer) Start() error {
	if p.role != RoleCaller {
		return fmt.Errorf("%w: only the caller creates an offer", ErrUnexpectedSignal)
	}
	if err := p.contextError(); err != nil {
		return err
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local offer: %w", err)
	}
	local := p.pc.LocalDescription()
	if local == nil {
		return errors.New("local offer was not set")
	}
	return p.emitSignal(Signal{Description: cloneDescription(local)})
}

// ApplySignal applies an offer, answer, ICE candidate, or end-of-candidates marker.
func (p *Peer) ApplySignal(signal Signal) error {
	if err := p.contextError(); err != nil {
		return err
	}
	fields := 0
	if signal.Description != nil {
		fields++
	}
	if signal.Candidate != nil {
		fields++
	}
	if signal.ICEComplete {
		fields++
	}
	if fields != 1 {
		return fmt.Errorf("%w: signal must contain exactly one value", ErrUnexpectedSignal)
	}
	if signal.ICEComplete {
		return nil
	}
	if signal.Candidate != nil {
		return p.applyCandidate(*signal.Candidate)
	}
	return p.applyDescription(*signal.Description)
}

// Send validates and queues a control message. A full queue applies backpressure
// until the caller's context is canceled or capacity becomes available.
func (p *Peer) Send(ctx context.Context, message protocol.ControlMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.validator.ValidateControl(message); err != nil {
		return fmt.Errorf("validate control message: %w", err)
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	if len(encoded) > protocol.MaxControlMessageSize {
		return protocol.ErrMessageTooLarge
	}
	select {
	case p.outbound <- outboundMessage{data: encoded}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	}
}

// Flush waits until all messages queued before it have been handed to Pion.
func (p *Peer) Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	flushed := make(chan struct{})
	select {
	case p.outbound <- outboundMessage{flush: flushed}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	}
	select {
	case <-flushed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	}
}

func (p *Peer) Status() (webrtc.PeerConnectionState, string) {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.state, p.route
}

func (p *Peer) Signals() <-chan Signal { return p.signals }
func (p *Peer) Events() <-chan Event   { return p.events }
func (p *Peer) Done() <-chan struct{}  { return p.done }

func (p *Peer) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		p.cancel()
		closeErr = p.pc.Close()
		p.wg.Wait()
		close(p.done)
	})
	return closeErr
}

func (p *Peer) installPeerHandlers() {
	p.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			_ = p.emitSignal(Signal{ICEComplete: true})
			return
		}
		value := candidate.ToJSON()
		_ = p.emitSignal(Signal{Candidate: &value})
	})
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		route := p.selectedRoute()
		p.statusMu.Lock()
		p.state = state
		if route != "unknown" {
			p.route = route
		}
		currentRoute := p.route
		p.statusMu.Unlock()
		if currentRoute == "" {
			currentRoute = "unknown"
		}
		p.emitEvent(Event{Type: EventConnectionState, Connection: state, Route: currentRoute})
	})
}

func (p *Peer) attachControlChannel(channel *webrtc.DataChannel) error {
	if channel.Label() != ControlChannelLabel || !channel.Ordered() ||
		channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		return fmt.Errorf("%w: label=%q ordered=%t", ErrInvalidDataChannel, channel.Label(), channel.Ordered())
	}

	p.channelMu.Lock()
	if p.channel != nil && p.channel != channel {
		p.channelMu.Unlock()
		return fmt.Errorf("%w: duplicate control channel", ErrInvalidDataChannel)
	}
	p.channel = channel
	p.channelMu.Unlock()

	channel.OnOpen(func() {
		p.readyOnce.Do(func() { close(p.ready) })
		p.emitEvent(Event{Type: EventControlOpen})
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		control, err := protocol.DecodeControl(message.Data, p.validator)
		if err != nil {
			p.emitEvent(Event{Type: EventError, Err: fmt.Errorf("decode control message: %w", err)})
			return
		}
		if p.seen(control.ID) {
			return
		}
		p.emitEvent(Event{Type: EventControlMessage, Control: control})
	})
	channel.OnError(func(err error) {
		p.emitEvent(Event{Type: EventError, Err: fmt.Errorf("control data channel: %w", err)})
	})
	return nil
}

func (p *Peer) sendLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case outbound := <-p.outbound:
			if outbound.flush != nil {
				close(outbound.flush)
				continue
			}
			select {
			case <-p.ready:
			case <-p.ctx.Done():
				return
			}
			p.channelMu.RLock()
			channel := p.channel
			p.channelMu.RUnlock()
			if channel == nil {
				p.emitEvent(Event{Type: EventError, Err: ErrInvalidDataChannel})
				continue
			}
			if err := channel.Send(outbound.data); err != nil {
				p.emitEvent(Event{Type: EventError, Err: fmt.Errorf("send control message: %w", err)})
			}
		}
	}
}

func (p *Peer) selectedRoute() string {
	sctp := p.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return "unknown"
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return "unknown"
	}
	kind := "direct"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		kind = "relay"
	}
	protocolName := pair.Local.Protocol.String()
	if protocolName == "" {
		protocolName = pair.Remote.Protocol.String()
	}
	if protocolName == "" {
		protocolName = "unknown"
	}
	return kind + "/" + protocolName
}

func (p *Peer) applyDescription(description webrtc.SessionDescription) error {
	switch description.Type {
	case webrtc.SDPTypeOffer:
		if p.role != RoleCallee {
			return fmt.Errorf("%w: caller received an offer", ErrUnexpectedSignal)
		}
	case webrtc.SDPTypeAnswer:
		if p.role != RoleCaller {
			return fmt.Errorf("%w: callee received an answer", ErrUnexpectedSignal)
		}
	default:
		return fmt.Errorf("%w: unsupported description type %s", ErrUnexpectedSignal, description.Type)
	}

	if err := p.pc.SetRemoteDescription(description); err != nil {
		return fmt.Errorf("set remote %s: %w", description.Type, err)
	}
	if err := p.markRemoteDescriptionAndFlushCandidates(); err != nil {
		return err
	}

	if description.Type == webrtc.SDPTypeOffer {
		answer, err := p.pc.CreateAnswer(nil)
		if err != nil {
			return fmt.Errorf("create answer: %w", err)
		}
		if err := p.pc.SetLocalDescription(answer); err != nil {
			return fmt.Errorf("set local answer: %w", err)
		}
		local := p.pc.LocalDescription()
		if local == nil {
			return errors.New("local answer was not set")
		}
		if err := p.emitSignal(Signal{Description: cloneDescription(local)}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Peer) applyCandidate(candidate webrtc.ICECandidateInit) error {
	p.remoteMu.Lock()
	if !p.remoteDescription {
		p.pendingCandidates = append(p.pendingCandidates, candidate)
		p.remoteMu.Unlock()
		return nil
	}
	p.remoteMu.Unlock()
	if err := p.pc.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("add ICE candidate: %w", err)
	}
	return nil
}

func (p *Peer) markRemoteDescriptionAndFlushCandidates() error {
	p.remoteMu.Lock()
	p.remoteDescription = true
	pending := p.pendingCandidates
	p.pendingCandidates = nil
	p.remoteMu.Unlock()

	for _, candidate := range pending {
		if err := p.pc.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("add pending ICE candidate: %w", err)
		}
	}
	return nil
}

func (p *Peer) emitSignal(signal Signal) error {
	select {
	case p.signals <- signal:
		return nil
	case <-p.ctx.Done():
		return ErrClosed
	}
}

func (p *Peer) emitEvent(event Event) {
	select {
	case p.events <- event:
	case <-p.ctx.Done():
	}
}

func (p *Peer) contextError() error {
	select {
	case <-p.ctx.Done():
		return ErrClosed
	default:
		return nil
	}
}

func (p *Peer) seen(id string) bool {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	if _, exists := p.seenIDs[id]; exists {
		return true
	}
	p.seenIDs[id] = struct{}{}
	p.seenOrder = append(p.seenOrder, id)
	if len(p.seenOrder) > p.seenLimit {
		oldest := p.seenOrder[0]
		p.seenOrder = p.seenOrder[1:]
		delete(p.seenIDs, oldest)
	}
	return false
}

func cloneDescription(description *webrtc.SessionDescription) *webrtc.SessionDescription {
	copy := *description
	return &copy
}
