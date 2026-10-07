package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRemoval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		attempt  bool
		finished bool
		expired  bool
		revoked  bool
		sibling  bool
		want     int
	}{
		{name: "idle", want: http.StatusNoContent},
		{name: "already revoked", revoked: true, want: http.StatusNoContent},
		{name: "claimed work", want: http.StatusUnprocessableEntity},
		{name: "running attempt", attempt: true, want: http.StatusUnprocessableEntity},
		{name: "expired running attempt", attempt: true, expired: true, want: http.StatusUnprocessableEntity},
		{name: "finished attempt", attempt: true, finished: true, want: http.StatusNoContent},
		{name: "busy sibling", attempt: true, sibling: true, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			worker := r
			if test.sibling {
				worker = sharedRunner(t, r)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(worker.binding.MachineID)+"/heartbeat", worker.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation}), http.StatusOK)
			}
			var item tracker.NativeIssue
			if test.attempt || test.name == "claimed work" {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				item = f.create(t, "Removal history")
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
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(f.service.config.now().Add(-time.Second)), lease.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.revoked {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = ?", formatHubTime(f.service.config.now()), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil)
			requireNativeStatus(t, response, test.want)
			removed := test.want == http.StatusNoContent
			if !removed && !strings.Contains(response.Body.String(), "active work") {
				t.Fatalf("missing active-work explanation: %s", response.Body.String())
			}
			var removedAt, revokedAt sql.NullString
			var name string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT r.removed_at, t.revoked_at, r.display_name FROM runner_identities r JOIN api_tokens t ON t.id = r.token_id WHERE r.id = ?", r.binding.RunnerID).Scan(&removedAt, &revokedAt, &name); err != nil {
				t.Fatal(err)
			}
			if removedAt.Valid != removed || revokedAt.Valid != removed || name != "Runner" {
				t.Fatalf("identity after removal: removed=%v revoked=%v name=%q", removedAt, revokedAt, name)
			}
			runners, err := f.service.listRunnerRoutingData(t.Context(), f.project.OrganizationID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, runner := range runners {
				if removed && runner.RunnerID == r.binding.RunnerID {
					t.Fatal("removed runner still listed")
				}
			}
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
			fleet, err := f.service.hostedFleetRunners(t.Context(), apiCredential{}, map[tracker.ProjectID]bool{f.project.ID: true}, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, runner := range fleet {
				if removed && runner.ID == r.binding.RunnerID {
					t.Fatal("removed runner still in hosted fleet")
				}
			}
			f.service.config.Hosted = nil
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			runtime, _, err := readRuntimeRunners(t.Context(), f.service.database.db, scope, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			for _, runner := range runtime {
				if removed && runner.RunnerID == r.binding.RunnerID {
					t.Fatal("removed runner still eligible")
				}
			}
			status := http.StatusOK
			if removed {
				status = http.StatusUnauthorized
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil), http.StatusNotFound)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), status)
			if test.finished {
				names, err := readHostedRunnerNames(t.Context(), f.service.database.db, f.project.OrganizationID, false)
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
				var historicalName string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT r.display_name FROM native_attempts a JOIN lease_runners lr ON lr.lease_id = a.lease_id JOIN runner_identities r ON r.id = lr.runner_id WHERE a.work_item_id = ?", item.WorkItemID).Scan(&historicalName); err != nil || historicalName != "Runner" {
					t.Fatalf("historical runner name = %q, %v", historicalName, err)
				}
			}
		})
	}
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
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: operations, TTLSeconds: 60})
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

func TestRunnerEnrollmentSingleRedemption(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	start := make(chan struct{})
	statuses := make(chan int, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption)
			statuses <- response.Code
		})
	}
	close(start)
	workers.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			winners++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("redemption status = %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("redemption winners = %d", winners)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), http.StatusUnauthorized)
	for _, token := range []string{r.enrollment.Token, r.redemption.Credential} {
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE token_hash = ?", token).Scan(&count); err != nil || count != 0 {
			t.Fatalf("plaintext credential persisted: count=%d err=%v", count, err)
		}
	}
}

func TestRunnerUnboundEnrollment(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	create := func(t *testing.T, request runnerauth.EnrollmentRequest) runnerauth.Enrollment {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, request)
		requireNativeStatus(t, response, http.StatusCreated)
		var enrollment runnerauth.Enrollment
		decodeHubResponse(t, response, &enrollment)
		return enrollment
	}
	redemption := func(t *testing.T, binding runnerauth.Binding) runnerauth.Redemption {
		t.Helper()
		credential, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		return runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}
	}
	unbound := runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60}

	for _, test := range []struct {
		sprite string
		status int
	}{
		{"customer-host", http.StatusCreated},
		{"other-host", http.StatusUnprocessableEntity},
		{"Customer-host", http.StatusUnprocessableEntity},
	} {
		t.Run("Sprite identity "+test.sprite, func(t *testing.T) {
			enrollment := create(t, unbound)
			body := redemption(t, runnerauth.NewBinding())
			body.SpriteName = test.sprite
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", enrollment.Token, body), test.status)
			if test.status == http.StatusCreated {
				var name string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT json_extract(capabilities_json, '$.sprite_name') FROM machines WHERE id=?", body.MachineID).Scan(&name); err != nil || name != test.sprite {
					t.Fatalf("persisted Sprite = %q, %v", name, err)
				}
			}
		})
	}

	for _, test := range []struct {
		name    string
		request runnerauth.EnrollmentRequest
		status  int
	}{
		{"unbound", unbound, http.StatusCreated},
		{"runner without machine", func() runnerauth.EnrollmentRequest {
			request := unbound
			request.RunnerID = runnerauth.NewBinding().RunnerID
			return request
		}(), http.StatusUnprocessableEntity},
		{"shared without a host", func() runnerauth.EnrollmentRequest {
			request := unbound
			request.SharedMachine = true
			return request
		}(), http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, test.request), test.status)
		})
	}

	enrollment := create(t, unbound)
	first := runnerauth.NewBinding()
	statuses := make(chan int, 4)
	var workers sync.WaitGroup
	for i := range 4 {
		binding := first
		if i > 0 {
			binding = runnerauth.NewBinding()
		}
		body := redemption(t, binding)
		workers.Go(func() {
			statuses <- performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", enrollment.Token, body).Code
		})
	}
	workers.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusCreated {
			winners++
		} else if status != http.StatusUnauthorized {
			t.Fatalf("redemption status = %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("unbound redemption winners = %d, want 1", winners)
	}
	var runner, machine string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT runner_id, machine_id FROM runner_enrollments WHERE id = ?", enrollment.ID).Scan(&runner, &machine); err != nil || runner == "" || machine == "" {
		t.Fatalf("redeemed enrollment binding = %q %q, %v", runner, machine, err)
	}
	var identities int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_identities WHERE id = ? AND machine_id = ?", runner, machine).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("runner identity rows = %d, %v", identities, err)
	}

	taken := create(t, unbound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", taken.Token, redemption(t, runnerauth.Binding{RunnerID: runner, MachineID: tracker.MachineID(machine)})), http.StatusConflict)
}

func TestRunnerConcurrentRotationHasOneWinner(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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

func TestRunnerEnrollmentValidationAndClock(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*runnerFixture, *time.Time)
		want   int
	}{
		{"valid", func(*runnerFixture, *time.Time) {}, http.StatusCreated},
		{"just before expiry", func(r *runnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt.Add(-time.Nanosecond) }, http.StatusCreated},
		{"expiry boundary", func(r *runnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt }, http.StatusUnauthorized},
		{"expired", func(r *runnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt.Add(time.Second) }, http.StatusUnauthorized},
		{"clock before issuance", func(_ *runnerFixture, now *time.Time) { *now = now.Add(-time.Nanosecond) }, http.StatusUnauthorized},
		{"invalid clock", func(_ *runnerFixture, now *time.Time) { *now = time.Time{} }, http.StatusInternalServerError},
		{"wrong machine", func(r *runnerFixture, _ *time.Time) { r.redemption.MachineID = runnerauth.NewBinding().MachineID }, http.StatusUnauthorized},
		{"wrong runner", func(r *runnerFixture, _ *time.Time) { r.redemption.RunnerID = runnerauth.NewBinding().RunnerID }, http.StatusUnauthorized},
		{"wrong organization", func(r *runnerFixture, _ *time.Time) { r.base = "/api/v2/organizations/" + newNativeID("org") }, http.StatusUnauthorized},
		{"weak credential", func(r *runnerFixture, _ *time.Time) { r.redemption.Credential = "example-provider-secret" }, http.StatusUnprocessableEntity},
		{"enrollment reused as identity", func(r *runnerFixture, _ *time.Time) { r.redemption.Credential = r.enrollment.Token }, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 123, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "clock")
			r := prepareRunner(t, f, runnerauth.Read)
			test.change(&r, &now)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), test.want)
		})
	}
}

func TestRunnerRenewRotateRevokeRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 0, 31, 39, 0, time.UTC)
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	f := newNativeFixture(t, openTestService(t, config), "", "lifecycle")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	if r.identity.Binding != r.binding || !r.identity.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) {
		t.Fatal("enrollment changed binding or expiry")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, config)
	r.service = f.service
	now = now.Add(12 * time.Hour)
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
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), http.StatusOK)
	now = time.Date(2026, 9, 30, 12, 34, 0, 0, time.UTC)
	heartbeatPath := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	heartbeat := map[string]any{"capacity": 2, "version": "test"}
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
	f.service = openTestService(t, config)
	now = now.Add(30 * 24 * time.Hour)
	for _, path := range []string{r.identityPath() + "/renew", r.identityPath() + "/rotate", f.base + "/claims", f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, replacement, struct{}{}), http.StatusUnauthorized)
	}
	var events string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT group_concat(kind, ',') FROM (SELECT kind FROM runner_identity_events WHERE runner_id = ? ORDER BY id)", r.binding.RunnerID).Scan(&events); err != nil || events != "enrolled,renewed,renewed,rotated,revoked" {
		t.Fatalf("lifecycle audit = %q, err=%v", events, err)
	}
}

func TestRunnerIdentityBindingAndOperations(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	other.enroll(t)
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
					"source": "configured_committed_workflow", "observed_at": f.service.config.now(),
					"future_observation": true,
				}
			}
			token := r.redemption.Credential
			if test.admin {
				token = testHubAdminToken
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", token, body), test.status)
			if test.status == http.StatusOK {
				var name, version string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT COALESCE(json_extract(capabilities_json, '$.sprite_name'), ''), version FROM machines WHERE id=?", r.binding.MachineID).Scan(&name, &version); err != nil || name != test.name || version != "develop-040e7195c" {
					t.Fatalf("heartbeat Sprite identity = %q, version = %q, %v", name, version, err)
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
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: descriptor.ID}}
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
	var display, machine string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id, display_name FROM machines WHERE id = ?", r.binding.MachineID).Scan(&machine, &display); err != nil || machine != string(r.binding.MachineID) || display != "Renamed" {
		t.Fatal("rename did not preserve identity")
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
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", r.redemption.Credential, values), http.StatusOK)
		response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", r.redemption.Credential, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if strings.Contains(response.Body.String(), key) || strings.Contains(response.Body.String(), "example-secret-do-not-store") {
			t.Fatalf("unknown event field was stored: %s", response.Body.String())
		}
	}
}
