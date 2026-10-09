package hubclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var hubDatabasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return hubserver.Open(ctx, hubserver.Config{DatabasePath: path, GitHubDisabled: true, Logger: slog.New(slog.DiscardHandler)})
})

func TestProviderSchedulerEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	for _, unavailable := range []string{"intake off during admission", "fallback", "fail", "local wait", "bounded", "plain", "known waits", "six slots", "provider slots", "mixed providers", "provider hydration", "stage slots", "quota reset"} {
		t.Run(unavailable, func(t *testing.T) { t.Parallel(); testProviderSchedulerEndToEnd(t, unavailable) })
	}
}

func TestProviderEmptyPreviewReachesClaim(t *testing.T) {
	t.Parallel()
	for _, issueID := range []string{"", "wi_test"} {
		t.Run("preview_"+issueID, func(t *testing.T) {
			claims := 0
			observations := 0
			requirement := providercapacity.Requirement{Role: runner.RoleCode, Backend: "codex", Model: "sol"}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/capabilities":
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeDispatchWaitCapability}})
				case "/api/v2/organizations/org_test/projects/prj_test/claims/preview":
					if issueID == "" {
						_, _ = w.Write([]byte(`{"items":[]}`))
					} else {
						_, _ = w.Write([]byte(`{"items":[{"work_item_id":"wi_test","state":"Todo","title":"test"}]}`))
					}
				case "/api/v2/organizations/org_test/projects/prj_test/claims":
					claims++
					w.WriteHeader(http.StatusConflict)
					if issueID == "" {
						_, _ = w.Write([]byte(`{"code":"no_claimable_work","message":"No work"}`))
					} else {
						_, _ = w.Write([]byte(`{"code":"provider_capacity","message":"private customer prompt token=secret"}`))
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			client, err := New(Config{URL: "https://hub.test", TokenSource: func() string { return "test-token" }, HTTPClient: providerHandlerClient(handler)})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			scheduler := &Scheduler{providerReports: func() ([]providercapacity.Report, error) { return nil, nil }}
			request := orchestrator.SchedulingRequest{ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
				if issueID == "" {
					t.Error("empty preview should not resolve a model")
				}
				return requirement, nil
			}, CandidateClaimObserved: func(_ context.Context, candidate connector.Issue, allowed bool, predicate string, required *providercapacity.Requirement) {
				observations++
				if candidate.ID != issueID || allowed || predicate != "hub_claim.provider_capacity" || required == nil || *required != requirement {
					t.Fatalf("claim observation: issue=%s allowed=%t predicate=%s requirement=%+v", candidate.ID, allowed, predicate, required)
				}
			}}
			_, err = scheduler.claimPreviewCandidates(t.Context(), request, &NativeConnector{client: native}, tracker.NativeClaim{}, 1)
			if claims != 1 || issueID == "" && (!errors.Is(err, ErrNoClaimableWork) || observations != 0) || issueID != "" && (hubErrorCode(err) != "provider_capacity" || observations != 1) {
				t.Fatalf("preview: claims=%d observations=%d error=%v", claims, observations, err)
			}
		})
	}
}

func testProviderSchedulerEndToEnd(t *testing.T, unavailable string) {
	t.Helper()
	batch := unavailable == "six slots" || unavailable == "provider slots" || unavailable == "mixed providers" || unavailable == "provider hydration" || unavailable == "stage slots" || unavailable == "quota reset"
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte("provider-test-admin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	httpClient := providerHandlerClient(service.Handler())
	const hubURL = "https://hub.test"
	admin, err := New(Config{URL: hubURL, TokenSource: func() string { return "provider-test-admin" }, HTTPClient: httpClient})
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
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "capacity", "idempotency_key": "capacity", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Rework", Dispatchable: true}}}, &project); err != nil {
		t.Fatal(err)
	}
	var otherProject tracker.NativeProject
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "other project", "idempotency_key": "other-project", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}, &otherProject); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, hubURL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{project.ID, otherProject.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "customer", DisplayName: "Runner", Capacity: 2, Version: "test"}
	if batch {
		machine.Capacity = 6
	}
	var enrolledIdentity runnerauth.Identity
	redemption := runnerauth.Redemption{BackendIsolation: machine.BackendIsolation, Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version}
	if err := admin.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/runner-enrollments/redeem", redemption, &enrolledIdentity); err != nil {
		t.Fatal(err)
	}
	file.Identity = enrolledIdentity
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: machine.Capacity, ProjectIDs: []tracker.ProjectID{project.ID, otherProject.ID}}}
	if unavailable == "plain" {
		routing.ProjectIDs = []tracker.ProjectID{project.ID}
	}
	if err := admin.request(t.Context(), http.MethodPut, "/api/v2/organizations/"+string(organization)+"/runners/"+file.Identity.RunnerID+"/routing", routing, nil); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: hubURL, IdentityFile: path, HTTPClient: httpClient})
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
	other, err := admin.Native(organization, otherProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other-unsupported"}, Title: "higher priority unsupported project", Body: "```detent-agent\nschema: 1\nmodel: astra\n```" + issueContractTestSections, State: "Todo", Priority: new(1)}); err != nil {
		t.Fatal(err)
	}
	if unavailable == "known waits" {
		for i := range 12 {
			if _, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("waiting-%d", i)}, Title: "Known waiting work", State: "Rework"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	titles := []string{"unsupported model", "compatible model"}
	if batch {
		titles = []string{"rework-0", "rework-1", "rework-2", "independent-0", "independent-1", "independent-2", "independent-3", "independent-4"}
	}
	for _, title := range titles {
		model := "sol"
		state := "Todo"
		if title == "unsupported model" {
			state = "Rework"
			if unavailable != "local wait" && unavailable != "plain" {
				model = "astra"
			}
		}
		if strings.HasPrefix(title, "rework-") {
			state = "Rework"
		}
		body := "```detent-agent\nschema: 1\nmodel: " + model + "\n```" + issueContractTestSections
		if _, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: title}, Title: title, Body: body, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now()}
	if batch {
		report.MaxConcurrent = 6
	}
	if unavailable == "provider slots" {
		report.MaxConcurrent = 2
	}
	if unavailable == "mixed providers" {
		report.MaxConcurrent = 1
	}
	if unavailable == "quota reset" {
		report.Availability = "exhausted"
		report.ResetAt = report.ObservedAt.Add(time.Hour)
	}
	providerReports := func() ([]providercapacity.Report, error) {
		reports := []providercapacity.Report{report}
		if unavailable == "mixed providers" {
			reports = append(reports, providercapacity.Report{Provider: "anthropic", Backend: "claude", AccountAlias: "other", Models: []string{"sonnet"}, MaxConcurrent: 2, Availability: "available", ObservedAt: report.ObservedAt})
		}
		return reports, nil
	}
	if unavailable == "plain" {
		providerReports = nil
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, ProviderReports: providerReports})
	if err != nil {
		t.Fatal(err)
	}
	backend, launches := providerWorkspaceWrapper(t)
	cfg := config.Default()
	cfg.Agents.ModelSelection.Preset = new("sol_first")
	modelUnavailable := "fallback"
	if unavailable == "fail" {
		modelUnavailable = "fail"
	}
	cfg.Agents.ModelSelection.Unavailable = &modelUnavailable
	localRunner, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &providerPreviewWorkspace{}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	request := orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor,
		WorkflowStates: []string{"Rework", "Todo"}, DispatchPriorityByState: []string{"Rework", "Todo"}, CandidateLimit: 2,
		CandidateKnownWait: func(issue connector.Issue) bool { return unavailable == "known waits" && issue.State == "Rework" },
		CandidateReady: func(_ context.Context, issue connector.Issue) bool {
			if unavailable == "known waits" && issue.State == "Rework" {
				t.Fatal("known wait consumed readiness evaluation")
			}
			return (unavailable != "local wait" && unavailable != "plain") || issue.State != "Rework"
		},
		ProviderRequirement: func(ctx context.Context, issue connector.Issue, reports []providercapacity.Report) (providercapacity.Requirement, error) {
			if (unavailable == "local wait" || unavailable == "plain") && issue.State == "Rework" {
				t.Fatal("unready head reached model selection")
			}
			return localRunner.DispatchCapacity(ctx, runner.RunRequest{Issue: issue, ProviderReports: reports})
		}}
	if unavailable == "intake off during admission" {
		request.CandidateAdmission = func(connector.Issue) (func(), bool) { return nil, false }
		issues, err := scheduler.FetchCandidateIssues(t.Context(), request)
		if err != nil || len(issues) != 0 || len(scheduler.nativeClaims) != 0 {
			t.Fatalf("intake-off preview claimed work: %v, %v", issues, err)
		}
		return
	}
	if batch {
		request.AdmissionLimit, request.CandidateLimit = 6, 14
		admittedRework := 0
		if unavailable == "stage slots" {
			request.CandidateAdmitted = func(issue connector.Issue) {
				if issue.State == "Rework" {
					admittedRework++
				}
			}
			request.CandidateReady = func(_ context.Context, issue connector.Issue) bool {
				return issue.State != "Rework" || admittedRework == 0
			}
		}
		ready := request.CandidateReady
		evaluations := 0
		request.CandidateReady = func(ctx context.Context, issue connector.Issue) bool {
			evaluations++
			return ready(ctx, issue)
		}
		request.ProviderRequirement = func(_ context.Context, issue connector.Issue, _ []providercapacity.Report) (providercapacity.Requirement, error) {
			if unavailable == "mixed providers" && issue.State == "Todo" {
				return providercapacity.Requirement{Role: runner.RoleCode, Backend: "claude", Model: "sonnet"}, nil
			}
			return providercapacity.Requirement{Role: runner.RoleCode, Backend: "codex", Model: "sol"}, nil
		}
		if unavailable == "quota reset" {
			now := report.ObservedAt
			scheduler.now = func() time.Time { return now }
			for range 4 {
				evaluations = 0
				if full, err := scheduler.FetchCandidateIssues(t.Context(), request); err != nil || len(full) != 0 || len(scheduler.nativeClaims) != 0 || evaluations > request.CandidateLimit {
					t.Fatalf("quota denial admitted work: %v, %v, retained=%d", full, err, len(scheduler.nativeClaims))
				}
			}
			now = now.Add(31 * time.Second)
			report.Availability = "available"
			report.ResetAt = time.Time{}
			report.ObservedAt = time.Now()
			evaluations = 0
		}
		transport := client.httpClient.Transport
		if unavailable == "provider hydration" {
			reads := 0
			client.httpClient.Transport = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && r.URL.Query().Get("view") == "recovery" {
					reads++
					if reads == 2 {
						return nil, errors.New("injected hydration failure")
					}
				}
				return transport.RoundTrip(r)
			})
			issues, err := scheduler.FetchCandidateIssues(t.Context(), request)
			if err == nil || len(issues) != 0 {
				t.Fatalf("provider hydration failure = %d, %v", len(issues), err)
			}
			client.httpClient.Transport = transport
			evaluations = 0
		}
		issues, err := scheduler.FetchCandidateIssues(t.Context(), request)
		want := 6
		if unavailable == "provider slots" {
			want = 2
		}
		if unavailable == "mixed providers" {
			want = 3
		}
		if err != nil || len(issues) != want || issues[0].State != "Rework" || evaluations > request.CandidateLimit {
			t.Fatalf("provider batch = %d, %v, want %d with Rework first", len(issues), err, want)
		}
		if unavailable == "stage slots" && admittedRework != 1 {
			t.Fatalf("overcommitted Rework slots: %d", admittedRework)
		}
		if unavailable == "provider slots" {
			for range 4 {
				evaluations = 0
				if full, err := scheduler.FetchCandidateIssues(t.Context(), request); err != nil || len(full) != 0 || evaluations > request.CandidateLimit {
					t.Fatalf("full provider refresh = %d, %v, evaluations=%d", len(full), err, evaluations)
				}
			}
			high, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "urgent after release", Body: "```detent-agent\nschema: 1\nmodel: sol\n```", State: "Rework", Priority: new(tracker.QueuePriorityUrgent)})
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.ReleaseClaim(t.Context(), issues[0].ID, "dispatch_deferred"); err != nil {
				t.Fatal(err)
			}
			evaluations = 0
			next, err := scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(next) != 1 || next[0].ID != string(high.WorkItemID) || len(scheduler.nativeClaims) != 2 {
				t.Fatalf("freed provider slot did not admit urgent work: %v, %v, retained=%d", next, err, len(scheduler.nativeClaims))
			}
			issues[0] = next[0]
		}
		used := make(map[string]int)
		for _, issue := range issues {
			execution := scheduler.RunExecution(issue.ID)
			if execution == nil {
				t.Fatal("claimed issue has no native execution")
			}
			reservation := execution.(runner.ProviderCapacityExecution).ProviderCapacity()
			if reservation == nil || scheduler.nativeClaims[issue.ID].lease.PolicyID != descriptor.ID {
				t.Fatal("batch lost provider reservation or immutable policy")
			}
			used[reservation.Backend]++
			if unavailable == "mixed providers" && issue.State == "Todo" && reservation.Backend != "claude" {
				t.Fatal("independent provider work was not admitted")
			}
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: reservation.Role, Backend: reservation.Backend, Model: reservation.Model}); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.ReleaseClaim(t.Context(), issue.ID, "dispatch_deferred"); err != nil {
				t.Fatal(err)
			}
		}
		if unavailable == "mixed providers" && (used["codex"] != 1 || used["claude"] != 2) {
			t.Fatalf("overcommitted providers: %v", used)
		}
		admittedRework = 0
		evaluations = 0
		if again, err := scheduler.FetchCandidateIssues(t.Context(), request); err != nil || len(again) != want {
			t.Fatalf("released provider reservations retained slots: %d, %v", len(again), err)
		}
		return
	}
	if unavailable == "bounded" {
		request.CandidateLimit = 1
		candidates, err := scheduler.FetchCandidateIssues(t.Context(), request)
		if err != nil && !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(candidates) != 0 {
			t.Fatalf("bounded unavailable head = %+v, %v", candidates, err)
		}
		request.CandidateLimit = 2
	}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 || candidates[0].Title != "compatible model" {
		t.Fatalf("selection = %+v, %v", candidates, err)
	}
	if *launches != 0 {
		t.Fatalf("provider scheduling launched wrapper %d times before workspace existed", *launches)
	}
	issue := candidates[0]
	if unavailable == "local wait" || unavailable == "bounded" || unavailable == "plain" || unavailable == "known waits" {
		if unavailable == "plain" && scheduler.nativeClaims[issue.ID].lease.ProviderReservation != nil {
			t.Fatal("plain claim acquired a provider reservation")
		}
		if err := scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
			t.Fatal(err)
		}
		return
	}
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
	if err := execution.Start(t.Context(), identity); !errors.Is(err, ErrUnavailable) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("lost start acknowledgment must retain transient transport identity: %v", err)
	}
	pending := execution.(*nativeExecution).pending
	if pending == nil || pending.Type != "run.started" || pending.Data.Sequence != 1 || pending.Data.Identity == nil || *pending.Data.Identity != identity {
		t.Fatalf("lost start acknowledgment lost pending execution identity: %#v", pending)
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
	recovery, err := native.Recovery(t.Context(), tracker.NativeWorkItemID(candidates[0].ID))
	if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Sequence != 1 {
		t.Fatalf("start retries duplicated or lost the durable attempt: attempts=%#v error=%v", recovery.Attempts, err)
	}
	// This attempt already occupies the provider's only slot. An ordinary
	// empty claim result keeps admission idle without inventing an outage.
	next, err := scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(next) != 0 {
		t.Fatalf("exhausted occupied capacity admitted another attempt: candidates=%#v error=%v", next, err)
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
		{"removed account limit", func(r *providercapacity.Report) { r.MaxConcurrent = 0 }, "sol", true},
		{"invalid report", func(r *providercapacity.Report) { r.MaxConcurrent = -1 }, "sol", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			test.change(&current)
			e := &nativeExecution{scheduler: &Scheduler{now: func() time.Time { return now }, providerReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{current}, nil }}, claim: nativeClaim{lease: tracker.NativeLease{ProviderReservation: &providercapacity.Reservation{Requirement: requirement, Report: report}}}}
			remote, _ := nativeSSHExecution(t, t.Context(), e, t.TempDir())
			if got := remote.(runner.ProviderCapacityExecution).ProviderCapacity(); !reflect.DeepEqual(got, e.ProviderCapacity()) {
				t.Fatalf("SSH changed the central reservation: %#v", got)
			}
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

func providerHandlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		incoming := request.Clone(request.Context())
		incoming.RequestURI = incoming.URL.RequestURI()
		handler.ServeHTTP(recorder, incoming)
		return recorder.Result(), nil
	})}
}
