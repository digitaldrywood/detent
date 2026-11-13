package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOnboardingPreservesImportedProgressAndRetries(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted.WorkOSOrganizationID = "provider_" + cfg.Hosted.OrganizationID
	cfg.Postgres.Pool = postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1}
	cfg.deps.RunMaintenance = func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return nil
	}
	const ownerToken = "example_onboarding_owner"
	now := time.Now().UTC()
	if _, err := cfg.Hosted.Provider.CreateMembership(t.Context(), "user_owner", "provider_org_startup", "owner"); err != nil {
		t.Fatal(err)
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup", cfg.Hosted)
	if err := store.postgres.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_owner", Name: "Example owner", Hash: apikey.HashToken(ownerToken), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_owner", MembershipID: "membership_user_owner", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: "user_owner", Email: "owner@example.test", MembershipID: "membership_user_owner", Role: "owner", Active: true, PrincipalID: "credential_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for _, id := range []tracker.ProjectID{"project_first", "project_second"} {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}

			runner := "runner_" + string(id)
			machine := "machine_" + string(id)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: runner, Name: runner, Hash: apikey.HashToken(runner), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: runner, ProjectID: id}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: machine, Hostname: "example-host", Capacity: 2}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: "enrollment_" + runner, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enroll_" + runner), Scope: "projects", ProjectIDs: []tracker.ProjectID{id}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_startup", RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: runner, EnrollmentID: "enrollment_" + runner, Scope: "projects", State: "active", DisplayName: runner, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{id}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			checks := runnerauth.LocalChecks{Checkout: "passed", Configuration: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: now, CheckoutAt: now, ConfigurationAt: now, DoctorAt: now, ProviderAt: now}
			if id == "project_second" {
				checks.Configuration = "missing"
				checks.Doctor = "pending"
				checks.ProviderKinds = []string{"claude_code"}
			}
			if err := w.ReplaceRunnerLocalChecks(t.Context(), runner, id, checks); err != nil {
				return err
			}
			if err := w.RequestRunnerRepositories(t.Context(), runner, "request_"+runner); err != nil {
				return err
			}
			if err := w.ReplaceRunnerRepositories(t.Context(), runnerstore.RepositoryList{OrganizationID: "org_startup", RunnerID: runner, RequestID: "request_" + runner, ListedAt: &now}, []runnerstore.Repository{{OrganizationID: "org_startup", RunnerID: runner, Owner: "example-org", Name: string(id), ListedAt: now}}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_owner", ProjectID: id, CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_owner", ProjectID: id}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	records := []postgres.OnboardingProgressImport{
		{ProjectID: "project_first", Progress: onboarding.Progress{Revision: 3, Repository: "existing", Doctor: true, Artifacts: "local", UpdatedAt: "2026-11-01T12:00:00.123456789Z"}},
		{ProjectID: "project_second", Progress: onboarding.Progress{Revision: 7, Repository: "generate", Provider: true, Artifacts: "customer", UpdatedAt: "2026-11-01T12:00:00Z"}},
	}
	objects := &memoryRunDetails{}
	original := map[tracker.ProjectID]string{}
	historical := newCoreNeutralFixture(t, "org_startup", "project_onboarding")
	for _, record := range records {
		raw, err := json.Marshal(map[string]any{"repository": record.Progress.Repository, "doctor": record.Progress.Doctor, "provider": record.Progress.Provider, "artifacts": record.Progress.Artifacts, "policy_approved": true, "revision": int64(record.Progress.Revision), "updated_at": record.Progress.UpdatedAt})
		if err != nil {
			t.Fatal(err)
		}
		original[record.ProjectID] = string(raw)
		historical.add("project_onboarding", map[string]any{"organization_id": "org_startup", "project_id": record.ProjectID, "revision": int64(record.Progress.Revision), "progress_json": string(raw), "updated_at": record.Progress.UpdatedAt})
	}
	source := postgres.CoreProjectImportSource{BundlePath: historical.write(), OrganizationID: "org_startup", Objects: objects}
	for range 2 {
		if count, err := store.postgres.ImportCoreOnboardingProgress(t.Context(), source); err != nil || count != 2 {
			t.Fatalf("historical onboarding rows=%d error=%v", count, err)
		}
	}

	changed := append([]postgres.OnboardingProgressImport{}, records...)
	changed[0].Progress.Provider = true
	if err := store.postgres.ImportOnboardingProgress(t.Context(), "org_startup", changed); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("changed imported source overwrote progress: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.postgres == nil || service.config.TenantRepository == nil {
		t.Fatal("required PostgreSQL fixture owners are not configured")
	}
	read := func(project tracker.ProjectID) onboarding.Progress {
		t.Helper()
		path := "/api/v2/organizations/org_startup/projects/" + string(project) + "/onboarding"
		response := performHubAPIRequest(t, service, http.MethodGet, path, ownerToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var result onboarding.Project
		decodeHubResponse(t, response, &result)
		if len(result.Runners) != 1 || result.Runners[0].Runner.RunnerID != "runner_"+string(project) || result.Runners[0].LocalChecks == nil || result.Runners[0].RepositoryList == nil || len(result.Runners[0].RepositoryList.Repositories) != 1 || result.Runners[0].RepositoryList.Repositories[0].Name != string(project) {
			t.Fatalf("project runner proof/repository scope=%+v", result.Runners)
		}
		checks := result.Runners[0].LocalChecks
		if project == "project_first" && (!checks.Passed() || !slices.Equal(checks.ProviderKinds, []string{"codex"})) || project == "project_second" && (checks.Configuration != "missing" || checks.Doctor != "pending" || !slices.Equal(checks.ProviderKinds, []string{"claude_code"})) {
			t.Fatalf("project local checks lost fields: %+v", checks)
		}
		return result.Progress
	}
	for _, record := range records {
		if actual := read(record.ProjectID); actual != record.Progress {
			t.Fatalf("imported project %s progress=%+v want=%+v", record.ProjectID, actual, record.Progress)
		}
	}
	if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(reader tenantstore.Reader) error {
		for _, record := range records {
			receipt, err := reader.Receipt(t.Context(), "production-import", "core-onboarding", string(record.ProjectID))
			if err != nil {
				return err
			}
			raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
			if err != nil {
				return err
			}
			var preserved struct {
				ProgressJSON string
				Revision     int64
				UpdatedAt    string
			}
			if err := json.Unmarshal(raw, &preserved); err != nil {
				return err
			}
			if preserved.ProgressJSON != original[record.ProjectID] || preserved.Revision != int64(record.Progress.Revision) || preserved.UpdatedAt != record.Progress.UpdatedAt {
				return fmt.Errorf("historical onboarding source changed: %+v", preserved)
			}
			var legacy struct {
				PolicyApproved bool `json:"policy_approved"`
			}
			if err := json.Unmarshal([]byte(preserved.ProgressJSON), &legacy); err != nil || !legacy.PolicyApproved {
				return fmt.Errorf("historical approval source lost: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	path := "/api/v2/organizations/org_startup/projects/project_first/onboarding"
	request := saveOnboardingRequest{Mutation: tracker.Mutation{IdempotencyKey: "retry-checkout"}, Progress: records[0].Progress}
	request.Progress.Doctor, request.Progress.Provider = false, true
	repository := service.config.TenantRepository
	gate := &onboardingReplayRepository{Repository: repository, ready: make(chan struct{}, 2), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	var requests sync.WaitGroup
	t.Cleanup(func() { release(); requests.Wait() })
	service.config.TenantRepository = gate
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		req := newHubAPIRequest(t, http.MethodPut, path, ownerToken, request)
		requests.Add(1)
		go func() {
			defer requests.Done()
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, req)
			responses <- response
		}()
	}
	for range 2 {
		select {
		case <-gate.ready:
		case response := <-responses:
			t.Fatalf("concurrent progress request failed before CAS: %d %s", response.Code, response.Body.String())
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	release()
	for range 2 {
		response := <-responses
		requireNativeStatus(t, response, http.StatusOK)
		var saved onboarding.Progress
		decodeHubResponse(t, response, &saved)
		if saved.Revision != 4 || saved.Doctor || !saved.Provider || saved.Repository != request.Progress.Repository || saved.Artifacts != request.Progress.Artifacts || saved.UpdatedAt == "" {
			t.Fatalf("saved retry progress=%+v", saved)
		}
	}
	service.config.TenantRepository = repository
	if actual := read("project_second"); actual != records[1].Progress {
		t.Fatalf("retry changed another project's progress: %+v", actual)
	}
	request.IdempotencyKey = "stale-retry"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, path, ownerToken, request), http.StatusConflict)
	request.Progress.Revision, request.Progress.Repository = 4, "unsupported"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, path, ownerToken, request), http.StatusUnprocessableEntity)
	foreign := "/api/v2/organizations/org_other/projects/project_first/onboarding"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, foreign, ownerToken, nil), http.StatusNotFound)
}

type onboardingReplayRepository struct {
	tenantstore.Repository
	ready   chan struct{}
	release chan struct{}
}
type onboardingReplayWriter struct {
	tenantstore.Writer
	projectstore.OnboardingPageWriter
	repository *onboardingReplayRepository
}

func (r *onboardingReplayRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(onboardingReplayWriter{Writer: w, OnboardingPageWriter: w.(projectstore.OnboardingPageWriter), repository: r})
	})
}
func (w onboardingReplayWriter) InvalidateOnboardingPage(ctx context.Context, project tracker.ProjectID, viewer string) error {
	w.repository.ready <- struct{}{}
	select {
	case <-w.repository.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return w.OnboardingPageWriter.InvalidateOnboardingPage(ctx, project, viewer)
}
