package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProviderSchedulerEndToEnd(t *testing.T) {
	t.Parallel()
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte("provider-test-admin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	admin, err := New(Config{URL: server.URL, TokenSource: func() string { return "provider-test-admin" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := admin.request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	organization := organizations.Items[0].ID
	var project tracker.NativeProject
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "capacity", "idempotency_key": "capacity", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}, &project); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Hostname: "customer", DisplayName: "Runner", Capacity: 2, Version: "test"}
	if _, err := EnrollRunner(t.Context(), path, organization, enrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: server.URL, IdentityFile: path, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := admin.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := clientTestPolicy()
	if _, err := native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"unsupported model", "compatible model"} {
		if _, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: title}, Title: title, State: "Todo"}); err != nil {
			t.Fatal(err)
		}
	}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, ProviderReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{report}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor, ProviderRequirement: func(_ context.Context, issue connector.Issue) (providercapacity.Requirement, error) {
		model := "sol"
		if issue.Title == "unsupported model" {
			model = "astra"
		}
		return providercapacity.Requirement{Role: "code", Backend: "codex", Model: model}, nil
	}}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].Title != "compatible model" {
		t.Fatalf("selection = %+v, %v", candidates, err)
	}
	issue := candidates[0]
	if _, err := scheduler.AdoptClaim(t.Context(), issue, time.Now()); err != nil {
		t.Fatal(err)
	}
	execution := scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("native claim has no execution lifecycle")
	}
	report.Availability = "exhausted"
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "sol"}); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("changed local quota start = %v", err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	report.Availability = "available"
	candidates, err = scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("released start did not free capacity: %v", err)
	}
	execution = scheduler.RunExecution(candidates[0].ID)
	if execution == nil {
		t.Fatal("reclaimed issue has no execution lifecycle")
	}
	identity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "sol"}
	transport := &executionTransport{next: client.httpClient.Transport}
	client.httpClient.Transport = transport
	t.Cleanup(func() { client.httpClient.Transport = transport.next })
	transport.drop.Store(true)
	if err := execution.Start(t.Context(), identity); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("lost start acknowledgment = %v", err)
	}
	report.Availability = "exhausted"
	if err := execution.Start(t.Context(), identity); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("unacknowledged start skipped local quota revalidation: %v", err)
	}
	report.Availability = "available"
	if err := execution.Start(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	report.Availability = "exhausted"
	if err := execution.Start(t.Context(), identity); err != nil {
		t.Fatalf("active identity changed: %v", err)
	}
	if _, err := scheduler.FetchCandidateIssues(t.Context(), request); !errors.Is(err, orchestrator.ErrSchedulingUnavailable) {
		t.Fatalf("incompatible capacity should defer scheduling: %v", err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), candidates[0].ID, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalProviderRevalidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	requirement := providercapacity.Requirement{Role: "code", Backend: "codex", Model: "sol"}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
	for _, test := range []struct {
		name   string
		change func(*providercapacity.Report)
		model  string
		want   bool
	}{
		{"same account", func(*providercapacity.Report) {}, "sol", true},
		{"stale quota", func(r *providercapacity.Report) {
			r.Availability = "exhausted"
			r.ObservedAt = now.Add(-providercapacity.MaxAge)
		}, "sol", true},
		{"exhaustion", func(r *providercapacity.Report) { r.Availability = "exhausted" }, "sol", false},
		{"account change", func(r *providercapacity.Report) { r.AccountAlias = "other" }, "sol", false},
		{"shared pool change", func(r *providercapacity.Report) { r.SharedAccountAlias = "other" }, "sol", false},
		{"model changed", func(*providercapacity.Report) {}, "astra", false},
		{"invalid report", func(r *providercapacity.Report) { r.MaxConcurrent = 0 }, "sol", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			test.change(&current)
			e := &nativeExecution{scheduler: &Scheduler{now: func() time.Time { return now }, providerReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{current}, nil }}, claim: nativeClaim{lease: tracker.NativeLease{ProviderReservation: &providercapacity.Reservation{Requirement: requirement, Report: report}}}}
			err := e.validateProviderStart(tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: test.model})
			if (err == nil) != test.want || err != nil && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatal(err)
			}
		})
	}
}
