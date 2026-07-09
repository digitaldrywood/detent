package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestIsBusy(t *testing.T) {
	t.Parallel()

	busyErr := generateBusyError(t)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "plain error", err: errors.New("database is locked"), want: false},
		{name: "sqlite busy error", err: busyErr, want: true},
		{name: "wrapped sqlite busy error", err: fmt.Errorf("creating api key: %w", busyErr), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsBusy(tt.err); got != tt.want {
				t.Fatalf("IsBusy(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// generateBusyError produces a genuine driver SQLITE_BUSY error by holding a
// write transaction on one connection while a second, zero-timeout connection
// attempts a write.
func generateBusyError(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "busy.db")

	holder, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holderConn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	t.Cleanup(func() { _ = holderConn.Close() })
	if _, err := holderConn.ExecContext(ctx, "create table if not exists busy_probe (id integer)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := holderConn.ExecContext(ctx, "begin immediate"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	t.Cleanup(func() { _, _ = holderConn.ExecContext(ctx, "rollback") })

	contender, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if _, err := contender.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatalf("set contender busy_timeout: %v", err)
	}
	_, busyErr := contender.ExecContext(ctx, "insert into busy_probe (id) values (1)")
	if busyErr == nil {
		t.Fatal("expected contender write to fail with SQLITE_BUSY")
	}
	return busyErr
}
