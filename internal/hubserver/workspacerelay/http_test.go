package workspacerelay

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRelayFrameAuthorityDeadline(t *testing.T) {
	const budget = 25 * time.Second
	for _, tc := range []struct {
		name   string
		caller time.Duration
		want   time.Duration
	}{
		{name: "connection without a deadline", want: budget},
		{name: "caller deadline", caller: time.Minute, want: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
					defer cancel()
				}
				w := HTTP{HTTPDeps: HTTPDeps{
					RequestBudget: budget,
					Logger:        slog.New(slog.DiscardHandler),
					Repository: func() tenantstore.Repository {
						return relayDeadlineRepository{read: func(ctx context.Context) error {
							deadline, ok := ctx.Deadline()
							if !ok || time.Until(deadline) != tc.want {
								t.Fatalf("authority read deadline present = %t, time remaining = %s, want %s", ok, time.Until(deadline), tc.want)
							}
							<-ctx.Done()
							return ctx.Err()
						}}
					},
				}}
				connection := &Connection{Out: make(chan Outbound, 1), Done: make(chan struct{})}
				w.HandlePersonFrame(ctx, connection, workspacehub.Record{}, workspacesession.Frame{Type: workspacesession.TypeResume})
				select {
				case message := <-connection.Out:
					if message.Final != workspacesession.CodeRevoked {
						t.Errorf("connection close reason = %q, want %q", message.Final, workspacesession.CodeRevoked)
					}
				default:
					t.Error("authority deadline did not close the connection")
				}
			})
		})
	}
}

type relayDeadlineRepository struct {
	tenantstore.Repository
	read func(context.Context) error
}

func (r relayDeadlineRepository) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	return r.read(ctx)
}

func TestCloseRelayAuditDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "active caller"},
		{name: "disconnected caller", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancel {
					cancel()
				}
				called := false
				w := HTTP{HTTPDeps: HTTPDeps{
					Clock: time.Now,
					AccessWrite: func(ctx context.Context, _ tracker.OrganizationID, _ func(tenantstore.Writer) error) error {
						called = true
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) != hubapi.DefaultCleanupBudget {
							t.Fatalf("audit deadline present = %t, time remaining = %s, want %s", ok, time.Until(deadline), hubapi.DefaultCleanupBudget)
						}
						if err := ctx.Err(); err != nil {
							t.Fatalf("audit started with canceled context: %v", err)
						}
						<-ctx.Done()
						if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
							t.Errorf("audit context stopped with %v, want deadline exceeded", ctx.Err())
						}
						return nil
					},
				}}
				w.CloseRelayAudit(ctx, workspacehub.Record{}, &Connection{AuditID: "example"}, "person_closed")
				if !called {
					t.Error("audit cleanup did not reach the writer")
				}
			})
		})
	}
}
