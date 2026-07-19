package database

import "sync"

// OutboxNotifier is a commit-signal broadcaster for long-poll readers: writers
// call NotifyOutboxAppended after committing a tx that appended outbox events,
// and awaiters block on Wait() until the next notify or their own timeout. It
// uses the closed-channel-swap idiom so any number of waiters wake on one
// signal without per-waiter bookkeeping.
type OutboxNotifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newOutboxNotifier() *OutboxNotifier {
	return &OutboxNotifier{ch: make(chan struct{})}
}

// Wait returns a channel that closes on the next NotifyOutboxAppended. Callers
// take it BEFORE reading the current cursor, so a signal that races the read is
// never lost (they re-read after waking).
func (n *OutboxNotifier) Wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *OutboxNotifier) broadcast() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}

// OutboxWait exposes the notifier's wait channel on the DB.
func (d *DB) OutboxWait() <-chan struct{} {
	return d.outboxNotifier().Wait()
}

// NotifyOutboxAppended wakes every long-poll awaiter. Safe to over-call (a
// spurious wake just makes readers re-check and find nothing new); call it
// AFTER the committing tx so the appended rows are already visible.
func (d *DB) NotifyOutboxAppended() {
	d.outboxNotifier().broadcast()
}

func (d *DB) outboxNotifier() *OutboxNotifier {
	d.outboxOnce.Do(func() { d.outboxNtf = newOutboxNotifier() })
	return d.outboxNtf
}
