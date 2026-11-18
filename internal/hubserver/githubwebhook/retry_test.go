package githubwebhook

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }
func retryFixture(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	repository := hubtest.Repository(t)
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org", Now: now})
	for _, project := range []tracker.ProjectID{"first", "second"} {
		hubtest.Project(t, repository, "org", hubtest.ProjectOptions{ID: project})
		if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project)
			if err != nil {
				return err
			}
			record.Settings.Repository, record.Settings.Intake = "example-org/example-project", "manual"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return New(Deps{Repository: repository, Organization: "org", Details: hubtest.NewDetails(), Now: func() time.Time { return now }, Integration: func(_ context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (Integration, error) {
		return Integration{Scope: hubapi.NewScope(hubapi.Facts{Organization: organization, Project: project}, nil), Repository: "example-org/example-project"}, nil
	}}), &now
}
func retryEdit(t *testing.T, service *Service) (projectstore.GitHubWebhook, Payload) {
	t.Helper()
	raw := json.RawMessage(`{"action":"edited","repository":{"full_name":"example-org/example-project"},"issue":{"node_id":"I_source","number":1,"title":"Updated source","body":"Updated source body","html_url":"https://github.com/example-org/example-project/issues/1","state":"open","user":{"login":"source-author"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","labels":[],"assignees":[]}}`)
	var payload Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(service.deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return projectstore.GitHubWebhook{DeliveryID: "delivery", Repository: "example-org/example-project", EventType: "issues", Action: "edited", Payload: ref, ReceivedAt: service.deps.Now(), NextAttemptAt: service.deps.Now(), ExpiresAt: service.deps.Now().Add(time.Hour), Status: "pending"}, payload
}
func TestGitHubWebhookRetryJitter(t *testing.T) {
	service, now := retryFixture(t)
	row, payload := retryEdit(t, service)
	payload.Issue.NodeID = nil
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	row.Payload, err = eventstore.NewContent(service.deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.(projectstore.GitHubIntakeWriter).CreateGitHubWebhook(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for attempt, test := range []struct {
		jitter float64
		delay  time.Duration
	}{{0, time.Second}, {1, 4 * time.Second}, {0.5, 6 * time.Second}, {0.25, 10 * time.Second}, {0, 16 * time.Second}, {1, 64 * time.Second}, {0.5, 48 * time.Second}} {
		service.deps.Jitter = func() float64 { return test.jitter }
		if err := service.Process(t.Context(), row); err == nil {
			t.Fatal("invalid source accepted")
		}
		if err := service.deps.Repository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), row.DeliveryID, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if row.Attempts != int64(attempt+1) || !row.NextAttemptAt.Equal(now.Add(test.delay)) {
			t.Fatalf("attempt=%d next=%v want attempt=%d next=%v", row.Attempts, row.NextAttemptAt, attempt+1, now.Add(test.delay))
		}
		*now = now.Add(2 * time.Minute)
	}
}

func TestDeferredWebhookProcessesEachAcceptedProject(t *testing.T) {
	s, _ := retryFixture(t)
	row, _ := retryEdit(t, s)
	var applied []tracker.ProjectID
	integrate := s.deps.Integration
	s.deps.Integration = func(ctx context.Context, org tracker.OrganizationID, project tracker.ProjectID) (Integration, error) {
		applied = append(applied, project)
		return integrate(ctx, org, project)
	}
	store := func(project tracker.ProjectID) {
		t.Helper()
		row.BindingProject = project
		row.EventType = "push"
		if err := s.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubIntakeWriter)
			created, err := owner.CreateGitHubWebhook(t.Context(), row)
			if err != nil || !created {
				t.Fatalf("create receipt for %s: created=%t err=%v", project, created, err)
			}
			created, err = owner.CreateGitHubWebhook(t.Context(), row)
			if created {
				t.Fatalf("duplicate receipt for %s", project)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	store("second")
	store("first")
	for range 3 {
		if err := s.Maintain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(applied)
	if !slices.Equal(applied, []tracker.ProjectID{"first", "second"}) {
		t.Fatalf("applied projects %v", applied)
	}
	applied = nil
	row.DeliveryID = "before-rebinding"
	store("first")
	for _, repository := range []string{"example-org/replacement", row.Repository} {
		if err := s.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), "first")
			if err != nil {
				return err
			}
			project.Settings.Repository = repository
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("applied old receipt after repository rebinding to %v", applied)
	}
}
