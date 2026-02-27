package jsonlview

import (
	"sync"
	"time"
)

// Watcher polls a JSONL file for new lines and pushes parsed events
// through a channel. Uses polling (not fsnotify) for simplicity.
type Watcher struct {
	path   string
	offset int64
	events chan []*SessionEvent
	stopCh chan struct{}

	mu      sync.Mutex
	stopped bool
}

// NewWatcher creates a watcher starting from a byte offset.
// Use offset=0 to watch from the beginning, or pass the file size
// after initial load to only get new events.
func NewWatcher(path string, startOffset int64) *Watcher {
	return &Watcher{
		path:   path,
		offset: startOffset,
		events: make(chan []*SessionEvent, 64),
		stopCh: make(chan struct{}),
	}
}

// Start begins polling in a goroutine.
func (w *Watcher) Start() {
	go w.run()
}

// Stop halts the polling goroutine.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
	}
}

// Events returns the channel of new parsed events.
func (w *Watcher) Events() <-chan []*SessionEvent {
	return w.events
}

func (w *Watcher) run() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	defer close(w.events)

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.poll()
		}
	}
}

func (w *Watcher) poll() {
	newEvents, newOffset, err := ParseFileFromOffset(w.path, w.offset)
	if err != nil || len(newEvents) == 0 {
		return
	}

	w.offset = newOffset

	select {
	case w.events <- newEvents:
	default:
		// Channel full, skip this batch (consumer is slow)
	}
}
