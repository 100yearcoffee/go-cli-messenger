package calls

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"termcall/internal/protocol"
)

var (
	ErrCallExists        = errors.New("call already exists")
	ErrCallNotFound      = errors.New("call not found")
	ErrUserBusy          = errors.New("user is busy")
	ErrNotParticipant    = errors.New("user is not a call participant")
	ErrInvalidTransition = errors.New("invalid call state transition")
	ErrCandidateLimit    = errors.New("ICE candidate limit exceeded")
)

type State string

const (
	StateRinging     State = "RINGING"
	StateAccepted    State = "ACCEPTED"
	StateNegotiating State = "NEGOTIATING"
	StateConnected   State = "CONNECTED"
	StateDeclined    State = "DECLINED"
	StateCancelled   State = "CANCELLED"
	StateTimedOut    State = "TIMED_OUT"
	StateEnded       State = "ENDED"
)

type Call struct {
	ID               string
	Caller           string
	Callee           string
	State            State
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Deadline         time.Time
	CleanupAt        time.Time
	CallerCandidates int
	CalleeCandidates int
}

func (c Call) Other(username string) (string, error) {
	switch username {
	case c.Caller:
		return c.Callee, nil
	case c.Callee:
		return c.Caller, nil
	default:
		return "", ErrNotParticipant
	}
}

func (c Call) Terminal() bool {
	switch c.State {
	case StateDeclined, StateCancelled, StateTimedOut, StateEnded:
		return true
	default:
		return false
	}
}

type Config struct {
	RingTimeout        time.Duration
	NegotiationTimeout time.Duration
	CleanupAfter       time.Duration
}

type Manager struct {
	mu     sync.Mutex
	config Config
	calls  map[string]*Call
	active map[string]string
}

func New(config Config) *Manager {
	if config.RingTimeout <= 0 {
		config.RingTimeout = 45 * time.Second
	}
	if config.NegotiationTimeout <= 0 {
		config.NegotiationTimeout = 30 * time.Second
	}
	if config.CleanupAfter <= 0 {
		config.CleanupAfter = time.Minute
	}
	return &Manager{
		config: config,
		calls:  make(map[string]*Call),
		active: make(map[string]string),
	}
}

func (m *Manager) Invite(callID, caller, callee string, now time.Time) (Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if caller == callee {
		return Call{}, fmt.Errorf("%w: caller and callee are identical", ErrInvalidTransition)
	}
	if _, exists := m.calls[callID]; exists {
		return Call{}, ErrCallExists
	}
	if _, busy := m.active[caller]; busy {
		return Call{}, fmt.Errorf("%w: %s", ErrUserBusy, caller)
	}
	if _, busy := m.active[callee]; busy {
		return Call{}, fmt.Errorf("%w: %s", ErrUserBusy, callee)
	}
	call := &Call{
		ID: callID, Caller: caller, Callee: callee, State: StateRinging,
		CreatedAt: now, UpdatedAt: now, Deadline: now.Add(m.config.RingTimeout),
	}
	m.calls[callID] = call
	m.active[caller] = callID
	m.active[callee] = callID
	return *call, nil
}

func (m *Manager) Transition(messageType protocol.SignalType, callID, actor, recipient string, now time.Time) (Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call, exists := m.calls[callID]
	if !exists {
		return Call{}, ErrCallNotFound
	}
	other, err := call.Other(actor)
	if err != nil || other != recipient {
		return Call{}, ErrNotParticipant
	}

	switch messageType {
	case protocol.SignalCallAccept:
		if call.State != StateRinging || actor != call.Callee {
			return Call{}, m.transitionError(call, messageType)
		}
		m.setState(call, StateAccepted, now, now.Add(m.config.NegotiationTimeout))
	case protocol.SignalCallDecline:
		if call.State != StateRinging || actor != call.Callee {
			return Call{}, m.transitionError(call, messageType)
		}
		m.finish(call, StateDeclined, now)
	case protocol.SignalCallCancel:
		if call.State != StateRinging || actor != call.Caller {
			return Call{}, m.transitionError(call, messageType)
		}
		m.finish(call, StateCancelled, now)
	case protocol.SignalWebRTCOffer:
		if call.State != StateAccepted || actor != call.Caller {
			return Call{}, m.transitionError(call, messageType)
		}
		m.setState(call, StateNegotiating, now, now.Add(m.config.NegotiationTimeout))
	case protocol.SignalWebRTCAnswer:
		if call.State != StateNegotiating || actor != call.Callee {
			return Call{}, m.transitionError(call, messageType)
		}
		m.setState(call, StateConnected, now, time.Time{})
	case protocol.SignalWebRTCICE, protocol.SignalWebRTCICEComplete:
		if call.State != StateAccepted && call.State != StateNegotiating && call.State != StateConnected {
			return Call{}, m.transitionError(call, messageType)
		}
		if messageType == protocol.SignalWebRTCICE {
			if actor == call.Caller {
				if call.CallerCandidates >= protocol.MaxICECandidates {
					return Call{}, ErrCandidateLimit
				}
				call.CallerCandidates++
			} else {
				if call.CalleeCandidates >= protocol.MaxICECandidates {
					return Call{}, ErrCandidateLimit
				}
				call.CalleeCandidates++
			}
		}
		call.UpdatedAt = now
	case protocol.SignalCallEnd:
		if call.State != StateAccepted && call.State != StateNegotiating && call.State != StateConnected {
			return Call{}, m.transitionError(call, messageType)
		}
		m.finish(call, StateEnded, now)
	default:
		return Call{}, m.transitionError(call, messageType)
	}
	return *call, nil
}

func (m *Manager) Expire(now time.Time) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var expired []Call
	for id, call := range m.calls {
		if call.Terminal() {
			if !call.CleanupAt.IsZero() && !now.Before(call.CleanupAt) {
				delete(m.calls, id)
			}
			continue
		}
		if !call.Deadline.IsZero() && !now.Before(call.Deadline) {
			m.finish(call, StateTimedOut, now)
			expired = append(expired, *call)
		}
	}
	return expired
}

func (m *Manager) Disconnect(username string, now time.Time) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	callID, exists := m.active[username]
	if !exists {
		return nil
	}
	call, exists := m.calls[callID]
	if !exists || call.Terminal() {
		delete(m.active, username)
		return nil
	}
	m.finish(call, StateEnded, now)
	return []Call{*call}
}

func (m *Manager) Get(callID string) (Call, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call, exists := m.calls[callID]
	if !exists {
		return Call{}, false
	}
	return *call, true
}

func (m *Manager) setState(call *Call, state State, now, deadline time.Time) {
	call.State = state
	call.UpdatedAt = now
	call.Deadline = deadline
}

func (m *Manager) finish(call *Call, state State, now time.Time) {
	m.setState(call, state, now, time.Time{})
	call.CleanupAt = now.Add(m.config.CleanupAfter)
	delete(m.active, call.Caller)
	delete(m.active, call.Callee)
}

func (m *Manager) transitionError(call *Call, messageType protocol.SignalType) error {
	return fmt.Errorf("%w: %s from %s", ErrInvalidTransition, messageType, call.State)
}
