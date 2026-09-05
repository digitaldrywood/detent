package hubserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
		redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
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
	f := newNativeFixture(t, nil, "", "enrollment")
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

func TestRunnerConcurrentRotationHasOneWinner(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "rotations")
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
	now := time.Date(2026, 9, 5, 12, 0, 0, 123, time.UTC)
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
	for _, path := range []string{r.identityPath() + "/renew", r.identityPath() + "/rotate", f.base + "/claims", f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, replacement, struct{}{}), http.StatusUnauthorized)
	}
	var events string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT group_concat(kind, ',') FROM (SELECT kind FROM runner_identity_events WHERE runner_id = ? ORDER BY id)", r.binding.RunnerID).Scan(&events); err != nil || events != "enrolled,renewed,rotated,revoked" {
		t.Fatalf("lifecycle audit = %q, err=%v", events, err)
	}
}

func TestRunnerIdentityBindingAndOperations(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "binding")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	other.enroll(t)
	issue := f.create(t, "work")
	claim := tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
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
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: newNativeID("policy")}}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", other.redemption.Credential, event), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", r.redemption.Credential, event), http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", r.redemption.Credential, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), r.binding.RunnerID) {
		t.Fatal("history did not attribute event to authenticated runner")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Renamed", "capacity": 2, "version": "test"}), http.StatusNoContent)
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
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", r.redemption.Credential, values), http.StatusUnprocessableEntity)
	}
}
