package hubserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubOutboundSignal struct{ calls chan OutboxItem }

func (s githubOutboundSignal) Execute(ctx context.Context, item OutboxItem) error {
	select {
	case s.calls <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s githubOutboundSignal) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return s.Execute(ctx, item)
}

func TestPostgresGitHubOutboundWorkerStarts(t *testing.T) {
	calls := make(chan OutboxItem, 1)
	service := openTestService(t, Config{OutboxBackend: githubOutboundSignal{calls: calls}})
	org := service.database.hostedOrganization
	raw := json.RawMessage(`{"phase":"summary","body":"Example summary","marker":"<!-- example-summary -->"}`)
	payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}); err != nil {
			return err
		}
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: "explicit", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: time.Now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-calls:
		var desired WorkpadDesired
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

func TestPostgresGitHubOutboundUsesConfiguredBackend(t *testing.T) {
	repository := projectRecordPostgres(t)
	details := &memoryRunDetails{}
	backend := &recordingOutboxBackend{}
	service := &Service{database: &database{}, config: (Config{TenantRepository: repository, RunDetails: details, OutboxBackend: backend}).normalized()}
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		now := time.Now().UTC()
		raw, err := json.Marshal(WorkpadDesired{Phase: "summary", Summary: true, Marker: "<!-- example-summary -->", Body: "Example summary", IssueNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}); err != nil {
				return err
			}
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: string(org), Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: now, Payload: payload})
		}); err != nil {
			t.Fatal(err)
		}
		service.database.hostedOrganization = org
		if processed, err := service.ProcessOutbox(t.Context()); err != nil || !processed {
			t.Fatalf("delivery processed=%t err=%v", processed, err)
		}
		if processed, err := service.ProcessOutbox(t.Context()); err != nil || processed {
			t.Fatalf("completion replay processed=%t err=%v", processed, err)
		}
	}
	if backend.executeCalls != 2 || backend.effects != 2 || len(backend.deliveredKeys) != 2 {
		t.Fatalf("backend calls=%d effects=%d keys=%v", backend.executeCalls, backend.effects, backend.deliveredKeys)
	}
}
