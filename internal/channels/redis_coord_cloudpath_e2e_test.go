package channels_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/rediscoord"
	"github.com/redis/go-redis/v9"
)

// redisCoordE2EClient boots an in-memory Redis (miniredis) and returns a
// go-redis client wired to it plus the miniredis handle (needed to advance
// the clock deterministically for TTL-expiry assertions). These e2e tests
// exercise the exact production path #10 added: a real Redis round-trip
// for channel leases and bus delivery.
func redisCoordE2EClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc, s
}

// fakeRedisChannel is a Channel implementation that records when Start is
// invoked and captures every outbound message, so the e2e can assert what
// the Manager actually routed.
type fakeRedisChannel struct {
	name      string
	accountID string
	started   chan struct{}
	sends     chan bus.OutboundMessage
}

func (f *fakeRedisChannel) Name() string         { return f.name }
func (f *fakeRedisChannel) AccountID() string    { return f.accountID }
func (f *fakeRedisChannel) BotUsername() string  { return "" }
func (f *fakeRedisChannel) Send(chatID, text string) error { return nil }
func (f *fakeRedisChannel) SendTyping(chatID string) error { return nil }

func (f *fakeRedisChannel) Start(ctx context.Context) error {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}

func (f *fakeRedisChannel) SendMessage(msg bus.OutboundMessage) error {
	select {
	case f.sends <- msg:
	default:
	}
	return nil
}

// TestRedisLeaser_AcquireRenewRelease_CloudPath verifies the rediscoord
// Leaser's cross-process singleton semantics against a real Redis. The
// gateway wires this leaser when FASTAGENT_REDIS_ENABLED is set, so this
// is the Cloud-deploy path for multi-replica IM bots.
func TestRedisLeaser_AcquireRenewRelease_CloudPath(t *testing.T) {
	rc, _ := redisCoordE2EClient(t)
	ctx := context.Background()
	l := rediscoord.NewLeaser(rc, "fastagent-e2e")

	// First holder acquires the (telegram, botA) lease.
	ok, err := l.Acquire(ctx, "telegram", "botA", "holder-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire holder-a: ok=%v err=%v", ok, err)
	}
	// Second holder must be refused while A owns the lease.
	ok, err = l.Acquire(ctx, "telegram", "botA", "holder-b", time.Minute)
	if err != nil || ok {
		t.Fatalf("Acquire holder-b while A holds: ok=%v err=%v", ok, err)
	}
	// Holder A renews — still owns it.
	ok, err = l.Renew(ctx, "telegram", "botA", "holder-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Renew holder-a: ok=%v err=%v", ok, err)
	}
	// Holder B cannot renew a lease it doesn't own.
	ok, err = l.Renew(ctx, "telegram", "botA", "holder-b", time.Minute)
	if err != nil || ok {
		t.Fatalf("Renew holder-b: ok=%v err=%v", ok, err)
	}
	// Release lets a peer take over immediately.
	if err := l.Release(ctx, "telegram", "botA", "holder-a"); err != nil {
		t.Fatalf("Release holder-a: %v", err)
	}
	ok, err = l.Acquire(ctx, "telegram", "botA", "holder-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire holder-b after release: ok=%v err=%v", ok, err)
	}
}

// TestRedisLeaser_TTLExpiry_CloudPath verifies that when the holder stops
// renewing, the lease expires and a peer can take over — the crash-recovery
// path that keeps one pod from blocking a bot forever. FastForward advances
// miniredis's clock deterministically past the TTL (miniredis does not
// expire keys in real-time).
func TestRedisLeaser_TTLExpiry_CloudPath(t *testing.T) {
	rc, s := redisCoordE2EClient(t)
	ctx := context.Background()
	l := rediscoord.NewLeaser(rc, "fastagent-e2e")

	ok, err := l.Acquire(ctx, "wechat", "botX", "holder-a", 100*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	// Let the TTL lapse — no renewal.
	s.FastForward(200 * time.Millisecond)
	ok, err = l.Renew(ctx, "wechat", "botX", "holder-a", time.Minute)
	if err != nil || ok {
		t.Fatalf("Renew after TTL lapse should fail: ok=%v err=%v", ok, err)
	}
	// Peer acquires the expired lease.
	ok, err = l.Acquire(ctx, "wechat", "botX", "holder-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire after expiry: ok=%v err=%v", ok, err)
	}
}

// TestRedisBus_StreamRoundTrip_CloudPath verifies the Redis Streams bridge
// delivers IM inbound/outbound across the bus. The fork's channels manager
// now consumes OutboundConsumer(), so this asserts the modified delivery
// path (not the process-local web/api shortcut).
func TestRedisBus_StreamRoundTrip_CloudPath(t *testing.T) {
	rc, _ := redisCoordE2EClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mb := bus.NewRedis(bus.RedisConfig{Client: rc, Prefix: "fastagent-e2e", Group: "e2e-g", Consumer: "e2e-c"})
	if err := mb.Start(ctx); err != nil {
		t.Fatalf("bus.Start: %v", err)
	}

	// Inbound: telegram messages ride the shared stream.
	mb.Inbound <- bus.InboundMessage{Channel: "telegram", AccountID: "botA", ChatID: "c1", Text: "hi", UserID: "u1"}
	select {
	case got := <-mb.InboundConsumer():
		if got.Text != "hi" || got.ChatID != "c1" {
			t.Fatalf("inbound round-trip mismatch: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inbound never delivered via redis stream")
	}

	// Outbound: IM replies ride the shared stream.
	mb.Outbound <- bus.OutboundMessage{Channel: "telegram", AccountID: "botA", ChatID: "c1", Text: "reply"}
	select {
	case got := <-mb.OutboundConsumer():
		if got.Text != "reply" || got.ChatID != "c1" {
			t.Fatalf("outbound round-trip mismatch: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("outbound never delivered via redis stream")
	}
}

// TestManager_RedisBus_OutboundRouting_CloudPath is the closest mirror of
// the production wiring: a real Manager over a Redis bus routes an IM
// outbound message to the registered adapter via OutboundConsumer().
func TestManager_RedisBus_OutboundRouting_CloudPath(t *testing.T) {
	rc, _ := redisCoordE2EClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mb := bus.NewRedis(bus.RedisConfig{Client: rc, Prefix: "fastagent-e2e", Group: "e2e-g", Consumer: "e2e-c"})
	if err := mb.Start(ctx); err != nil {
		t.Fatalf("bus.Start: %v", err)
	}

	fake := &fakeRedisChannel{
		name:      "telegram",
		accountID: "botA",
		started:   make(chan struct{}, 1),
		sends:     make(chan bus.OutboundMessage, 4),
	}
	mgr := channels.NewManagerWithLeaser(mb, rediscoord.NewLeaser(rc, "fastagent-e2e"), "holder-e2e")
	mgr.Register(fake)
	// Start blocks until ctx is cancelled (process-lifetime router), so it
	// runs in the background goroutine exactly as the gateway launches it.
	go mgr.Start(ctx)

	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("channel adapter never started")
	}

	mb.Outbound <- bus.OutboundMessage{Channel: "telegram", AccountID: "botA", ChatID: "c1", Text: "routed"}
	select {
	case got := <-fake.sends:
		if got.Text != "routed" || got.ChatID != "c1" {
			t.Fatalf("manager routed wrong message: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager never routed outbound via redis bus")
	}
}

// TestManager_RedisSingleton_OnlyOneHolderStarts_CloudPath verifies the
// lease gate at the Manager level: when two managers share a Redis leaser,
// only the lease-holder's adapter Start runs; the peer stays quiet.
func TestManager_RedisSingleton_OnlyOneHolderStarts_CloudPath(t *testing.T) {
	rc, _ := redisCoordE2EClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busA := bus.NewRedis(bus.RedisConfig{Client: rc, Prefix: "fastagent-e2e", Group: "e2e-g", Consumer: "e2e-c-a"})
	busB := bus.NewRedis(bus.RedisConfig{Client: rc, Prefix: "fastagent-e2e", Group: "e2e-g", Consumer: "e2e-c-b"})
	if err := busA.Start(ctx); err != nil {
		t.Fatalf("busA.Start: %v", err)
	}
	if err := busB.Start(ctx); err != nil {
		t.Fatalf("busB.Start: %v", err)
	}

	leaser := rediscoord.NewLeaser(rc, "fastagent-e2e")
	chanA := &fakeRedisChannel{name: "telegram", accountID: "botA", started: make(chan struct{}, 1), sends: make(chan bus.OutboundMessage, 4)}
	chanB := &fakeRedisChannel{name: "telegram", accountID: "botA", started: make(chan struct{}, 1), sends: make(chan bus.OutboundMessage, 4)}

	mgrA := channels.NewManagerWithLeaser(busA, leaser, "holder-a")
	mgrB := channels.NewManagerWithLeaser(busB, leaser, "holder-b")
	mgrA.RegisterSingleton(chanA)
	mgrB.RegisterSingleton(chanB)
	// Start blocks for the process lifetime (lease loop), so it runs in a
	// background goroutine as the gateway launches it. Start A first and
	// confirm it holds the lease before bringing up the standby B — makes
	// the lease winner deterministic instead of a startup race.
	go mgrA.Start(ctx)
	select {
	case <-chanA.started:
	case <-time.After(5 * time.Second):
		t.Fatal("holder-a adapter never started")
	}

	go mgrB.Start(ctx)
	// Manager B is the standby — with a 10s lease retry it must NOT start
	// within the observation window while A holds the lease.
	select {
	case <-chanB.started:
		t.Fatal("standby holder-b adapter started while holder-a owns the lease")
	case <-time.After(2 * time.Second):
	}
}
