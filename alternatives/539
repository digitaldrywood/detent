package citrigger

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReapplyStaggersRepositoryActions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 7, 15, 14, 0, 5, 0, time.UTC)
	_, timestampPath := CoordinationPaths(dir, "digitaldrywood/detent")
	if err := os.MkdirAll(filepath.Dir(timestampPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(timestampPath, []byte(now.Add(-5*time.Second).Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var waits []time.Duration
	actions := 0
	err := Reapply(context.Background(), Options{
		CoordinationDir: dir,
		Repository:      "digitaldrywood/detent",
		Stagger:         15 * time.Second,
	}, Dependencies{
		Now: func() time.Time { return now },
		Wait: func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			now = now.Add(duration)
			return nil
		},
	}, func(context.Context) error {
		actions++
		return nil
	})
	if err != nil {
		t.Fatalf("Reapply() error = %v", err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{10 * time.Second}) {
		t.Fatalf("waits = %#v, want 10s stagger", waits)
	}
	if actions != 1 {
		t.Fatalf("actions = %d, want 1", actions)
	}
	raw, err := os.ReadFile(timestampPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(raw), now.Format(time.RFC3339Nano)+"\n"; got != want {
		t.Fatalf("timestamp = %q, want %q", got, want)
	}
}
