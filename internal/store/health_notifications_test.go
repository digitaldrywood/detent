package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthNotificationStatesPersistAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.db")
	now := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)

	first, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.SaveHealthNotificationStates(ctx, []HealthNotificationState{{
		Identity:  "project:detent:dispatch_stall",
		StateJSON: []byte(`{"schema":1,"active":true}`),
		UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("SaveHealthNotificationStates() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	states, err := second.ListHealthNotificationStates(ctx)
	if err != nil {
		t.Fatalf("ListHealthNotificationStates() error = %v", err)
	}
	if len(states) != 1 || states[0].Identity != "project:detent:dispatch_stall" || !bytes.Equal(states[0].StateJSON, []byte(`{"schema":1,"active":true}`)) || !states[0].UpdatedAt.Equal(now) {
		t.Fatalf("states = %#v", states)
	}
}
