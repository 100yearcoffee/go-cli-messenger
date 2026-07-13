package presence

import (
	"errors"
	"sync/atomic"
	"testing"

	"termcall/internal/protocol"
)

type fakeEndpoint struct {
	username string
	closed   atomic.Bool
}

func (f *fakeEndpoint) Username() string                     { return f.username }
func (f *fakeEndpoint) Deliver(protocol.SignalMessage) error { return nil }
func (f *fakeEndpoint) Close()                               { f.closed.Store(true) }

func TestRegistryOwnershipAndClose(t *testing.T) {
	t.Parallel()
	registry := New()
	alice := &fakeEndpoint{username: "alice"}
	duplicate := &fakeEndpoint{username: "alice"}
	if err := registry.Register(alice); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(duplicate); !errors.Is(err, ErrUsernameInUse) {
		t.Fatalf("duplicate registration error = %v", err)
	}
	registry.Unregister(duplicate)
	if got, exists := registry.Get("alice"); !exists || got != alice {
		t.Fatal("non-owner unregistered active endpoint")
	}
	registry.CloseAll()
	if !alice.closed.Load() {
		t.Fatal("CloseAll did not close endpoint")
	}
	registry.Unregister(alice)
	if registry.Len() != 0 {
		t.Fatalf("registry length = %d", registry.Len())
	}
}
