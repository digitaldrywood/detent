package hubclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClientEnrollmentSchedulingAndRotationRecovery(t *testing.T) {
	t.Parallel()
	const adminToken = "runner-client-test-admin"
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(adminToken)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	var dropRotation atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rotate") && dropRotation.CompareAndSwap(true, false) {
			recorder := httptest.NewRecorder()
			service.Handler().ServeHTTP(recorder, r)
			if recorder.Code != http.StatusOK {
				t.Errorf("rotation failed before response loss: %d", recorder.Code)
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server cannot hijack response")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
			return
		}
		service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	admin, err := New(Config{URL: server.URL, TokenSource: func() string { return adminToken }, HTTPClient: server.Client()})
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
	file, err := runnerauth.Initialize(path, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Hostname: "customer", DisplayName: "Runner", Capacity: 1, Version: "test"}
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
	client, err := New(Config{URL: server.URL, IdentityFile: path, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	adminNative, err := admin.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fleetAdmin, err := NewFleetClient(admin, organization, map[string]tracker.ProjectID{"native": project.ID})
	if err != nil {
		t.Fatal(err)
	}
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Trusted builder", Tags: []string{"Build"}, State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{project.ID}}}
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
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "native", Policy: descriptor})
	if err != nil || len(candidates) != 1 || candidates[0].ID != string(issue.WorkItemID) {
		t.Fatalf("enrolled scheduler: candidates=%d err=%v", len(candidates), err)
	}
	if _, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatalf("runner-side validation: %v", err)
	}
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
	dropRotation.Store(true)
	if _, err := RefreshRunner(t.Context(), path, true); err == nil {
		t.Fatal("dropped rotation response reported success")
	}
	pending, err := runnerauth.Load(path)
	if err != nil || pending.PendingCredential == "" || pending.Credential != file.Credential {
		t.Fatalf("interrupted rotation not recoverable: %v", err)
	}
	restarted, err := New(Config{URL: server.URL, IdentityFile: path, HTTPClient: server.Client()})
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
	if err := admin.RevokeRunner(t.Context(), organization, identity.Binding); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RenewClaim(t.Context(), string(issue.WorkItemID), time.Now()); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		t.Fatalf("revocation did not stop the owned claim: %v", err)
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
