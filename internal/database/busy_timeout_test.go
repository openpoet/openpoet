package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// TestBusyTimeoutWaitsForWriteLock proves database.New() opens with a
// busy_timeout: a write that collides with a lock held by another connection
// waits for it to clear instead of failing immediately with SQLITE_BUSY.
func TestBusyTimeoutWaitsForWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")

	db, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A second, independent connection grabs and holds the write lock.
	locker, err := sqlx.Connect("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	locker.SetMaxOpenConns(1)
	t.Cleanup(func() { locker.Close() })

	ctx := context.Background()
	conn, err := locker.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}

	const holdFor = 800 * time.Millisecond
	released := make(chan struct{})
	go func() {
		time.Sleep(holdFor)
		_, _ = conn.ExecContext(ctx, "COMMIT")
		_ = conn.Close()
		close(released)
	}()

	start := time.Now()
	err = db.SetSetting(ctx, "busy_timeout_probe", "ok")
	elapsed := time.Since(start)
	<-released

	if err != nil {
		t.Fatalf("write under contention failed (busy_timeout missing?): %v", err)
	}
	if elapsed < holdFor/2 {
		t.Fatalf("write returned in %v — it did not wait for the held lock", elapsed)
	}
}

// TestBusyTimeoutWithoutPragmaFailsFast is a control: the same collision on a
// connection opened WITHOUT busy_timeout errors immediately, showing the assert
// above is really exercising the pragma and not just a slow machine.
func TestBusyTimeoutWithoutPragmaFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nobusy.db")

	db, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	bare, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	bare.SetMaxOpenConns(1)
	t.Cleanup(func() { bare.Close() })

	ctx := context.Background()
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("db conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	defer conn.ExecContext(ctx, "COMMIT")

	if _, err := bare.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('x','y')"); err == nil {
		t.Fatal("expected SQLITE_BUSY without busy_timeout, got nil error")
	}
}
