package platformstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func TestNotificationQueuesPreserveTimestampAndScope(t *testing.T) {
	r := repository(t, template.Database(t), testConfig(t))
	at := time.Date(2026, 11, 1, 12, 0, 0, 123456000, time.FixedZone("example", -6*60*60))
	for _, account := range []bool{false, true} {
		tx, err := r.BeginEntry(t.Context(), corestore.Serving, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			if err := tx.EnqueueNotification(t.Context(), account, Notification{Event: "signup", ID: id, Email: id + "@example.test", OccurredAt: at.Format(time.RFC3339Nano)}); err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []bool{false, true} {
		found := map[string]bool{}
		for range 2 {
			item, err := r.ClaimNotification(t.Context(), corestore.Serving, account, at.Add(time.Hour).Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := time.Parse(time.RFC3339Nano, item.OccurredAt)
			if err != nil || !parsed.Equal(at) || parsed.Location() != time.UTC {
				t.Fatalf("notification timestamp=%s err=%v", item.OccurredAt, err)
			}
			if found[item.ID] {
				t.Fatal("notification claimed twice")
			}
			found[item.ID] = true
		}
		if !found["first"] || !found["second"] {
			t.Fatalf("notification recipients=%v", found)
		}
		if _, err := r.ClaimNotification(t.Context(), corestore.Serving, account, at.Format(time.RFC3339Nano)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("empty queue=%v", err)
		}
	}
}

func TestConstructorRejectsUnconfiguredPoolBeforeConnect(t *testing.T) {
	native, err := pgxpool.ParseConfig("postgres://example:fixture@127.0.0.1:1/example?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	native.MaxConns = 3
	calls := 0
	native.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { calls++; return errors.New("unexpected connection") }
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := New(t.Context(), pool, testConfig(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unconfigured pool=%v", err)
	}
	if calls != 0 {
		t.Fatal("constructor connected before validating native execution")
	}
}
