package presence

import (
	"errors"
	"sync"

	"termcall/internal/protocol"
)

var ErrUsernameInUse = errors.New("username is already connected")

type Endpoint interface {
	Username() string
	Deliver(protocol.SignalMessage) error
	Close()
}

type Registry struct {
	mu    sync.RWMutex
	users map[string]Endpoint
}

func New() *Registry {
	return &Registry{users: make(map[string]Endpoint)}
}

func (r *Registry) Register(endpoint Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	username := endpoint.Username()
	if _, exists := r.users[username]; exists {
		return ErrUsernameInUse
	}
	r.users[username] = endpoint
	return nil
}

func (r *Registry) Get(username string) (Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	endpoint, exists := r.users[username]
	return endpoint, exists
}

func (r *Registry) Unregister(endpoint Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, exists := r.users[endpoint.Username()]; exists && current == endpoint {
		delete(r.users, endpoint.Username())
	}
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}

func (r *Registry) CloseAll() {
	r.mu.RLock()
	endpoints := make([]Endpoint, 0, len(r.users))
	for _, endpoint := range r.users {
		endpoints = append(endpoints, endpoint)
	}
	r.mu.RUnlock()
	for _, endpoint := range endpoints {
		endpoint.Close()
	}
}
