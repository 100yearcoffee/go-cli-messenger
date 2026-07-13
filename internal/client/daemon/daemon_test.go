package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForReconnectIsCancelable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := waitForReconnect(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("canceled reconnect wait did not return promptly")
	}
}

func TestWaitForReconnectDelay(t *testing.T) {
	t.Parallel()
	started := time.Now()
	if err := waitForReconnect(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 8*time.Millisecond {
		t.Fatal("reconnect wait returned before its delay")
	}
}
