package hubclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestProviderSchedulerEndToEnd(t *testing.T) {
	t.Parallel()
	for _, unavailable := range []string{"fallback", "fail"} {
		t.Run(unavailable, func(t *testing.T) { t.Parallel(); testProviderSchedulerEndToEnd(t, unavailable) })
	}
}

func testProviderSchedulerEndToEnd(t *testing.T, unavailable string) {
	t.Helper()
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
		model := "sol"
		if title == "unsupported model" {
			model = "astra"
		}
		body := "```detent-agent\nschema: 1\nmodel: " + model + "\n```"
		if _, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: title}, Title: title, Body: body, State: "Todo"}); err != nil {
			t.Fatal(err)
		}
	}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, ProviderReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{report}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	backend, launches := providerWorkspaceWrapper(t)
	cfg := config.Default()
	cfg.Agents.ModelSelection.Preset = new("sol_first")
	cfg.Agents.ModelSelection.Unavailable = &unavailable
	localRunner, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &providerPreviewWorkspace{}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	request := orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor, ProviderRequirement: func(ctx context.Context, issue connector.Issue, reports []providercapacity.Report) (providercapacity.Requirement, error) {
		return localRunner.DispatchCapacity(ctx, runner.RunRequest{Issue: issue, ProviderReports: reports})
	}}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].Title != "compatible model" {
		t.Fatalf("selection = %+v, %v", candidates, err)
	}
	if *launches != 0 {
		t.Fatalf("provider scheduling launched wrapper %d times before workspace existed", *launches)
	}
	issue := candidates[0]
	if _, err := scheduler.AdoptClaim(t.Context(), issue, time.Now()); err != nil {
		t.Fatal(err)
	}
	execution := scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("native claim has no execution lifecycle")
	}
	attemptWorkspace := t.TempDir()
	process := runner.AgentProcessRequest{Workspace: attemptWorkspace, Environment: procgroup.Environment{Variables: workspace.EnvironmentVariables(workspace.Info{Path: attemptWorkspace}, workspace.Issue{ID: issue.ID, Identifier: issue.Identifier})}}
	models, err := backend.ListModels(t.Context(), process)
	if err != nil || len(models) != 1 || models[0].Model != "sol" || *launches != 1 {
		t.Fatalf("workspace wrapper catalog = %+v, %v; launches=%d", models, err, *launches)
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

// Dispatch must not call any workspace operation before acquiring a claim.
type providerPreviewWorkspace struct{ workspace.Backend }

func providerWorkspaceWrapper(t *testing.T) (*codex.AgentBackend, *int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launches := new(int)
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		(*launches)++
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProviderWorkspaceWrapperProcess$")
		cmd.Env = append(os.Environ(), "DETENT_TEST_PROVIDER_WRAPPER=1")
		return cmd
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := codex.NewAppServer(factory, codex.WithReadTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := codex.NewAgentBackend(server, codex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return backend, launches
}

func TestProviderWorkspaceWrapperProcess(t *testing.T) {
	if os.Getenv("DETENT_TEST_PROVIDER_WRAPPER") != "1" {
		return
	}
	cwd, err := os.Getwd()
	expected := os.Getenv("DETENT_WORKSPACE")
	// This models a wrapper that acquires its work item using PWD/workspace.
	actualInfo, statErr := os.Stat(cwd)
	expectedInfo, expectedErr := os.Stat(expected)
	if err != nil || statErr != nil || expectedErr != nil || !os.SameFile(actualInfo, expectedInfo) || os.Getenv("PWD") != expected || os.Getenv("DETENT_ISSUE_ID") == "" || os.Getenv("DETENT_ISSUE_IDENTIFIER") == "" {
		os.Exit(12)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(13)
		}
		if request.ID == nil {
			continue
		}
		var result json.RawMessage
		switch request.Method {
		case "initialize":
			result = json.RawMessage(`{"userAgent":"workspace-wrapper"}`)
		case "model/list":
			result = json.RawMessage(`{"data":[{"id":"sol","model":"sol","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}`)
		default:
			os.Exit(14)
		}
		if encoder.Encode(struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}{*request.ID, result}) != nil {
			os.Exit(15)
		}
	}
	os.Exit(0)
}
