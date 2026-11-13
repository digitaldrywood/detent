package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRemoval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		attempt   bool
		finished  bool
		expired   bool
		revoked   bool
		sibling   bool
		mcp       bool
		zeroClock bool
		want      int
	}{
		{name: "idle", want: http.StatusNoContent},
		{name: "zero clock", zeroClock: true},
		{name: "already revoked", revoked: true, want: http.StatusNoContent},
		{name: "claimed work", want: http.StatusUnprocessableEntity},
		{name: "running attempt", attempt: true, want: http.StatusUnprocessableEntity},
		{name: "expired running attempt", attempt: true, expired: true, want: http.StatusNoContent},
		{name: "mcp expired running attempt", attempt: true, expired: true, mcp: true, want: http.StatusNoContent},
		{name: "mcp running attempt", attempt: true, mcp: true, want: http.StatusUnprocessableEntity},
		{name: "finished attempt", attempt: true, finished: true, want: http.StatusNoContent},
		{name: "busy sibling", attempt: true, sibling: true, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
			f := newPostgresDefaultNativeFixture(t, Config{Clock: clock.Now})
			f.service.echo.GET(hostedOrganizationBase+"/fleet", f.service.hostedFleet, f.service.localAppOnly)
			repository := f.service.config.TenantRepository
			organization := f.project.OrganizationID
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			worker := r
			if test.sibling {
				worker = sharedRunner(t, r)
			}
			if test.attempt || test.name == "claimed work" {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			}
			item := f.create(t, "Removal history")
			if test.attempt || test.name == "claimed work" {
				report := map[string]any{
					"display_name": worker.redemption.DisplayName, "capacity": 2, "version": "test",
					"backend_isolation": r.redemption.BackendIsolation,
					"capacity_configuration": runnerauth.CapacityConfig{
						Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: clock.Now(),
					},
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(worker.binding.MachineID)+"/heartbeat", worker.redemption.Credential, report), http.StatusOK)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker.redemption.Credential, tracker.NativeClaim{
					PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: worker.binding.MachineID, SessionID: "remove-test",
					TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if test.attempt {
					event := nativeStartedEvent(lease)
					path := f.base + "/work-items/" + string(item.WorkItemID) + "/events"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker.redemption.Credential, event), http.StatusOK)
					if test.finished {
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish", 2, "succeeded"
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker.redemption.Credential, event), http.StatusOK)
					}
				}
				if test.expired || test.finished {
					clock.Advance(lease.ExpiresAt.Sub(clock.Now()) + time.Second)
				}
			}
			if test.revoked {
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.RevokeAccessCredential(t.Context(), identity.TokenID, clock.Now())
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			removed := test.want == http.StatusNoContent
			if test.zeroClock {
				assertRunnerRemovalWithZeroClock(t, f, r.binding.RunnerID, runnerstore.ErrInvalidRecord, 2)
			} else if test.mcp {
				result, err := removeRunnerThroughMCP(t, f, r.binding.RunnerID)
				var refusal *operatortool.RequestError
				var action struct {
					Status chatpkg.ActionStatus `json:"status"`
				}
				switch {
				case removed && (err != nil || json.Unmarshal(result.Content, &action) != nil || action.Status != chatpkg.ActionSucceeded):
					t.Fatalf("MCP removal = %s, %v", result.Content, err)
				case !removed && (!errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "active work")):
					t.Fatalf("MCP refusal = %v", err)
				}
			} else {
				response := performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil)
				requireNativeStatus(t, response, test.want)
				if !removed && !strings.Contains(response.Body.String(), "active work") {
					t.Fatalf("missing active-work explanation: %s", response.Body.String())
				}
			}
			if test.name == "idle" {
				assertRunnerRemovalWithZeroClock(t, f, r.binding.RunnerID, nil, 1)
			}
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
				identity, err := reader.RunnerIdentity(t.Context(), r.binding.RunnerID)
				if err != nil {
					return err
				}
				credential, err := reader.AccessCredential(t.Context(), identity.TokenID)
				if err != nil {
					return err
				}
				if (identity.RemovedAt != nil) != removed || (credential.RevokedAt != nil) != removed || identity.DisplayName != "Runner" {
					return fmt.Errorf("identity after removal: removed=%v revoked=%v name=%q", identity.RemovedAt, credential.RevokedAt, identity.DisplayName)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			runners, err := f.service.listRunnerRoutingData(t.Context(), organization, 16, 0)
			if err != nil {
				t.Fatal(err)
			}
			if listed := slices.ContainsFunc(runners, func(runner runnerauth.Runner) bool { return runner.RunnerID == r.binding.RunnerID }); listed == removed {
				t.Fatalf("runner routing listed=%v after removal=%v", listed, removed)
			}
			fleetResponse := performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/fleet", testHubAdminToken, nil)
			requireNativeStatus(t, fleetResponse, http.StatusOK)
			var fleet hostedFleetResponse
			decodeHubResponse(t, fleetResponse, &fleet)
			if listed := slices.ContainsFunc(fleet.Runners, func(runner hostedFleetRunner) bool { return runner.ID == r.binding.RunnerID }); listed == removed {
				t.Fatalf("runner fleet listed=%v after removal=%v", listed, removed)
			}
			credential, authStatus, err := f.service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(f.token), "", "")
			if err != nil || authStatus != http.StatusOK {
				t.Fatalf("project credential status=%d: %v", authStatus, err)
			}
			scope := hubscope.NativeScope{Organization: organization, Project: f.project.ID, Credential: credential}
			var runtime tracker.NativeRuntimeEvidence
			if err := f.service.readRuntimeAdmissionTenant(t.Context(), scope, item, &runtime, nil); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(runtime.Unavailable, "approved_dispatch_policy") {
				t.Fatalf("runtime admission unavailable: %v", runtime.Unavailable)
			}
			if listed := slices.ContainsFunc(runtime.Admission, func(entry tracker.NativeRuntimeAdmission) bool { return entry.RunnerID == r.binding.RunnerID }); listed == removed {
				t.Fatalf("runner runtime admission listed=%v after removal=%v", listed, removed)
			}
			if test.sibling && !slices.ContainsFunc(runtime.Admission, func(entry tracker.NativeRuntimeAdmission) bool { return entry.RunnerID == worker.binding.RunnerID }) {
				t.Fatal("removing one runner hid its busy sibling")
			}
			status := http.StatusOK
			if removed {
				status = http.StatusUnauthorized
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil), http.StatusNotFound)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), status)
			if test.finished {
				names, err := f.service.readHostedRunnerNamesTenant(t.Context(), false)
				if err != nil || names[r.binding.RunnerID].DisplayName != "Runner" {
					t.Fatalf("historical name lookup = %#v, %v", names, err)
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/attempts", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var attempts tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &attempts)
				if len(attempts.Items) != 1 || attempts.Items[0].Status != "succeeded" {
					t.Fatalf("history after removal: %#v", attempts)
				}
				historicalRunner := attempts.Items[0].RunnerID
				if historicalRunner != r.binding.RunnerID {
					t.Fatalf("historical runner = %q, want %q", historicalRunner, r.binding.RunnerID)
				}
				err = repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					identity, err := reader.RunnerIdentity(t.Context(), historicalRunner)
					if err != nil {
						return err
					}
					if identity.DisplayName != "Runner" {
						return fmt.Errorf("historical runner name = %q", identity.DisplayName)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func assertRunnerRemovalWithZeroClock(t *testing.T, f nativeFixture, runnerID string, want error, wantClockReads int) {
	t.Helper()
	organization := f.project.OrganizationID
	admin, status, err := f.service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("admin credential status=%d: %v", status, err)
	}

	config := f.service.config
	authorizationTime := config.Clock()
	clockReads := 0
	config.Clock = func() time.Time {
		clockReads++
		if clockReads == 1 {
			return authorizationTime
		}
		return time.Time{}
	}
	service := &Service{database: f.service.database, config: config}
	scope := hubscope.NativeScope{Organization: organization, Credential: admin}
	_, err = service.revokeRunnerIdentityCommand(t.Context(), scope, runnerID)
	if !errors.Is(err, want) {
		t.Fatalf("runner removal error = %v, want %v", err, want)
	}
	if clockReads != wantClockReads {
		t.Fatalf("runner removal clock reads = %d, want %d", clockReads, wantClockReads)
	}
}

func removeRunnerThroughMCP(t *testing.T, f nativeFixture, runnerID string) (operatortool.Result, error) {
	t.Helper()
	contexts := make(chan context.Context, 1)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/runner-removal-context"
	f.service.echo.GET("/api/v2/organizations/:organization/runner-removal-context", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	ctx := operatortool.BindConnection(<-contexts, "runner-removal", "direct-test")
	executor := hubFleetExecutor{service: f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	return executor.Execute(ctx, operatortool.Call{Name: operatortool.RevokeRunnerIdentity, Arguments: json.RawMessage(`{"request_id":"remove","runner_id":"` + runnerID + `"}`)})
}

type runnerFixture struct {
	nativeFixture
	binding    runnerauth.Binding
	enrollment runnerauth.Enrollment
	redemption runnerauth.Redemption
	identity   runnerauth.Identity
	base       string
}

func prepareRunner(t *testing.T, f nativeFixture, operations ...string) runnerFixture {
	t.Helper()
	return prepareRunnerForScope(t, f, "projects", operations...)
}

func prepareRunnerForScope(t *testing.T, f nativeFixture, scope string, operations ...string) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	projects := []tracker.ProjectID{f.project.ID}
	if scope == "organization" {
		projects = nil
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, runnerauth.EnrollmentRequest{Scope: scope, Binding: binding, ProjectIDs: projects, Operations: operations, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	return runnerFixture{nativeFixture: f, binding: binding, enrollment: enrollment, base: base,
		redemption: runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
}

func (r *runnerFixture) enroll(t *testing.T) {
	t.Helper()
	response := performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption)
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &r.identity)
}

func (r runnerFixture) identityPath() string { return r.base + "/runners/" + r.binding.RunnerID }

func (r runnerFixture) reportCapacity(t *testing.T) {
	t.Helper()
	reportRunnerCapacity(t, r.service, r.nativeFixture.base, r.redemption.Credential, r.binding.MachineID, r.redemption.DisplayName, r.redemption.BackendIsolation, r.redemption.Capacity)
}

func TestRunnerConcurrentRotationHasOneWinner(t *testing.T) {
	t.Parallel()
	f := newPostgresDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read)
	r.enroll(t)
	start := make(chan struct{})
	statuses := make(chan int, 4)
	var workers sync.WaitGroup
	for range 4 {
		replacement, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			<-start
			response := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/rotate", r.redemption.Credential, runnerauth.Rotation{Credential: replacement})
			statuses <- response.Code
		})
	}
	close(start)
	workers.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusUnauthorized {
			t.Fatalf("rotation status = %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("rotation winners = %d, want 1", winners)
	}
}

func TestRunnerRenewRotateRevokeRestart(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Date(2026, 9, 29, 0, 31, 39, 0, time.UTC)}
	now := clock.Now()
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: clock.Now}), "", "lifecycle")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	if r.identity.Binding != r.binding || !r.identity.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) {
		t.Fatal("enrollment changed binding or expiry")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	r.service = f.service
	now = now.Add(12 * time.Hour)
	clock.Advance(now.Sub(clock.Now()))
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/renew", r.redemption.Credential, struct{}{})
	requireNativeStatus(t, response, http.StatusOK)
	var renewed runnerauth.Identity
	decodeHubResponse(t, response, &renewed)
	if !renewed.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) || renewed.Binding != r.binding {
		t.Fatal("renewal changed binding or expiry incorrectly")
	}
	// Production sequence: last renewal 12:31:39Z, stopped 22:17Z,
	// restarted the next day at 12:34Z, after the 12:31:39Z expiry.
	now = time.Date(2026, 9, 29, 22, 17, 0, 0, time.UTC)
	clock.Advance(now.Sub(clock.Now()))
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), http.StatusOK)
	now = time.Date(2026, 9, 30, 12, 34, 0, 0, time.UTC)
	clock.Advance(now.Sub(clock.Now()))
	heartbeatPath := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, heartbeatPath, r.redemption.Credential, heartbeat), http.StatusUnauthorized)
	response = performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/renew", r.redemption.Credential, struct{}{})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &renewed)
	if renewed.Binding != r.binding || renewed.OrganizationID != r.identity.OrganizationID || !renewed.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) {
		t.Fatal("restart renewal changed identity or expiry incorrectly")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, heartbeatPath, r.redemption.Credential, heartbeat), http.StatusOK)
	replacement, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/rotate", r.redemption.Credential, runnerauth.Rotation{Credential: replacement}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), http.StatusUnauthorized)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), replacement, nil), http.StatusOK)
	for _, test := range []struct {
		name, method, path string
		body               any
	}{
		{"generic rotation", http.MethodPost, "/api/v1/tokens/" + r.binding.RunnerID + "/rotate", struct{}{}},
		{"grant widening", http.MethodPost, "/api/v2/tokens/" + r.binding.RunnerID + "/grants", map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, test.method, test.path, testHubAdminToken, test.body)
			if response.Code != http.StatusNotFound && response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("bypass status = %d", response.Code)
			}
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	now = now.Add(30 * 24 * time.Hour)
	clock.Advance(now.Sub(clock.Now()))
	for _, path := range []string{r.identityPath() + "/renew", r.identityPath() + "/rotate", f.base + "/claims", f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, replacement, struct{}{}), http.StatusUnauthorized)
	}
	var events string
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT string_agg(kind,',' ORDER BY event_id) FROM runner_identity_events WHERE organization_id=$1 AND runner_id=$2", f.project.OrganizationID, r.binding.RunnerID).Scan(&events); err != nil || events != "enrolled,renewed,renewed,rotated,revoked" {
		t.Fatalf("lifecycle audit = %q, err=%v", events, err)
	}
}

func TestRunnerIdentityBindingAndOperations(t *testing.T) {
	t.Parallel()
	f := newPostgresDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	r.reportCapacity(t)
	other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	other.enroll(t)
	other.reportCapacity(t)
	for _, test := range []struct {
		name    string
		status  int
		unknown string
		admin   bool
	}{
		{name: "", status: http.StatusOK},
		{name: "another-host", status: http.StatusUnprocessableEntity},
		{name: r.redemption.Hostname, status: http.StatusOK},
		{name: r.redemption.Hostname, unknown: "top-level", status: http.StatusOK},
		{name: r.redemption.Hostname, unknown: "nested", status: http.StatusOK},
		{unknown: "top-level", admin: true, status: http.StatusUnprocessableEntity},
		{unknown: "nested", admin: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(fmt.Sprintf("Sprite heartbeat %s unknown=%s admin=%t", test.name, test.unknown, test.admin), func(t *testing.T) {
			body := map[string]any{"display_name": r.redemption.DisplayName, "capacity": r.redemption.Capacity, "version": "develop-040e7195c", "sprite_name": test.name, "backend_isolation": r.redemption.BackendIsolation}
			switch test.unknown {
			case "top-level":
				body["future_observation"] = map[string]any{"enabled": true}
			case "nested":
				body["project_configuration"] = map[string]any{
					"project_id": string(f.project.ID), "authority": "local_global_configuration",
					"source": "configured_committed_workflow", "observed_at": f.service.config.Clock(),
					"future_observation": true,
				}
			}
			token := r.redemption.Credential
			if test.admin {
				token = testHubAdminToken
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", token, body), test.status)
			if test.status == http.StatusOK {
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					machine, err := reader.Machine(t.Context(), string(r.binding.MachineID))
					if err == nil && (machine.SpriteName != test.name || machine.Version != "develop-040e7195c") {
						t.Fatalf("heartbeat Sprite identity = %q, version = %q", machine.SpriteName, machine.Version)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	issue := f.create(t, "work")
	descriptor := hubTestPolicy()
	descriptor.Requirements.RunnerID = r.binding.RunnerID
	descriptor.Requirements.MachineID = string(r.binding.MachineID)
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	otherClaim := claim
	otherClaim.MachineID = other.binding.MachineID
	otherClaim.SessionID = "other-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", other.redemption.Credential, otherClaim)
	requireNativeStatus(t, response, http.StatusConflict)
	if !strings.Contains(response.Body.String(), "selector_no_match") {
		t.Fatal("an enrolled runner widened the approved selector")
	}
	for _, test := range []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"impersonated claim", http.MethodPost, f.base + "/claims", claim, http.StatusNotFound},
		{"impersonated registration", http.MethodPost, f.base + "/machines/register", map[string]any{"id": r.binding.MachineID, "hostname": "customer-host", "display_name": "Same host", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"new machine registration", http.MethodPost, f.base + "/machines/register", map[string]any{"id": runnerauth.NewBinding().MachineID, "hostname": "customer-host", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"impersonated heartbeat", http.MethodPost, f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat", map[string]any{"display_name": "same", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"impersonated lease", http.MethodPost, f.base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}, http.StatusNotFound},
		{"impersonated release", http.MethodPost, f.base + "/leases/" + string(lease.ID) + "/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}, http.StatusNotFound},
		{"impersonated identity", http.MethodGet, r.identityPath(), nil, http.StatusNotFound},
		{"v1 downgrade", http.MethodGet, "/api/v1/work-items", nil, http.StatusForbidden},
		{"collaboration without grant", http.MethodPost, f.base + "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "denied"}, Title: "denied", State: "Todo"}, http.StatusForbidden},
		{"global outbox", http.MethodGet, "/api/v1/outbox/health", nil, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, test.method, test.path, other.redemption.Credential, test.body), test.want)
		})
	}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt"), PolicyID: descriptor.ID}}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", other.redemption.Credential, event), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", r.redemption.Credential, event), http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", r.redemption.Credential, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), r.binding.RunnerID) {
		t.Fatal("history did not attribute event to authenticated runner")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.base+"/machines/"+string(r.binding.MachineID)+"/routing", testHubAdminToken, runnerauth.HostChange{ExpectedRevision: 1, DisplayName: "Renamed", Capacity: 2}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Worker override", "capacity": 2, "version": "test"}), http.StatusOK)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		machine, err := reader.Machine(t.Context(), string(r.binding.MachineID))
		if err == nil && (machine.ID != string(r.binding.MachineID) || machine.DisplayName != "Renamed") {
			t.Fatal("rename did not preserve identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, otherProject.base, r.redemption.Credential, nil), http.StatusNotFound)
	readOnly := prepareRunner(t, f, runnerauth.Read)
	readOnly.enroll(t)
	for _, denied := range []string{f.base + "/claims", f.base + "/machines/register", path + "/events"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, denied, readOnly.redemption.Credential, struct{}{}), http.StatusForbidden)
	}
	for _, key := range []string{"provider_api_key", "storage_credentials", "prompt"} {
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		values := make(map[string]any)
		if err := json.Unmarshal(body, &values); err != nil {
			t.Fatal(err)
		}
		values[key] = "example-secret-do-not-store"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", r.redemption.Credential, values), http.StatusUnprocessableEntity)
	}
}
