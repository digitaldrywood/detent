package githuboutbound

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest/pgwire"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOutboxStepStopsAfterOneDeliveryOrRejection(t *testing.T) {
	failure := errors.New("delivery unavailable")
	for _, test := range []struct {
		name      string
		delivered bool
		failure   error
		canceled  bool
		github    int
	}{
		{name: "delivered", delivered: true, github: 1},
		{name: "rejected", failure: failure, github: 1},
		{name: "idle", github: 1},
		{name: "canceled", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			github := 0
			delivered, _, err := processOutboxStep(ctx, func(context.Context) (bool, time.Time, error) {
				github++
				return test.delivered, time.Time{}, test.failure
			})
			if github != test.github {
				t.Fatalf("delivery budget: github=%d", github)
			}
			if test.canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if !errors.Is(err, test.failure) || delivered != test.delivered {
				t.Fatalf("delivery outcome: delivered=%t err=%v", delivered, err)
			}
		})
	}
}

func TestOutboundIdleStatements(t *testing.T) {
	wire := &pgwire.Wire{}
	admin := hubtest.Database(t)
	repo := hubtest.RepositoryOn(t, admin, hubtest.WithPoolConfig(func(config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return pgwire.Wrap(conn, wire), nil
		}
	}))
	deps := outboundDeps()
	deps.Repository, deps.Details = repo, hubtest.NewDetails()
	backend := &recordingOutboxBackend{}
	deps.Backend = backend
	now := time.Now().UTC().Truncate(time.Microsecond)
	deps.Now = func() time.Time { return now }
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	deps.Organization = organizations[1]
	receiver := NewWorker(New(deps))
	expectWake := func(want bool) {
		t.Helper()
		select {
		case <-receiver.wake:
			if !want {
				t.Fatal("unexpected outbound wake")
			}
		default:
			if want {
				t.Fatal("committed outbound work did not wake its worker")
			}
		}
	}
	for _, org := range organizations {
		hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: org})
		hubtest.Project(t, repo, org, hubtest.ProjectOptions{ID: "project"})
		payload, err := eventstore.NewContent(deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, []byte(`{"body":"example"}`))
		if err != nil {
			t.Fatal(err)
		}
		due := now
		if org == organizations[0] {
			due = now.Add(250 * time.Millisecond)
		}
		row := projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "item", Key: string(org), Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: due, Payload: payload}
		enqueue := func(w tenantstore.Writer) error {
			err := w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), row, receiver.Wake)
			expectWake(false)
			return err
		}
		rollback := errors.New("rollback")
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := enqueue(w); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		expectWake(false)
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, enqueue); err != nil {
			t.Fatal(err)
		}
		expectWake(org == deps.Organization)
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, enqueue); err != nil {
			t.Fatal(err)
		}
		expectWake(false)
	}
	t.Run("timer honors durable deadline", func(t *testing.T) {
		d := deps
		d.Organization = organizations[0]
		d.OutboxPollInterval = time.Minute
		d.Jitter = func() float64 { return 1 }
		ticks := make(chan time.Time, 1)
		waits := make(chan time.Duration, 1)
		d.NewTimer = func(time.Duration) Timer {
			return Timer{Ticks: ticks, Reset: func(delay time.Duration) { waits <- delay }, Stop: func() {}}
		}
		worker := NewWorker(New(d))
		worker.Start(t.Context())
		defer worker.Stop()
		ticks <- now
		if delay := <-waits; delay != 250*time.Millisecond {
			t.Errorf("next delivery in 250ms, timer=%s", delay)
		}
	})
	for _, org := range organizations {
		deps.Organization = org
		wire.Reset()
		worked, err := New(deps).Process(t.Context())
		exchanges, statements := wire.Stop()
		if err != nil || worked != (org == organizations[1]) {
			t.Fatalf("%s processed=%t err=%v", org, worked, err)
		}
		if !worked && statements != 1 {
			t.Errorf("%s idle outbound issued %d statements, %d exchanges", org, statements, exchanges)
		}
	}
	for _, org := range organizations {
		deps.Organization = org
		wire.Reset()
		worked, err := New(deps).Process(t.Context())
		exchanges, statements := wire.Stop()
		if err != nil || worked || statements != 1 || exchanges != 1 {
			t.Errorf("%s after delivery: worked=%t statements=%d exchanges=%d err=%v", org, worked, statements, exchanges, err)
		}
	}
	if len(backend.deliveredKeys) != 1 || backend.deliveredKeys[0] != string(organizations[1]) {
		t.Fatalf("delivered %v", backend.deliveredKeys)
	}
	t.Run("processing lease and retry deadlines", func(t *testing.T) {
		org := organizations[0]
		at := now.Add(250 * time.Millisecond)
		leaseUntil := at.Add(3 * time.Second)
		var row projectstore.GitHubOutbound
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			row, err = w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), at, leaseUntil, "lease")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		checkDeadline := func(want time.Time) {
			t.Helper()
			wire.Reset()
			got, err := repo.GitHubOutboundDeadline(t.Context(), org)
			exchanges, statements := wire.Stop()
			if err != nil || !got.Equal(want) || exchanges != 1 || statements != 1 {
				t.Fatalf("deadline %s, want %s, statements=%d exchanges=%d err=%v", got, want, statements, exchanges, err)
			}
		}
		checkDeadline(leaseUntil)
		row.Status, row.Outcome, row.NextAttemptAt = "retrying", "rejected", leaseUntil.Add(time.Second)
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubOutboxWriter).FinishGitHubOutbound(t.Context(), row, at)
		}); err != nil {
			t.Fatal(err)
		}
		checkDeadline(row.NextAttemptAt)
	})
	t.Run("rollback releases a due row at fast cadence", func(t *testing.T) {
		d := deps
		org := organizations[1]
		d.Organization = org
		d.OutboxPollInterval = time.Second
		d.Jitter = func() float64 { return 1 }
		backend := &recordingOutboxBackend{}
		d.Backend = backend
		ticks := make(chan time.Time, 1)
		waits := make(chan time.Duration, 1)
		d.NewTimer = func(time.Duration) Timer {
			return Timer{Ticks: ticks, Reset: func(delay time.Duration) { waits <- delay }, Stop: func() {}}
		}
		worker := NewWorker(New(d))
		worker.Start(t.Context())
		defer worker.Stop()
		tick := func() time.Duration {
			ticks <- now
			return <-waits
		}
		for range 5 {
			tick()
		}
		if delay := tick(); delay != time.Minute {
			t.Fatalf("idle worker did not reach maximum poll interval: %s", delay)
		}
		payload, err := eventstore.NewContent(d.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, []byte(`{"body":"example"}`))
		if err != nil {
			t.Fatal(err)
		}
		row := projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "item", Key: "locked", Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: now, Payload: payload}
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), row, nil)
		}); err != nil {
			t.Fatal(err)
		}
		tx, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var id int64
		if err := tx.QueryRowContext(t.Context(), "SELECT id FROM github_outbound WHERE organization_id=$1 AND command_key=$2 FOR UPDATE", org, row.Key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if delay := tick(); delay != d.OutboxPollInterval || len(backend.deliveredKeys) != 0 {
			t.Errorf("locked due row: next poll=%s deliveries=%v", delay, backend.deliveredKeys)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		tick()
		if len(backend.deliveredKeys) != 1 || backend.deliveredKeys[0] != row.Key {
			t.Fatalf("deliveries after rollback=%v", backend.deliveredKeys)
		}
	})
}
