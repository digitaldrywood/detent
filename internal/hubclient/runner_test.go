package hubclient

import (
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
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClientEnrollmentSchedulingAndRotationRecovery(t *testing.T) {
	const adminToken = "runner-client-test-admin"
	const hubURL = "https://runner-hub.example.test"
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(adminToken), Version: "v1.2.4"})
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
	var policyStatus atomic.Int64
	rotationEntered, rotationResume := make(chan struct{}), make(chan struct{})
	var resumeRotation sync.Once
	transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "runner-hub.example.test" {
			return nil, errors.New("unexpected runner Hub destination")
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/policy") {
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
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "runners", "idempotency_key": "runners", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}, &project); err != nil {
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
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Trusted builder", Tags: []string{"Build"}, State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{project.ID}, IsolationTier: "native-trusted", Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, Spillover: runnerauth.Spillover{Mode: "after", AfterMinutes: 0}}}
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
		}{
			{"workflow unavailable", isolation.Report{"native/workflow": {}}, []string{"settings_invalid", "tier_unavailable"}},
			{"backend unavailable", isolation.Report{"native/codex": {}}, []string{"backend_missing", "tier_unavailable"}},
			{"empty report", isolation.Report{}, []string{"backend_missing", "tier_unavailable"}},
			{"partially failed report", isolation.Report{"native/codex": {isolation.NativeTrusted}, "native/claude": {}}, []string{"backend_missing", "tier_unavailable"}},
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
					IsolationReport: func(context.Context) isolation.Report {
						if !held {
							t.Error("isolation probe ran without keeping the Sprite awake")
						}
						probes++
						if probes == 1 {
							return test.report
						}
						return machine.BackendIsolation
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
	issue, err := adminNative.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Enrolled work", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("old runner refusal retains the version in the logged error", func(t *testing.T) {
		oldMachine := machine
		oldMachine.Version = "v1.2.3"
		oldScheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"native": project.ID}, Machine: oldMachine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := oldScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
		var refusal *APIError
		if len(candidates) != 0 || !errors.As(err, &refusal) || refusal.Status != http.StatusUpgradeRequired || refusal.Message != "Too old to take work, needs v1.2.4" || !strings.Contains(err.Error(), refusal.Message) || errors.Is(err, ErrNoClaimableWork) {
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
		_, err = scheduler.RenewClaim(guarded, string(issue.WorkItemID), time.Now())
		if !errors.Is(err, instancelock.ErrHeld) || errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
			t.Fatalf("credential-owner contention = %v", err)
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
	if err != nil || cached.Revision != 2 || cached.Routing.IsolationTier != "native-trusted" || cached.Routing.Spillover.Mode != "after" || len(cached.Routing.ProjectIDs) != 1 {
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
	file.Identity.ExpiresAt = time.Now().Add(time.Minute)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Project(t.Context()); err != nil {
		t.Fatal(err)
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
	if err := restarted.request(t.Context(), http.MethodGet, "/api/v2/capabilities", nil, nil); err != nil {
		t.Fatalf("restart with pending credential: %v", err)
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
	if err := scheduler.ReleaseClaim(t.Context(), string(issue.WorkItemID), "interrupted"); err != nil {
		t.Fatal(err)
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
	}, isolationReport: func(ctx context.Context) isolation.Report {
		if !held.Load() {
			t.Error("isolation probe ran without keeping the Sprite awake")
		}
		close(entered)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("probe has no bounded aggregate deadline")
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return isolation.Report{}
	}}
	done := make(chan error, 1)
	go func() { done <- scheduler.ensureNativeMachine(t.Context(), &NativeConnector{client: native}) }()
	<-entered
	acquired := make(chan struct{})
	go func() { scheduler.mu.Lock(); close(acquired); scheduler.mu.Unlock() }()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("probe blocked scheduler mutex")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("probe exceeded aggregate deadline")
	}
	if held.Load() {
		t.Fatal("canceled probe left its Sprite task held")
	}
}

func TestBlockedMachineReportDoesNotSerializeLeaseRenewal(t *testing.T) {
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

		case strings.HasSuffix(r.URL.Path, "/policy"):
			_ = json.NewEncoder(w).Encode(policy.Approval{Policy: approved})
		case strings.HasSuffix(r.URL.Path, "/policy/observed"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/renew"):
			var mutation tracker.NativeLeaseMutation
			if err := json.NewDecoder(r.Body).Decode(&mutation); err != nil {
				t.Error(err)
			}
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(tracker.NativeLease{ID: "lease", PolicyID: approved.ID, FencingToken: mutation.FencingToken, ServerTime: now, ExpiresAt: now.Add(time.Minute)})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	scheduler.nativeHeartbeats["prj_blocked"] = time.Now().Add(-time.Minute)
	scheduler.nativeHeartbeats["prj_free"] = time.Now().Add(time.Hour)
	free := scheduler.nativeProjects["free"]
	install := func(id, policyID string, token tracker.FencingToken) nativeClaim {
		claim := nativeClaim{source: free, lease: tracker.NativeLease{ID: "lease", PolicyID: policyID, FencingToken: token}}
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
				_, err := scheduler.renewNativeClaim(ctx, tc.name, stale)
				result <- err
			}()
			select {
			case err := <-result:
				if lost := errors.Is(err, orchestrator.ErrSchedulingClaimLost); lost != tc.wantLost || !lost && err != nil {
					t.Fatalf("renewNativeClaim() error = %v, want lost %t", err, tc.wantLost)
				}
			case <-time.After(time.Second):
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
	if time.Since(reported) > time.Minute {
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
	var capacities []int
	claims := 0
	transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		body := `{}`
		if strings.HasSuffix(request.URL.Path, "/heartbeat") {
			var report struct {
				Capacity int `json:"capacity"`
			}
			if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
				return nil, err
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
	machine := Machine{ID: file.Identity.MachineID, Capacity: 3, Version: "test"}
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
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); err != nil || claims != 1 {
		t.Fatalf("open claim = %v, calls = %d", err, claims)
	}
}
