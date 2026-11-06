package hubserver

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCredentialExpiryBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		offset    time.Duration
		statement string
		want      int
		wantRenew int
	}{
		{"before expiry", -time.Nanosecond, "", http.StatusOK, http.StatusOK},
		{"at expiry", 0, "", http.StatusUnauthorized, http.StatusOK},
		{"after expiry", time.Second, "", http.StatusUnauthorized, http.StatusOK},
		{"weekend", 3 * 24 * time.Hour, "", http.StatusUnauthorized, http.StatusOK},
		{"vacation", 30 * 24 * time.Hour, "", http.StatusUnauthorized, http.StatusOK},
		{"before creation", -25 * time.Hour, "", http.StatusUnauthorized, http.StatusUnauthorized},
		{"invalid creation", 0, "UPDATE api_tokens SET created_at = 'invalid' WHERE id = ?", http.StatusUnauthorized, http.StatusUnauthorized},
		{"invalid expiry", 0, "UPDATE api_tokens SET expires_at = 'invalid' WHERE id = ?", http.StatusUnauthorized, http.StatusUnauthorized},
		{"empty interval", 0, "UPDATE api_tokens SET expires_at = created_at WHERE id = ?", http.StatusUnauthorized, http.StatusUnauthorized},
		{"missing expiry", 0, "UPDATE api_tokens SET expires_at = NULL WHERE id = ?", http.StatusOK, http.StatusUnauthorized},
		{"revoked and expired", 0, "UPDATE api_tokens SET revoked_at = created_at WHERE id = ?", http.StatusUnauthorized, http.StatusUnauthorized},
		{"rotated and expired", 0, "UPDATE api_tokens SET token_hash = '1111111111111111111111111111111111111111111111111111111111111111' WHERE id = ?", http.StatusUnauthorized, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "expiry")
			r := prepareRunner(t, f, runnerauth.Read)
			r.enroll(t)
			now = r.identity.ExpiresAt.Add(test.offset)
			if test.statement != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), test.statement, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), test.want)
			if test.statement == "" && test.offset >= 0 {
				for _, path := range []string{
					r.identityPath() + "/rotate",
					r.base + "/runners/" + runnerauth.NewBinding().RunnerID + "/renew",
					"/api/v2/organizations/other/runners/" + r.binding.RunnerID + "/renew",
				} {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, struct{}{}), http.StatusUnauthorized)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/renew", r.redemption.Credential, struct{}{}), test.wantRenew)
			if test.wantRenew == http.StatusOK {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), http.StatusOK)
			}
		})
	}
}

func TestRunnerEnrollmentScopeRevocationAndCollisions(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read)
	for _, test := range []struct {
		name   string
		change func(*runnerauth.EnrollmentRequest)
	}{
		{"no projects", func(r *runnerauth.EnrollmentRequest) { r.ProjectIDs = nil }},
		{"unknown project", func(r *runnerauth.EnrollmentRequest) {
			r.ProjectIDs = []tracker.ProjectID{tracker.ProjectID(newNativeID("prj"))}
		}},
		{"duplicate projects", func(r *runnerauth.EnrollmentRequest) { r.ProjectIDs = append(r.ProjectIDs, r.ProjectIDs[0]) }},
		{"no operations", func(r *runnerauth.EnrollmentRequest) { r.Operations = nil }},
		{"administrator operation", func(r *runnerauth.EnrollmentRequest) { r.Operations = []string{"admin"} }},
		{"excessive TTL", func(r *runnerauth.EnrollmentRequest) { r.TTLSeconds = 901 }},
		{"zero TTL", func(r *runnerauth.EnrollmentRequest) { r.TTLSeconds = 0 }},
		{"mutable name ID", func(r *runnerauth.EnrollmentRequest) { r.MachineID = "hostname" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := runnerauth.EnrollmentRequest{Binding: runnerauth.NewBinding(), ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
			test.change(&request)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken, request), http.StatusUnprocessableEntity)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.base+"/runner-enrollments/"+r.enrollment.ID, testHubAdminToken, nil), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), http.StatusUnauthorized)
	r = prepareRunner(t, f, runnerauth.Read)
	request := runnerauth.EnrollmentRequest{Binding: r.binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusCreated)
	var competing runnerauth.Enrollment
	decodeHubResponse(t, response, &competing)
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", competing.Token, r.redemption), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken, request), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken, request), http.StatusConflict)
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]string{"name": "other"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	other := newNativeFixture(t, f.service, organization.ID, "foreign")
	request.Binding = runnerauth.NewBinding()
	request.ProjectIDs = []tracker.ProjectID{other.project.ID}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken, request), http.StatusUnprocessableEntity)
	live := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	live.enroll(t)
	for _, path := range []string{other.base, "/api/v2/organizations/" + string(organization.ID) + "/runners/" + live.binding.RunnerID} {
		for _, test := range []struct {
			name, token string
			status      int
		}{
			{"valid credential outside scope", live.redemption.Credential, http.StatusNotFound},
			{"revoked credential before resource lookup", r.redemption.Credential, http.StatusUnauthorized},
			{"invalid credential before resource lookup", "invalid", http.StatusUnauthorized},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, test.token, nil), test.status)
			})
		}
	}
}

func TestRunnerCredentialsAndRejectedProviderDataAreRedacted(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	path := filepath.Join(t.TempDir(), "hub.db")
	f := newNativeFixture(t, openTestService(t, Config{DatabasePath: path, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}), "", "redaction")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	const provider = "example-provider-key-not-for-hub"
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "safe", "capacity": 1, "version": "test", "provider_key": provider})
	requireNativeStatus(t, response, http.StatusOK)
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{provider, r.enrollment.Token, r.redemption.Credential} {
		if bytes.Contains(contents, []byte(value)) || bytes.Contains(logs.Bytes(), []byte(value)) || bytes.Contains(response.Body.Bytes(), []byte(value)) {
			t.Fatal("private value escaped into database, logs or errors")
		}
	}
}
