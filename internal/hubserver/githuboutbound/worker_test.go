package githuboutbound

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest/pgwire"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
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
			delivered, err := processOutboxStep(ctx, func(context.Context) (bool, error) {
				github++
				return test.delivered, test.failure
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
	repo := hubtest.Repository(t, hubtest.WithPoolConfig(func(config *pgxpool.Config) {
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
	now := time.Now().UTC()
	deps.Now = func() time.Time { return now }
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, org := range organizations {
		hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: org})
		hubtest.Project(t, repo, org, hubtest.ProjectOptions{ID: "project"})
		payload, err := eventstore.NewContent(deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, []byte(`{"body":"example"}`))
		if err != nil {
			t.Fatal(err)
		}
		due := now
		if org == organizations[0] {
			due = now.Add(time.Hour)
		}
		err = repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "item", Key: string(org), Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: due, Payload: payload})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
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
}
