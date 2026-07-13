package websocket

import (
	"sync"
	"time"
)

type fixedWindowLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
}

type windowCounter struct {
	start time.Time
	count int
}

type keyedFixedWindowLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]windowCounter
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{limit: limit, window: window}
}

func newKeyedFixedWindowLimiter(limit int, window time.Duration) *keyedFixedWindowLimiter {
	return &keyedFixedWindowLimiter{limit: limit, window: window, counters: make(map[string]windowCounter)}
}

func (l *keyedFixedWindowLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for existingKey, counter := range l.counters {
		if !now.Before(counter.start.Add(l.window)) {
			delete(l.counters, existingKey)
		}
	}
	counter := l.counters[key]
	if counter.start.IsZero() {
		counter.start = now
	}
	if counter.count >= l.limit {
		l.counters[key] = counter
		return false
	}
	counter.count++
	l.counters[key] = counter
	return true
}

func (l *fixedWindowLimiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() || !now.Before(l.start.Add(l.window)) {
		l.start = now
		l.count = 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}
