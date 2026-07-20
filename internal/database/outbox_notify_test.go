package database

import (
	"testing"
	"time"
)

// TestOutboxNotifierWakesWaiters pins the commit-signal broadcaster the
// long-poll await endpoint rests on: a waiter that took the channel BEFORE the
// notify wakes; a fresh waiter after it blocks until the NEXT notify.
func TestOutboxNotifierWakesWaiters(t *testing.T) {
	db := &DB{}

	w1 := db.OutboxWait()
	w2 := db.OutboxWait()
	select {
	case <-w1:
		t.Fatal("waiter woke before any notify")
	default:
	}
	db.NotifyOutboxAppended()
	for _, w := range []<-chan struct{}{w1, w2} {
		select {
		case <-w:
		case <-time.After(time.Second):
			t.Fatal("waiter did not wake on notify")
		}
	}
	w3 := db.OutboxWait()
	select {
	case <-w3:
		t.Fatal("post-notify waiter was already closed (missed a fresh generation)")
	default:
	}
	db.NotifyOutboxAppended()
	select {
	case <-w3:
	case <-time.After(time.Second):
		t.Fatal("fresh waiter did not wake on the next notify")
	}
}
