package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClientEnrollmentSchedulingAndRotationRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	const adminToken = "runner-client-test-admin"
	const hubURL = "https://runner-hub.example.test"
	service, err := hubserver.Open(t.Context(), hubserver.Config{Content: newHubContentStore(), RunDetails: &archivedRunDetails{}, DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(adminToken), Version: "v1.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	var dropRotation atomic.Bool
	var pauseRotation atomic.Bool
	var credentialRenewals atomic.Int32
	var policyStatus atomic.Int64
	rotationEntered, rotationResume := make(chan struct{}), make(chan struct{})
	var resumeRotation sync.Once
	transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "runner-hub.example.test" {
			return nil, errors.New("unexpected runner Hub destination")
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/runners/") && strings.HasSuffix(r.URL.Path, "/renew") {
			credentialRenewals.Add(1)
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/leases/") && strings.HasSuffix(r.URL.Path, "/renew") {
			if status := policyStatus.Load(); status != 0 {
				if status < 0 {
					return nil, errors.New("policy transport unavailable")
				}
				recorder := httptest.NewRecorder()
				recorder.WriteHeader(int(status))
				if _, err := recorder.WriteString(`{"code":"unavailable","message":"policy read unavailable"}`); err != nil {
					return nil, err
				}
				return recorder.Result(), nil
			}
		}
		if strings.HasSuffix(r.URL.Path, "/rotate") && pauseRotation.CompareAndSwap(true, false) {
			close(rotationEntered)
			select {
			case <-rotationResume:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, r)
		if strings.HasSuffix(r.URL.Path, "/rotate") && dropRotation.CompareAndSwap(true, false) {
			if recorder.Code != http.StatusOK {
				t.Errorf("rotation failed before response loss: %d", recorder.Code)
			}
			return nil, errors.New("rotation response lost after commit")
		}
		return recorder.Result(), nil
	})
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	httpClient := &http.Client{Transport: transport}
	t.Cleanup(func() { resumeRotation.Do(func() { close(rotationResume) }) })
	admin, err := New(Config{URL: hubURL, TokenSource: func() string { return adminToken }, HTTPClient: httpClient})
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
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "runners", "idempotency_key": "runners", "require_dependencies": true, "states": []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Blocked"}},
		{Name: "Blocked", Transitions: []string{"Todo"}},
	}}, &project); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, hubURL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "customer", DisplayName: "Runner", Capacity: 1, Version: "test"}
	identity, err := EnrollRunner(t.Context(), path, organization, enrollment.Token, machine)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Binding != file.Identity.Binding {
		t.Fatal("enrollment changed the host identity")
	}
	if _, err := EnrollRunner(t.Context(), path, organization, enrollment.Token, machine); err != nil {
		t.Fatalf("lost enrollment response recovery: %v", err)
	}
	client, err := New(Config{URL: hubURL, IdentityFile: path, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if version, err := client.Version(t.Context()); err != nil || version != "v1.2.4" {
		t.Fatalf("enrolled runner Hub version = %q, %v, want v1.2.4", version, err)
	}
	native, err := client.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	adminNative, err := admin.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	supported, err := native.HubFeature(t.Context(), tracker.NativeLocalChecksCapability)
	if err != nil || !supported {
		t.Fatalf("current Hub diagnostic capability = %v, %v", supported, err)
	}
	fleetAdmin, err := NewFleetClient(admin, organization, map[string]tracker.ProjectID{"native": project.ID})
	if err != nil {
		t.Fatal(err)
	}
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Trusted builder", Tags: []string{"Build"}, State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{project.ID}, IsolationTier: "native-trusted", Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}}}
	if err := fleetAdmin.UpdateRunner(t.Context(), file.Identity.RunnerID, change); err != nil {
		t.Fatal(err)
	}
	if err := fleetAdmin.UpdateHost(t.Context(), machine.ID, runnerauth.HostChange{ExpectedRevision: 1, DisplayName: "Renamed host", Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	fleetWorker, err := NewFleetClient(client, organization, map[string]tracker.ProjectID{"native": project.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		fleet    *FleetClient
		editable bool
	}{{"administrator", fleetAdmin, true}, {"worker", fleetWorker, false}} {
		t.Run(test.name, func(t *testing.T) {
			view, err := test.fleet.Fleet(t.Context())
			if err != nil || view.Editable != test.editable || len(view.Runners) != 1 || view.Runners[0].DisplayName != "Trusted builder" || view.Runners[0].HostDisplayName != "Renamed host" {
				t.Fatalf("fleet = %#v, %v", view, err)
			}
		})
	}
	if err := fleetWorker.UpdateRunner(t.Context(), file.Identity.RunnerID, change); err == nil {
		t.Fatal("worker changed routing")
	}
	if err := fleetWorker.UpdateHost(t.Context(), machine.ID, runnerauth.HostChange{}); err == nil {
		t.Fatal("worker changed host")
	}
	t.Run("diagnostics without a project orchestrator", func(t *testing.T) {
		for _, test := range []struct {
			name         string
			report       isolation.Report
			wantProblems []string
			details      []runnerauth.Problem
		}{
			{"workflow unavailable", isolation.Report{"native/workflow": {}}, []string{"settings_invalid", "tier_unavailable"}, nil},
			{"backend unavailable", isolation.Report{"native/codex": {}}, []string{"backend_missing", "tier_unavailable"}, nil},
			{"empty report", isolation.Report{}, []string{"backend_missing", "tier_unavailable"}, nil},
			{"partially failed report", isolation.Report{"native/codex": {isolation.NativeTrusted}, "native/claude": {}}, []string{"backend_missing", "tier_unavailable"}, nil},
			{"signed out", isolation.Report{"native/codex": {}}, []string{"backend_missing", "tier_unavailable"}, []runnerauth.Problem{{Code: "backend_missing", Subject: "native/codex", Message: "not signed in", Check: "codex login status", ErrorOutput: "Not logged in", FixHint: "Sign in", FixCommand: "codex login --device-auth"}}},
		} {
			t.Run(test.name, func(t *testing.T) {
				now := time.Now()
				held := false
				probes, acquired, released := 0, 0, 0
				scheduler, err := NewScheduler(client, SchedulerConfig{
					OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine,
					HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, Now: func() time.Time { return now },
					LeaseHold: func(context.Context) (func(), error) {
						if held {
							t.Error("probe acquired a second hold")
						}
						held = true
						acquired++
						return func() { held = false; released++ }, nil
					},
					IsolationReport: func(context.Context) (isolation.Report, []runnerauth.Problem) {
						if !held {
							t.Error("isolation probe ran without keeping the Sprite awake")
						}
						probes++
						if probes == 1 {
							problem := runnerauth.NewProblem("tier_unavailable")
							problem.Message = "backend sandbox probe failed: sandbox tooling is unavailable"
							return test.report, append([]runnerauth.Problem{problem}, test.details...)
						}
						return machine.BackendIsolation, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				view, err := fleetAdmin.Fleet(t.Context())
				if err != nil || len(view.Runners) != 1 || view.Runners[0].Health != "needs_attention" {
					t.Fatalf("failed startup fleet = %#v, %v", view, err)
				}
				if len(view.Runners[0].Problems) != len(test.wantProblems) {
					t.Fatalf("startup problems = %+v, want %v", view.Runners[0].Problems, test.wantProblems)
				}
				for _, code := range test.wantProblems {
					found := false
					for _, problem := range view.Runners[0].Problems {
						found = found || problem.Code == code
						if problem.Code == "tier_unavailable" && problem.Message != "backend sandbox probe failed: sandbox tooling is unavailable" {
							t.Fatalf("probe reason lost: %+v", problem)
						}
					}
					if !found {
						t.Fatalf("startup did not report %s", code)
					}
				}
				if probes != 1 || acquired != 1 || released != 1 || held {
					t.Fatalf("startup probes=%d acquired=%d released=%d held=%t", probes, acquired, released, held)
				}
				now = now.Add(time.Minute)
				if err := scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				view, err = fleetAdmin.Fleet(t.Context())
				if err != nil || len(view.Runners[0].Problems) != 0 || view.Runners[0].Health != "online" {
					t.Fatalf("recovered startup fleet = %#v, %v", view, err)
				}
				if probes != 2 || acquired != 2 || released != 2 || held {
					t.Fatalf("recovery probes=%d acquired=%d released=%d held=%t", probes, acquired, released, held)
				}
				if !scheduler.machine.BackendIsolation.Supports(isolation.NativeTrusted) {
					t.Fatalf("recovered report = %#v", scheduler.machine.BackendIsolation)
				}
			})
		}
	})
	descriptor := clientTestPolicy()
	descriptor.Requirements = policy.Requirements{RequiredTags: []string{"build"}, RunnerID: file.Identity.RunnerID, MachineID: string(file.Identity.MachineID)}
	descriptor = descriptor.WithID()
	if _, err := adminNative.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	issue, err := adminNative.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Enrolled work", Body: issueContractTestSections, State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("old runner refusal retains the version in the logged error", func(t *testing.T) {
		oldMachine := machine
		oldMachine.Version = "v0.117.55"
		oldScheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: oldMachine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := oldScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
		var refusal *APIError
		if len(candidates) != 0 || !errors.As(err, &refusal) || refusal.Status != http.StatusUpgradeRequired || refusal.Message != "Too old to take work, needs 0.117.56" || !strings.Contains(err.Error(), refusal.Message) || errors.Is(err, ErrNoClaimableWork) {
			t.Fatalf("runner refusal lost its visible reason: candidates=%d, error=%v", len(candidates), err)
		}
	})
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
	if err != nil || len(candidates) != 1 || candidates[0].ID != string(issue.WorkItemID) {
		t.Fatalf("enrolled scheduler: candidates=%d err=%v", len(candidates), err)
	}
	cachedClaim, cacheErr := runnerauth.LoadRoutingCache(path)
	if cacheErr != nil || cachedClaim.Revision != 2 || cachedClaim.Routing.IsolationTier != "native-trusted" {
		t.Fatalf("claim routing cache = %#v, %v", cachedClaim, cacheErr)
	}
	candidates[0].IsolationPolicy = nil
	adopted, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
	if err != nil {
		t.Fatalf("runner-side validation: %v", err)
	}
	if adopted.Issue.IsolationPolicy == nil || adopted.Issue.IsolationPolicy.Tier != isolation.NativeTrusted {
		t.Fatalf("adopted isolation policy = %#v", adopted.Issue.IsolationPolicy)
	}
	t.Run("credential rotation preserves the active execution", func(t *testing.T) {
		execution := scheduler.RunExecution(string(issue.WorkItemID))
		guarded, stop, err := execution.Guard(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
			t.Fatal(err)
		}
		before, err := adminNative.Recovery(t.Context(), issue.WorkItemID)
		if err != nil || len(before.Attempts) != 1 {
			t.Fatalf("active attempt = %#v, %v", before.Attempts, err)
		}
		for _, test := range []struct {
			name   string
			status int64
		}{
			{name: "transport failure", status: -1},
			{name: "hub unavailable", status: http.StatusServiceUnavailable},
			{name: "rate limited", status: http.StatusTooManyRequests},
		} {
			t.Run(test.name, func(t *testing.T) {
				policyStatus.Store(test.status)
				defer policyStatus.Store(0)
				_, err := scheduler.RenewClaim(guarded, string(issue.WorkItemID), time.Now())
				if err == nil || errors.Is(err, orchestrator.ErrSchedulingClaimLost) || guarded.Err() != nil || scheduler.RunExecution(string(issue.WorkItemID)) != execution {
					t.Fatalf("instance failure discarded the active worker: %v", err)
				}
			})
		}
		original, err := runnerauth.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		rotationDone := make(chan error, 1)
		pauseRotation.Store(true)
		go func() {
			_, err := RefreshRunner(t.Context(), path, true)
			rotationDone <- err
		}()
		defer resumeRotation.Do(func() { close(rotationResume) })
		select {
		case <-rotationEntered:
		case err := <-rotationDone:
			t.Fatalf("rotation did not reach the credential owner: %v", err)
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		renewDone := make(chan error, 1)
		go func() {
			_, err := scheduler.RenewClaim(guarded, string(issue.WorkItemID), time.Now())
			renewDone <- err
		}()
		select {
		case err := <-renewDone:
			t.Fatalf("renewal did not wait for the in-process rotation: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if guarded.Err() != nil || scheduler.RunExecution(string(issue.WorkItemID)) != execution {
			t.Fatalf("rotation discarded the active worker: %v", context.Cause(guarded))
		}
		during, err := adminNative.Recovery(t.Context(), issue.WorkItemID)
		if err != nil || during.Issue.State != before.Issue.State || during.Issue.Revision != before.Issue.Revision || !reflect.DeepEqual(during.Attempts, before.Attempts) {
			t.Fatalf("contention changed the issue or attempt: %#v, %v", during, err)
		}
		resumeRotation.Do(func() { close(rotationResume) })
		if err := <-rotationDone; err != nil {
			t.Fatal(err)
		}
		if err := <-renewDone; errors.Is(err, orchestrator.ErrSchedulingClaimLost) || errors.Is(err, instancelock.ErrHeld) {
			t.Fatalf("renewal after rotation = %v", err)
		}
		if guarded.Err() != nil || scheduler.RunExecution(string(issue.WorkItemID)) != execution {
			t.Fatalf("renewal after rotation discarded the active worker: %v", context.Cause(guarded))
		}
		rotated, err := runnerauth.Load(path)
		expectedIdentity := original.Identity
		expectedIdentity.ExpiresAt = rotated.Identity.ExpiresAt
		if err != nil || rotated.Credential == original.Credential || rotated.PendingCredential != "" || rotated.Identity.ExpiresAt.Before(original.Identity.ExpiresAt) || !reflect.DeepEqual(rotated.Identity, expectedIdentity) {
			t.Fatalf("rotation did not preserve identity and grants: %v", err)
		}
		renewed, err := scheduler.RenewClaim(guarded, string(issue.WorkItemID), time.Now())
		if err != nil || renewed.Owner != adopted.Owner || !renewed.LeaseExpiresAt.After(adopted.LeaseExpiresAt) {
			t.Fatalf("heartbeat after rotation = %#v, %v", renewed, err)
		}
		if err := execution.Validate(guarded); err != nil || errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
			t.Fatalf("worker lost authority after rotation: %v", err)
		}
		after, err := adminNative.Recovery(t.Context(), issue.WorkItemID)
		if err != nil || len(after.Attempts) != 1 {
			t.Fatalf("rotation lost active attempt: attempts=%d, %v", len(after.Attempts), err)
		}
		// Renewal advances the current lease projection, not attempt history.
		expectedAttempt := before.Attempts[0]
		expectedAttempt.LeaseRenewedAt = after.Attempts[0].LeaseRenewedAt
		expectedAttempt.LeaseExpiresAt = after.Attempts[0].LeaseExpiresAt
		if !after.Attempts[0].LeaseRenewedAt.After(before.Attempts[0].LeaseRenewedAt) || !after.Attempts[0].LeaseExpiresAt.Equal(renewed.LeaseExpiresAt) || after.Issue.State != before.Issue.State || after.Issue.Revision != before.Issue.Revision || !reflect.DeepEqual(after.Attempts[0], expectedAttempt) {
			t.Fatal("rotation changed the issue or attempt beyond the renewed lease timestamps")
		}
	})
	eligibility, err := fleetAdmin.ProjectEligibility(t.Context(), "native")
	if err != nil || len(eligibility.Exclusions) != 1 || len(eligibility.Runners) != 1 {
		t.Fatalf("occupied project eligibility = %#v, %v", eligibility, err)
	}
	if _, err := fleetAdmin.ProjectEligibility(t.Context(), "unknown"); err == nil {
		t.Fatal("unknown project widened selection")
	}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	cached, err := runnerauth.LoadRoutingCache(path)
	if err != nil || cached.Revision != 2 || cached.Routing.IsolationTier != "native-trusted" || len(cached.Routing.ProjectIDs) != 1 {
		t.Fatalf("heartbeat routing cache = %#v, %v", cached, err)
	}
	if err := os.Chmod(runnerauth.RoutingCachePath(path), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatalf("heartbeat with validated in-memory routing: %v", err)
	}
	if _, err := runnerauth.LoadRoutingCache(path); runtime.GOOS != "windows" && err == nil {
		t.Fatal("accepted an insecure routing cache")
	}
	if err := os.Chmod(runnerauth.RoutingCachePath(path), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err = runnerauth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		expiresIn time.Duration
	}{
		{name: "near expiry", expiresIn: time.Minute},
		{name: "expired", expiresIn: -time.Minute},
	} {
		t.Run("concurrent credential renewal "+test.name, func(t *testing.T) {
			file.Identity.ExpiresAt = time.Now().Add(test.expiresIn)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			lockPath := path + ".lock"
			old := time.Unix(946684800, 0)
			if err := os.Chtimes(lockPath, old, old); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			initialRenewals := credentialRenewals.Load()
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 8 {
				concurrent, err := New(Config{URL: hubURL, IdentityFile: path, HTTPClient: httpClient})
				if err != nil {
					t.Fatal(err)
				}
				concurrentNative, err := concurrent.Native(organization, project.ID)
				if err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					<-start
					got, err := concurrentNative.Project(t.Context())
					if err != nil || got.ID != project.ID {
						t.Errorf("concurrent renewed project read: %+v, %v", got, err)
					}
				})
			}
			close(start)
			wg.Wait()
			if got := credentialRenewals.Load() - initialRenewals; got != 1 {
				t.Fatalf("concurrent credential renewals = %d, want 1", got)
			}
			after, err := os.Stat(lockPath)
			if err != nil || after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("credential renewal did not persist lock ownership: %v", err)
			}
			persisted, err := runnerauth.Load(path)
			if err != nil || !persisted.Identity.ExpiresAt.After(file.Identity.ExpiresAt.Add(time.Hour)) {
				t.Fatalf("concurrent credential renewal not persisted: %v", err)
			}
		})
	}
	renewed, err := runnerauth.Load(path)
	if err != nil || !renewed.Identity.ExpiresAt.After(file.Identity.ExpiresAt.Add(time.Hour)) {
		t.Fatalf("automatic renewal not persisted: %v", err)
	}
	unassigned := change
	unassigned.ExpectedRevision = 2
	unassigned.ProjectIDs = []tracker.ProjectID{}
	if err := fleetAdmin.UpdateRunner(t.Context(), file.Identity.RunnerID, unassigned); err != nil {
		t.Fatal(err)
	}
	renewed.Identity.ExpiresAt = time.Now().Add(time.Minute)
	if err := runnerauth.Save(path, renewed); err != nil {
		t.Fatal(err)
	}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatalf("unassigned runner heartbeat after credential renewal: %v", err)
	}
	unassignedFile, err := runnerauth.Load(path)
	if err != nil || len(unassignedFile.Identity.ProjectIDs) != 0 || !unassignedFile.Identity.ExpiresAt.After(renewed.Identity.ExpiresAt.Add(time.Hour)) {
		t.Fatalf("unassigned runner renewal = %#v, %v", unassignedFile.Identity, err)
	}
	cached, err = runnerauth.LoadRoutingCache(path)
	if err != nil || cached.Revision != 3 || len(cached.Routing.ProjectIDs) != 0 {
		t.Fatalf("unassigned runner routing cache = %#v, %v", cached, err)
	}
	reassigned := change
	reassigned.ExpectedRevision = 3
	if err := fleetAdmin.UpdateRunner(t.Context(), file.Identity.RunnerID, reassigned); err != nil {
		t.Fatal(err)
	}
	unassignedFile.Identity.ExpiresAt = time.Now().Add(time.Minute)
	if err := runnerauth.Save(path, unassignedFile); err != nil {
		t.Fatal(err)
	}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatalf("reassigned runner heartbeat after credential renewal: %v", err)
	}
	reassignedFile, err := runnerauth.Load(path)
	if err != nil || len(reassignedFile.Identity.ProjectIDs) != 1 || reassignedFile.Identity.ProjectIDs[0] != project.ID {
		t.Fatalf("reassigned runner renewal = %#v, %v", reassignedFile.Identity, err)
	}
	dropRotation.Store(true)
	if _, err := RefreshRunner(t.Context(), path, true); err == nil {
		t.Fatal("dropped rotation response reported success")
	}
	pending, err := runnerauth.Load(path)
	if err != nil || pending.PendingCredential == "" || pending.Credential != file.Credential {
		t.Fatalf("interrupted rotation not recoverable: %v", err)
	}
	restarted, err := New(Config{URL: hubURL, IdentityFile: path, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := restarted.RunnerIdentity(t.Context()); err != nil || recovered.Binding != pending.Identity.Binding || !slices.Equal(recovered.ProjectIDs, reassignedFile.Identity.ProjectIDs) {
		t.Fatalf("identity refresh with pending credential: %#v, %v", recovered, err)
	}
	rotated, err := runnerauth.Load(path)
	if err != nil || rotated.PendingCredential != "" || rotated.Credential != pending.PendingCredential {
		t.Fatalf("rotation not finalized: %v", err)
	}
	if _, err := RefreshRunner(t.Context(), path, false); err != nil {
		t.Fatal(err)
	}
	t.Run("stale fencing token remains fatal", func(t *testing.T) {
		stale, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		id := string(issue.WorkItemID)
		claim := scheduler.nativeClaims[id]
		claim.lease.FencingToken++
		stale.nativeClaims[id] = claim
		stale.claims[id] = nativeTrackerLease(claim.lease)
		stale.claimPolicies[id] = scheduler.claimPolicies[id]
		_, err = stale.RenewClaim(t.Context(), id, time.Now())
		var failure *APIError
		if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) || !errors.As(err, &failure) || failure.Code != "stale_fencing_token" {
			t.Fatalf("stale heartbeat retained authority: %v", err)
		}
		if _, err := scheduler.RenewClaim(t.Context(), id, time.Now()); err != nil {
			t.Fatalf("stale heartbeat disturbed the active lease: %v", err)
		}
	})
	execution := scheduler.RunExecution(string(issue.WorkItemID))
	if execution == nil {
		t.Fatal("claimed issue has no native execution")
	}
	if err := execution.Finish(t.Context(), "interrupted"); err != nil {
		t.Fatal(err)
	}
	retained := scheduler.nativeClaims[string(issue.WorkItemID)].lease
	if err := scheduler.ReleaseClaim(t.Context(), string(issue.WorkItemID), "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Renew(t.Context(), retained, 90); err != nil {
		t.Fatalf("interruption released the original lease: %v", err)
	}
	resumedScheduler, err := NewScheduler(restarted, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resumedCandidates, err := resumedScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
	if err != nil || len(resumedCandidates) != 1 || resumedCandidates[0].ID != string(issue.WorkItemID) {
		t.Fatalf("incomplete interruption did not reconnect: candidates=%+v error=%v", resumedCandidates, err)
	}
	if _, err := resumedScheduler.AdoptClaim(t.Context(), resumedCandidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	resumedLease := resumedScheduler.nativeClaims[string(issue.WorkItemID)].lease
	if resumedLease.ID != retained.ID || resumedLease.FencingToken != retained.FencingToken {
		t.Fatalf("incomplete interruption replaced reconnect authority: old=%+v new=%+v", retained, resumedLease)
	}
	t.Log("genuine incomplete interruption retained its original lease/fence on reconnect")
	resumedExecution := resumedScheduler.RunExecution(string(issue.WorkItemID))
	if resumedExecution == nil {
		t.Fatal("resumed claim has no native execution")
	}
	if err := resumedExecution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	current, err := adminNative.Issue(t.Context(), issue.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	held, err := adminNative.Transition(t.Context(), issue.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Blocked", Reason: "user_requested"})
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := adminNative.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Unfinished prerequisite", Body: "Create the inventory schema and migrate existing storage.", State: "Blocked"})
	if err != nil {
		t.Fatal(err)
	}
	held, err = adminNative.Dependency(t.Context(), issue.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: held.Revision, RelatedWorkItemID: prerequisite.WorkItemID, Operation: "add"})
	if err != nil {
		t.Fatal(err)
	}
	heldScheduler, err := NewScheduler(restarted, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	heldCandidates, err := heldScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
	if err != nil || len(heldCandidates) != 0 {
		t.Fatalf("operator hold recovered as runnable: candidates=%+v error=%v", heldCandidates, err)
	}
	current, err = adminNative.Issue(t.Context(), issue.WorkItemID)
	if err != nil || current.State != "Blocked" || current.Revision != held.Revision || current.DependencyReady {
		t.Fatalf("recovery changed the operator hold: %+v, %v", current, err)
	}
	if _, err := native.Renew(t.Context(), retained, 90); !nativeLeaseLost(err) {
		t.Fatalf("recovery retained the held lease: %v", err)
	}
	heldRecovery, err := adminNative.Recovery(t.Context(), issue.WorkItemID)
	if err != nil || len(heldRecovery.Attempts) != 1 || heldRecovery.Attempts[0].Outcome != "cancelled" || heldRecovery.Attempts[0].ClaimReleasedAt == nil {
		t.Fatalf("recovery retained a running attempt: %+v, %v", heldRecovery.Attempts, err)
	}
	if _, err := adminNative.Transition(t.Context(), issue.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Todo", Reason: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                               string
		slots, cloud, host, reported, want int
	}{
		{"window limit", 2, 8, 8, 8, 2},
		{"cloud limit", 6, 1, 8, 8, 1},
		{"host limit", 6, 8, 1, 8, 1},
		{"reported limit", 6, 8, 8, 1, 1},
		{"zero window limit", 0, 8, 8, 8, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fleet, err := fleetAdmin.Fleet(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			current := fleet.Runners[0]
			routing := current.Routing
			routing.CapacityLimit = test.cloud
			routing.Availability = runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}, WindowSlots: map[string]int{"Mon-Sun 00:00-24:00": test.slots}}
			if err := fleetAdmin.UpdateRunner(t.Context(), current.RunnerID, runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: routing}); err != nil {
				t.Fatal(err)
			}
			if err := fleetAdmin.UpdateHost(t.Context(), machine.ID, runnerauth.HostChange{ExpectedRevision: current.HostRevision, DisplayName: current.HostDisplayName, Capacity: test.host}); err != nil {
				t.Fatal(err)
			}
			report := machine
			report.Capacity = test.reported
			now := time.Now().UTC()
			report.CapacityConfig = &runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: now}
			report.ProviderReports = []providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "capacity-test", Models: []string{"sol"}, MaxConcurrent: 8, Availability: "available", ObservedAt: now}}
			if err := native.HeartbeatMachine(t.Context(), report); err != nil {
				t.Fatal(err)
			}
			var capacity runnerauth.CapacityView
			if err := admin.request(t.Context(), http.MethodGet, "/api/v2/organizations/"+string(organization)+"/runners/"+current.RunnerID+"/capacity?backend=codex", nil, &capacity); err != nil {
				t.Fatal(err)
			}
			if capacity.Effective == nil || *capacity.Effective != test.want {
				t.Fatalf("effective capacity = %+v, want %d", capacity, test.want)
			}
		})
	}
	if err := admin.RevokeRunner(t.Context(), organization, identity.Binding); err != nil {
		t.Fatal(err)
	}
	_, err = native.Project(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("revoked runner remained authorized: %v", err)
	}
	if _, err := New(Config{URL: "https://different.example.test", IdentityFile: path}); err == nil {
		t.Fatal("credential accepted at a different Hub")
	}
}

func TestRunnerRequestsDoNotFollowCredentialRedirects(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	var forwarded atomic.Int64
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(recipient.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, recipient.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "example-enrollment-token" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateRunnerEnrollment(t.Context(), "org_example", runnerauth.EnrollmentRequest{})
	if err == nil || forwarded.Load() != 0 {
		t.Fatalf("credential redirect followed: requests=%d err=%v", forwarded.Load(), err)
	}
}

func TestIsolationProbeDoesNotHoldSchedulerMutex(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var held atomic.Bool
	scheduler := &Scheduler{client: client, now: time.Now, heartbeatInterval: time.Second, machine: Machine{ID: "machine", Capacity: 1}, nativeHeartbeats: map[tracker.ProjectID]time.Time{"prj_test": time.Now().Add(-time.Minute)}, leaseHold: func(context.Context) (func(), error) {
		held.Store(true)
		return func() { held.Store(false) }, nil
	}, isolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
		if !held.Load() {
			t.Error("isolation probe ran without keeping the Sprite awake")
		}
		close(entered)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute {
			t.Error("probe has no bounded aggregate deadline")
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return isolation.Report{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.ensureNativeMachine(ctx, &NativeConnector{client: native}) }()
	<-entered
	acquired := make(chan struct{})
	go func() { scheduler.mu.Lock(); close(acquired); scheduler.mu.Unlock() }()
	select {
	case <-acquired:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("probe blocked scheduler mutex")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("probe exceeded aggregate deadline")
	}
	if held.Load() {
		t.Fatal("canceled probe left its Sprite task held")
	}
}

func TestBlockedMachineReportDoesNotSerializeLeaseRenewal(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	approved := clientTestPolicy()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var registrations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prj_blocked/machines/register"):
			if registrations.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
				w.WriteHeader(http.StatusNoContent)
			case <-r.Context().Done():
			}

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/policy/observed"):
			w.WriteHeader(http.StatusNoContent)

		case strings.HasSuffix(r.URL.Path, "/policy"):
			_ = json.NewEncoder(w).Encode(policy.Approval{Policy: approved})
		case strings.HasSuffix(r.URL.Path, "/policy/observed"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_ = json.NewEncoder(w).Encode(runnerauth.Runner{})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-items/wi_example"):
			_ = json.NewEncoder(w).Encode(tracker.NativeIssue{SchedulingEligible: new(true)})
		case strings.HasSuffix(r.URL.Path, "/renew"):
			var mutation tracker.NativeLeaseMutation
			if err := json.NewDecoder(r.Body).Decode(&mutation); err != nil {
				t.Error(err)
			}
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(tracker.NativeLease{ID: "lease", PolicyID: approved.ID, FencingToken: mutation.FencingToken, ServerTime: now, ExpiresAt: now.Add(time.Minute)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prj_free/leases/lease/release"):
			var mutation tracker.NativeLeaseMutation
			if err := json.NewDecoder(r.Body).Decode(&mutation); err != nil {
				t.Error(err)
			}
			if mutation.FencingToken != 1 || mutation.Reason != "cancelled" || !mutation.CancelAttempt {
				t.Errorf("release mutation = %+v", mutation)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	defer cancel()
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"blocked": "prj_blocked", "free": "prj_free"},
		Machine:           Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 2},
		HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleHeartbeat := time.Now().Add(-time.Minute)
	scheduler.nativeHeartbeats["prj_blocked"] = staleHeartbeat
	scheduler.nativeHeartbeats["prj_free"] = time.Now().Add(time.Hour)
	free := scheduler.nativeProjects["free"]
	install := func(id, policyID string, token tracker.FencingToken) nativeClaim {
		claim := nativeClaim{source: free, lease: tracker.NativeLease{ID: "lease", WorkItemID: "wi_example", PolicyID: policyID, FencingToken: token}}
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		scheduler.nativeClaims[id] = claim
		scheduler.claims[id] = nativeTrackerLease(claim.lease)
		scheduler.claimPolicies[id] = claimPolicy{project: "free", descriptor: approved}
		return claim
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.ensureNativeMachine(ctx, scheduler.nativeProjects["blocked"]) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("machine report returned before reaching registration: %v", err)
	case <-ctx.Done():
		t.Fatal("machine registration did not start")
	}
	for _, tc := range []struct {
		name      string
		policyID  string
		renewed   tracker.FencingToken
		current   tracker.FencingToken
		wantLost  bool
		wantToken tracker.FencingToken
		retained  bool
	}{
		{name: "unrelated lease renews", policyID: approved.ID, renewed: 1, current: 1, wantToken: 1, retained: true},
		{name: "stale pinned policy drops the claim", policyID: "stale", renewed: 1, current: 1, wantLost: true},
		{name: "superseded fencing token cannot reinstall", policyID: approved.ID, renewed: 1, current: 2, wantLost: true, wantToken: 2, retained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := install(tc.name, tc.policyID, tc.renewed)
			if tc.current != tc.renewed {
				install(tc.name, tc.policyID, tc.current)
			}
			result := make(chan error, 1)
			go func() {
				_, err := scheduler.renewNativeClaim(ctx, tc.name, stale, 0)
				result <- err
			}()
			select {
			case err := <-result:
				if lost := errors.Is(err, orchestrator.ErrSchedulingClaimLost); lost != tc.wantLost || !lost && err != nil {
					t.Fatalf("renewNativeClaim() error = %v, want lost %t", err, tc.wantLost)
				}
			case <-ctx.Done():
				t.Fatal("lease renewal waited on another project's machine report")
			}
			scheduler.mu.Lock()
			current, ok := scheduler.nativeClaims[tc.name]
			scheduler.mu.Unlock()
			if ok != tc.retained || ok && current.lease.FencingToken != tc.wantToken {
				t.Fatalf("claim after renewal = %+v, present %t", current.lease, ok)
			}
		})
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("machine report did not finish")
	}
	if got := registrations.Load(); got != 1 {
		t.Fatalf("machine reports = %d, want 1", got)
	}
	scheduler.mu.Lock()
	reported := scheduler.nativeHeartbeats["prj_blocked"]
	scheduler.mu.Unlock()
	if !reported.After(staleHeartbeat) {
		t.Fatalf("blocked project heartbeat not committed: %v", reported)
	}
}

func TestRunnerAvailabilityHeartbeatAndClaim(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_test"
	file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	closed := runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun " + now.Add(time.Hour).Format("15:04") + "-" + now.Add(2*time.Hour).Format("15:04")}}
	snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 3, Availability: closed}.Normalized()}
	inventory := []skills.ProviderSkill{{Name: "plan", Scope: "project", Path: ".agents/skills/plan/SKILL.md", Invocation: "$plan", Enabled: true, UserInvocable: true}}
	var capacities []int
	claims := 0
	transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		body := `{}`
		if strings.HasSuffix(request.URL.Path, "/capabilities") {
			body = `{"features":["runner_project_skills"]}`
		} else if strings.HasSuffix(request.URL.Path, "/heartbeat") {
			var report struct {
				Capacity int                    `json:"capacity"`
				Skills   []skills.ProviderSkill `json:"skills"`
			}
			if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(report.Skills, inventory) {
				t.Errorf("heartbeat skill inventory = %#v, want %#v", report.Skills, inventory)
			}
			capacities = append(capacities, report.Capacity)
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		} else if strings.HasSuffix(request.URL.Path, "/claims") {
			claims++
			body = `{"isolation_policy":{"tier":"sandbox"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Capacity: 3, Version: "test", Skills: &inventory}
	if err := native.RegisterMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	if len(capacities) == 0 || capacities[0] != 0 || capacities[len(capacities)-1] != 0 {
		t.Fatalf("closed capacities = %v", capacities)
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
		t.Fatalf("closed claim = %v, calls = %d", err, claims)
	}
	restarted, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err = restarted.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
		t.Fatalf("cached claim = %v, calls = %d", err, claims)
	}
	snapshot.Revision++
	snapshot.Routing.Availability = runnerauth.Availability{}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	if capacities[len(capacities)-1] != 3 {
		t.Fatalf("reopened capacities = %v", capacities)
	}
	for _, test := range []struct {
		name  string
		slots map[string]int
		want  int
	}{
		{"counted", map[string]int{"Mon-Sun 00:00-24:00": 2}, 2},
		{"lower count", map[string]int{"Mon-Sun 00:00-24:00": 1}, 1},
		{"above reported", map[string]int{"Mon-Sun 00:00-24:00": 6}, 3},
		{"zero count", map[string]int{"Mon-Sun 00:00-24:00": 0}, 0},
		{"uncounted", nil, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot.Revision++
			snapshot.Routing.Availability = runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}, WindowSlots: test.slots}
			if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			if capacities[len(capacities)-1] != test.want {
				t.Fatalf("capacities = %v, want latest %d", capacities, test.want)
			}
			capacities = nil
			if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			if len(capacities) != 1 || capacities[0] != test.want {
				t.Fatalf("cached capacities = %v, want [%d]", capacities, test.want)
			}
			if test.want == 0 {
				if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
					t.Fatalf("zero-count claim = %v, calls = %d", err, claims)
				}
			}
		})
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); err != nil || claims != 1 {
		t.Fatalf("open claim = %v, calls = %d", err, claims)
	}
}

func TestUnchangedCredentialReadLeavesLockMetadataUnwritten(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		clients  int
		lockFile bool
	}{
		{name: "repeated requests", clients: 1, lockFile: true},
		{name: "concurrent clients", clients: 8, lockFile: true},
		{name: "missing lock file", clients: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lockPath := path + ".lock"
			var lockInfo os.FileInfo
			if test.lockFile {
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Unix(946684800, 0)
				if err := os.Chtimes(lockPath, old, old); err != nil {
					t.Fatal(err)
				}
				lockInfo, err = os.Stat(lockPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example" || r.Header.Get("Authorization") != "Bearer "+file.Credential {
					t.Errorf("unexpected authenticated project request: %s %s", r.Method, r.URL.Path)
				}
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"project_id":"prj_example","organization_id":"org_example"}`))}, nil
			})
			var wg sync.WaitGroup
			for range test.clients {
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					for range 3 {
						project, err := native.Project(t.Context())
						if err != nil || project.ID != "prj_example" || project.OrganizationID != "org_example" {
							t.Errorf("authenticated project read: %+v, %v", project, err)
						}
					}
				})
			}
			wg.Wait()
			if got := requests.Load(); got != int32(test.clients*3) {
				t.Fatalf("authenticated requests = %d, want %d", got, test.clients*3)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unchanged credential read modified the identity: %v", err)
			}
			afterLock, err := os.Stat(lockPath)
			if !test.lockFile {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unchanged credential read created a persistent lock file: %v", err)
				}
			} else if err != nil || !os.SameFile(lockInfo, afterLock) || !afterLock.ModTime().Equal(lockInfo.ModTime()) {
				t.Fatalf("a GET using an unchanged valid credential rewrote persistent lock metadata: %v", err)
			}
		})
	}
}

func TestLockIdentityFileSerializesCallersInOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	for _, test := range []struct {
		name    string
		callers int
	}{
		{name: "two callers", callers: 2},
		{name: "many callers", callers: 32},
	} {
		t.Run(test.name, func(t *testing.T) {
			var holders, maxHolders atomic.Int32
			var wg sync.WaitGroup
			errs := make(chan error, test.callers)
			for range test.callers {
				wg.Go(func() {
					release, err := lockIdentityFile(path)
					if err != nil {
						errs <- err
						return
					}
					current := holders.Add(1)
					for {
						seen := maxHolders.Load()
						if current <= seen || maxHolders.CompareAndSwap(seen, current) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					holders.Add(-1)
					if err := release(); err != nil {
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("lockIdentityFile() error = %v", err)
			}
			if maxHolders.Load() != 1 {
				t.Fatalf("concurrent holders = %d, want 1", maxHolders.Load())
			}
		})
	}
}

func TestLockIdentityFileRefusesHeldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: path})
	if err != nil {
		t.Fatal(err)
	}
	held, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := lockIdentityFile(path); !errors.Is(err, instancelock.ErrHeld) {
		t.Fatalf("lockIdentityFile() error = %v, want ErrHeld", err)
	}
	for _, test := range []struct {
		name    string
		expired bool
		pending bool
		refresh bool
	}{
		{name: "unchanged read"},
		{name: "expired credential", expired: true},
		{name: "pending rotation", pending: true},
		{name: "identity refresh", refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if test.expired {
				file.Identity.ExpiresAt = time.Now().Add(-time.Minute)
			}
			file.PendingCredential = ""
			if test.pending {
				file.PendingCredential = file.Credential
			}
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			got, err := client.runnerCredential(t.Context(), test.refresh)
			if test.expired || test.pending || test.refresh {
				if !errors.Is(err, instancelock.ErrHeld) {
					t.Fatalf("credential mutation bypassed the writer lock: %v", err)
				}
			} else if err != nil || got.Credential != file.Credential {
				t.Fatalf("unchanged credential read required a writer lock: %v", err)
			}
		})
	}
	release, err := lockIdentityFile(filepath.Join(t.TempDir(), "other.json"))
	if err != nil {
		t.Fatalf("lockIdentityFile(other) error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerReaderReconcilesAtomicRotation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		pending   bool
		protected bool
		expired   bool
		wrongHost bool
		wantErr   string
	}{
		{name: "pending credential accepted", pending: true},
		{name: "rotation persisted"},
		{name: "pending rotation during protected read", pending: true, protected: true},
		{name: "persisted rotation during protected read", protected: true},
		{name: "expired rotation during protected read", protected: true, expired: true, wantErr: "expired"},
		{name: "Hub binding changed during protected read", protected: true, wrongHost: true, wantErr: "binding cannot be replaced"},
		{name: "expired Hub response", expired: true, wantErr: "expired"},
		{name: "Hub binding changed", wrongHost: true, wantErr: "binding cannot be replaced"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			replacement, err := runnerauth.Initialize(filepath.Join(t.TempDir(), "private", "replacement.json"), file.HubURL)
			if err != nil {
				t.Fatal(err)
			}
			calls, protectedCalls := 0, 0
			var rotated []byte
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatalf("reader attempted %s", r.Method)
				}
				status := http.StatusOK
				body := `{"code":"unauthorized"}`
				protected := strings.Contains(r.URL.Path, "/projects/")
				if protected {
					protectedCalls++
				}
				if tt.protected && calls == 1 {
					raw, err := json.Marshal(file.Identity)
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, err
				}
				if !tt.protected && calls == 1 || tt.protected && protectedCalls == 1 && protected {
					release, err := lockIdentityFile(path)
					if err != nil {
						return nil, err
					}
					file.PendingCredential = replacement.Credential
					if !tt.pending {
						file.Credential, file.PendingCredential = replacement.Credential, ""
					}
					if err := errors.Join(runnerauth.Save(path, file), release()); err != nil {
						return nil, err
					}
					rotated, err = os.ReadFile(path)
					if err != nil {
						return nil, err
					}
					if tt.wrongHost {
						file.HubURL = "https://other.example.test"
						rotated, err = json.Marshal(file)
						if err != nil {
							return nil, err
						}
						if err := os.WriteFile(path, rotated, 0600); err != nil {
							return nil, err
						}
					}

					status = http.StatusUnauthorized
				} else {
					if r.Header.Get("Authorization") != "Bearer "+replacement.Credential {
						t.Fatal("reader did not use the rotated credential")
					}
					identity := file.Identity
					if tt.expired {
						identity.ExpiresAt = time.Now().Add(-time.Hour)
					}
					raw, err := json.Marshal(identity)
					if protected {
						raw, err = json.Marshal(tracker.NativeProject{ID: "prj_example", OrganizationID: "org_example"})
					}
					if err != nil {
						return nil, err
					}
					body = string(raw)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			client, err := NewRunnerReader(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			if tt.protected {
				native, nativeErr := client.Native("org_example", "prj_example")
				if nativeErr != nil {
					t.Fatal(nativeErr)
				}
				remote, readErr := native.Project(t.Context())
				err = readErr
				if tt.wantErr == "" && (err != nil || remote.ID != "prj_example" || remote.OrganizationID != "org_example") {
					t.Fatalf("protected project = %+v, %v", remote, err)
				}
			} else {
				var identity runnerauth.Identity
				identity, err = client.RunnerIdentity(t.Context())
				if tt.wantErr == "" && identity.Binding != file.Identity.Binding {
					t.Fatalf("reader identity = %+v, want binding %+v", identity, file.Identity.Binding)
				}
			}
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("reader error = %v; want %q", err, tt.wantErr)
			}
			if !tt.protected && calls > 2 || calls > 4 || protectedCalls > 2 {
				t.Fatalf("reader requests = %d, want at most two credential reads", calls)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(rotated) {
				t.Fatalf("reader overwrote rotation: %v", err)
			}
		})
	}
}
