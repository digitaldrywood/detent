package hubserver

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCredentialExpiryBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		offset time.Duration
		want   int
	}{
		{"before expiry", -time.Nanosecond, http.StatusOK}, {"at expiry", 0, http.StatusUnauthorized}, {"after expiry", time.Second, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "expiry")
			r := prepareRunner(t, f, runnerauth.Read)
			r.enroll(t)
			now = r.identity.ExpiresAt.Add(test.offset)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath(), r.redemption.Credential, nil), test.want)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/renew", r.redemption.Credential, struct{}{}), test.want)
		})
	}
}

func TestRunnerEnrollmentScopeRevocationAndCollisions(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "scopes")
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
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, live.redemption.Credential, nil), http.StatusNotFound)
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
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
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

func TestRunnerMigrationPreservesLegacyTokens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "00009_" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		migrations[file.Name()] = &fstest.MapFile{Data: data}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	const legacy = "legacy-worker-retained-across-upgrade"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens (id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES ('legacy','legacy',?,'fingerprint','worker','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, apikey.HashToken(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	config := Config{DatabasePath: path, now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	for range 2 {
		service := openTestService(t, config)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/health", legacy, nil), http.StatusOK)
		var expiry sql.NullString
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT expires_at FROM api_tokens WHERE id = 'legacy'").Scan(&expiry); err != nil || expiry.Valid {
			t.Fatalf("legacy expiry changed: %v", err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
