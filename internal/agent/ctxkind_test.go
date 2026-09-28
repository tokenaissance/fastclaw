package agent

import (
	"context"
	"testing"
	"time"
)

type ctxValueKey struct{}

// recordCtx is the "record" half of the context taxonomy (ctxkind.go): it must keep every value
// the store routes by, drop the parent's cancellation AND its deadline, and carry a short bound of
// its own.
//
// Falsification: implement it as `context.WithTimeout(ctx, budget)` and the parent's cancellation
// survives — which is precisely how the production events were lost on 2026-09-28.
func TestRecordCtxKeepsValuesAndDropsTheParentsDeath(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelParent()
	parent = context.WithValue(parent, ctxValueKey{}, "user-1")

	record, cancelRecord := recordCtx(parent, time.Second)
	defer cancelRecord()

	if got, _ := record.Value(ctxValueKey{}).(string); got != "user-1" {
		t.Fatalf("recordCtx dropped a value (%q); the store routes by what it carries", got)
	}
	if _, ok := record.Deadline(); !ok {
		t.Fatal("recordCtx has no deadline; the append would be unbounded")
	}

	cancelParent() // the turn dies
	select {
	case <-record.Done():
		t.Fatal("recordCtx died with its parent; the record would have been refused")
	case <-time.After(50 * time.Millisecond):
		// still alive: that is the point
	}

	// And its own bound does fire, so a store outage cannot pin the writer.
	short, cancelShort := recordCtx(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	<-short.Done()
	if short.Err() != context.DeadlineExceeded {
		t.Fatalf("recordCtx's own bound did not fire: %v", short.Err())
	}
}
