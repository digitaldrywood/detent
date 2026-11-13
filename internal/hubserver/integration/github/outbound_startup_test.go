package github_test

import (
	"context"
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"testing"
	"time"
)

type githubOutboundSignal struct{ calls chan hubserver.OutboxItem }

func (s githubOutboundSignal) Execute(ctx context.Context, item hubserver.OutboxItem) error {
	select {
	case s.calls <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s githubOutboundSignal) VerifyAndExecute(ctx context.Context, item hubserver.OutboxItem) error {
	return s.Execute(ctx, item)
}

func TestPostgresGitHubOutboundWorkerStarts(t *testing.T) {
	calls := make(chan hubserver.OutboxItem, 1)
	cfg, err := gitHubServiceConfig(t, hubserver.Config{InitialAdminToken: []byte("example-admin"), OutboxBackend: githubOutboundSignal{calls: calls}})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, postgresFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	org := tracker.OrganizationID("org_example")
	raw := json.RawMessage(`{"phase":"summary","body":"Example summary","marker":"<!-- example-summary -->"}`)
	payload, err := eventstore.NewContent(cfg.RunDetails).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}); err != nil {
			return err
		}
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubserver.ErrPostgresFeatureUnavailable
		}
		return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: "explicit", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(hubserver.MutationWorkpad), CreatedAt: time.Now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-calls:
		var desired hubserver.WorkpadDesired
		if err := json.Unmarshal(item.Desired, &desired); err != nil {
			t.Fatal(err)
		}
		if item.IdempotencyKey != "explicit" || desired.Body != "Example summary" || desired.Marker != "<!-- example-summary -->" {
			t.Fatalf("delivered=%+v", item)
		}
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}
