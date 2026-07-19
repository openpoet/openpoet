package automation

import (
	"context"
	"testing"

	"openpoet/internal/database"
)

// fakeEventStore serves a synthetic outbox for the await scan tests. head is
// the committed high-water mark; ReadEventOutboxPage returns at most `limit`
// rows above `after`, mirroring the real page reader.
type fakeEventStore struct {
	events []database.EventOutboxEvent
}

func (f *fakeEventStore) head() int64 {
	if len(f.events) == 0 {
		return 0
	}
	return f.events[len(f.events)-1].Sequence
}

func (f *fakeEventStore) ReadEventOutboxPage(_ context.Context, after int64, limit int) ([]database.EventOutboxEvent, int64, error) {
	var page []database.EventOutboxEvent
	for _, e := range f.events {
		if e.Sequence > after {
			page = append(page, e)
			if len(page) >= limit {
				break
			}
		}
	}
	return page, f.head(), nil
}

func (f *fakeEventStore) GetEventOutboxConsumerCursor(context.Context, string, string) (*database.EventOutboxConsumerCursor, error) {
	return &database.EventOutboxConsumerCursor{CursorSequence: 0}, nil
}

func (f *fakeEventStore) AckEventOutbox(context.Context, string, string, int64) (*database.EventOutboxConsumerCursor, error) {
	return &database.EventOutboxConsumerCursor{}, nil
}

func makeEvents(n int, etype, aggID string) []database.EventOutboxEvent {
	out := make([]database.EventOutboxEvent, n)
	for i := 0; i < n; i++ {
		out[i] = database.EventOutboxEvent{
			Sequence: int64(i + 1), EventID: "e", EventType: etype,
			AggregateType: "session", AggregateID: aggID, Actor: "coordinator",
			SchemaVersion: 1, PayloadJSON: "{}",
		}
	}
	return out
}

// TestOutboxHeadPagesPastScanLimit pins the fix for the critical baseline bug:
// with >500 rows, the baseline must be the true committed head, not the
// 500th-oldest row.
func TestOutboxHeadPagesPastScanLimit(t *testing.T) {
	store := &fakeEventStore{events: makeEvents(1200, "session.turn_completed", "s1")}
	api := &commandAPI{events: store}
	head, err := api.outboxHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head != 1200 {
		t.Fatalf("outboxHead = %d, want 1200 (not capped at the 500-row window)", head)
	}
}

// TestScanUntilMatchFindsMatchBeyondWindow pins that a match beyond the first
// 500-row window is found in one call (not parked waiting for a notify).
func TestScanUntilMatchFindsMatchBeyondWindow(t *testing.T) {
	events := makeEvents(600, "platform.noise", "x")
	// One matching event at sequence 601, past the first 500-row window.
	events = append(events, database.EventOutboxEvent{
		Sequence: 601, EventID: "m", EventType: "conflict.detected",
		AggregateType: "conflict", AggregateID: "C-1", Actor: "coordinator",
		SchemaVersion: 1, PayloadJSON: "{}",
	})
	store := &fakeEventStore{events: events}
	api := &commandAPI{events: store}
	matched, next, caughtUp, err := api.scanUntilMatchOrHead(context.Background(), 0, "conflict.")
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].Sequence != 601 {
		t.Fatalf("matched = %+v, want the seq-601 conflict past the window", matched)
	}
	if next < 601 || !caughtUp {
		t.Fatalf("next=%d caughtUp=%v, want >=601 and caught up", next, caughtUp)
	}
}

// TestScanCaughtUpWhenNoMatch pins that a full scan with no match reports
// caught-up at head, so the handler parks (rather than looping) correctly.
func TestScanCaughtUpWhenNoMatch(t *testing.T) {
	store := &fakeEventStore{events: makeEvents(700, "platform.noise", "x")}
	api := &commandAPI{events: store}
	matched, next, caughtUp, err := api.scanUntilMatchOrHead(context.Background(), 0, "conflict.")
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 0 || !caughtUp || next != 700 {
		t.Fatalf("no-match scan: matched=%d caughtUp=%v next=%d, want 0/true/700", len(matched), caughtUp, next)
	}
}
