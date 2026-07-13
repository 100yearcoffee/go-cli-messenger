package calls

import (
	"errors"
	"testing"
	"time"

	"termcall/internal/protocol"
)

func TestCallLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	manager := New(Config{})
	call, err := manager.Invite("call-1", "alice", "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	if call.State != StateRinging {
		t.Fatalf("initial state = %s", call.State)
	}
	if _, err := manager.Invite("call-2", "charlie", "bob", now); !errors.Is(err, ErrUserBusy) {
		t.Fatalf("busy invite error = %v", err)
	}
	if _, err := manager.Transition(protocol.SignalCallAccept, call.ID, "alice", "bob", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("caller accept error = %v", err)
	}
	call, err = manager.Transition(protocol.SignalCallAccept, call.ID, "bob", "alice", now)
	if err != nil || call.State != StateAccepted {
		t.Fatalf("accept = (%s, %v)", call.State, err)
	}
	call, err = manager.Transition(protocol.SignalWebRTCOffer, call.ID, "alice", "bob", now)
	if err != nil || call.State != StateNegotiating {
		t.Fatalf("offer = (%s, %v)", call.State, err)
	}
	if _, err := manager.Transition(protocol.SignalWebRTCICE, call.ID, "bob", "alice", now); err != nil {
		t.Fatalf("ICE: %v", err)
	}
	call, err = manager.Transition(protocol.SignalWebRTCAnswer, call.ID, "bob", "alice", now)
	if err != nil || call.State != StateConnected {
		t.Fatalf("answer = (%s, %v)", call.State, err)
	}
	call, err = manager.Transition(protocol.SignalCallEnd, call.ID, "alice", "bob", now)
	if err != nil || call.State != StateEnded || !call.Terminal() {
		t.Fatalf("end = (%s, %v)", call.State, err)
	}
	if _, err := manager.Invite("call-2", "charlie", "bob", now); err != nil {
		t.Fatalf("busy marker not released: %v", err)
	}
}

func TestExpiryCleanupAndDisconnect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	manager := New(Config{RingTimeout: time.Second, CleanupAfter: time.Second})
	call, err := manager.Invite("call-timeout", "alice", "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	if expired := manager.Expire(now.Add(500 * time.Millisecond)); len(expired) != 0 {
		t.Fatalf("call expired early: %+v", expired)
	}
	expired := manager.Expire(now.Add(time.Second))
	if len(expired) != 1 || expired[0].State != StateTimedOut {
		t.Fatalf("expired = %+v", expired)
	}
	manager.Expire(now.Add(2 * time.Second))
	if _, exists := manager.Get(call.ID); exists {
		t.Fatal("terminal call was not cleaned up")
	}

	call, err = manager.Invite("call-disconnect", "alice", "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	ended := manager.Disconnect("bob", now)
	if len(ended) != 1 || ended[0].State != StateEnded {
		t.Fatalf("disconnect result = %+v", ended)
	}
}

func TestCandidateLimit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	manager := New(Config{})
	call, err := manager.Invite("candidate-call", "alice", "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Transition(protocol.SignalCallAccept, call.ID, "bob", "alice", now); err != nil {
		t.Fatal(err)
	}
	for range protocol.MaxICECandidates {
		if _, err := manager.Transition(protocol.SignalWebRTCICE, call.ID, "alice", "bob", now); err != nil {
			t.Fatalf("candidate rejected early: %v", err)
		}
	}
	if _, err := manager.Transition(protocol.SignalWebRTCICE, call.ID, "alice", "bob", now); !errors.Is(err, ErrCandidateLimit) {
		t.Fatalf("candidate overflow error = %v", err)
	}
}

func TestOutOfOrderTransitionsAreRejected(t *testing.T) {
	t.Parallel()
	now := time.Now()
	manager := New(Config{})
	call, err := manager.Invite("out-of-order", "alice", "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		messageType protocol.SignalType
		actor       string
		recipient   string
	}{
		{protocol.SignalWebRTCOffer, "alice", "bob"},
		{protocol.SignalWebRTCAnswer, "bob", "alice"},
		{protocol.SignalCallEnd, "alice", "bob"},
	} {
		if _, err := manager.Transition(transition.messageType, call.ID, transition.actor, transition.recipient, now); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s error = %v, want ErrInvalidTransition", transition.messageType, err)
		}
	}
	if current, _ := manager.Get(call.ID); current.State != StateRinging {
		t.Fatalf("invalid transitions changed state to %s", current.State)
	}
}
