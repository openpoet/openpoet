package jsonlview

import (
	"log"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPConnector abstracts the ability to establish SSH+SFTP connections.
type SFTPConnector interface {
	Connect() (*ssh.Client, *sftp.Client, error)
}

// RemoteWatcher polls a JSONL file on a remote host via SFTP.
// It keeps a persistent SSH+SFTP connection and reconnects on failure.
type RemoteWatcher struct {
	connector SFTPConnector
	path      string // absolute path on remote host
	offset    int64
	events    chan []*SessionEvent
	stopCh    chan struct{}

	mu       sync.Mutex
	stopped  bool
	sshConn  *ssh.Client
	sftpConn *sftp.Client
}

// NewRemoteWatcher creates a remote watcher that will poll the given
// absolute path on the remote host.
func NewRemoteWatcher(connector SFTPConnector, remotePath string, startOffset int64) *RemoteWatcher {
	return &RemoteWatcher{
		connector: connector,
		path:      remotePath,
		offset:    startOffset,
		events:    make(chan []*SessionEvent, 64),
		stopCh:    make(chan struct{}),
	}
}

// Start begins polling in a goroutine.
func (w *RemoteWatcher) Start() {
	go w.run()
}

// Stop halts the polling goroutine and closes the SFTP connection.
func (w *RemoteWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
		w.closeConnectionLocked()
	}
}

// Events returns the channel of new parsed events.
func (w *RemoteWatcher) Events() <-chan []*SessionEvent {
	return w.events
}

func (w *RemoteWatcher) run() {
	ticker := time.NewTicker(1 * time.Second)
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

func (w *RemoteWatcher) poll() {
	sftpClient, err := w.ensureConnection()
	if err != nil {
		log.Printf("[RemoteWatcher] connection failed: %v", err)
		return
	}

	info, err := sftpClient.Stat(w.path)
	if err != nil {
		// File may not exist yet if session just started
		return
	}

	if info.Size() <= w.offset {
		return
	}

	file, err := sftpClient.Open(w.path)
	if err != nil {
		log.Printf("[RemoteWatcher] open failed: %v", err)
		w.closeConnection()
		return
	}
	defer file.Close()

	newEvents, newOffset, err := ParseReaderFromOffset(file, info.Size(), w.offset)
	if err != nil {
		log.Printf("[RemoteWatcher] parse error: %v", err)
		w.closeConnection()
		return
	}

	if len(newEvents) == 0 {
		return
	}

	w.offset = newOffset

	select {
	case w.events <- newEvents:
	default:
		// Channel full, skip (consumer is slow)
	}
}

func (w *RemoteWatcher) ensureConnection() (*sftp.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sftpConn != nil {
		// Check connection health with a cheap operation
		if _, err := w.sftpConn.Getwd(); err == nil {
			return w.sftpConn, nil
		}
		// Connection dead, reconnect
		w.closeConnectionLocked()
	}

	sshClient, sftpClient, err := w.connector.Connect()
	if err != nil {
		return nil, err
	}

	w.sshConn = sshClient
	w.sftpConn = sftpClient
	return sftpClient, nil
}

func (w *RemoteWatcher) closeConnection() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeConnectionLocked()
}

func (w *RemoteWatcher) closeConnectionLocked() {
	if w.sftpConn != nil {
		w.sftpConn.Close()
		w.sftpConn = nil
	}
	if w.sshConn != nil {
		w.sshConn.Close()
		w.sshConn = nil
	}
}
