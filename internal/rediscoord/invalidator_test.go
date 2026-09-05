package rediscoord

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestInvalidatorBroadcast(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()

	inv := NewInvalidator(rc, "fastagent:test:reload")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloaded := make(chan string, 1)
	go inv.Run(ctx, func(userID string) {
		select {
		case reloaded <- userID:
		default:
		}
	})

	// Give the subscriber a moment to attach, then publish.
	time.Sleep(50 * time.Millisecond)
	if err := inv.PublishReloadFor(ctx, "u1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case got := <-reloaded:
		if got != "u1" {
			t.Fatalf("reload user = %q, want u1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replica never received reload broadcast")
	}
}

func TestInvalidatorNilClientIsNoop(t *testing.T) {
	inv := NewInvalidator(nil, "ch")
	if err := inv.PublishReloadFor(context.Background(), "u1"); err != nil {
		t.Fatalf("nil client publish must be a no-op, got %v", err)
	}
}
