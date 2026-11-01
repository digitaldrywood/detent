package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapabilitiesOrganizationHintScopesTenantAuthentication(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	first, err := service.createNativeOrganizationFor(t.Context(), "First organization")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.createNativeOrganizationFor(t.Context(), "Second organization")
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.now()
	provider := newHostedSecurityProvider()
	service.config.Hosted = &HostedConfig{OrganizationID: string(first.ID), Provider: provider}
	credential := "tenant_scoped_runner_token"
	hash := apikey.HashToken(credential)
	if err := repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: first.ID, Name: first.Name, Slug: first.Slug, Local: true, CreatedAt: now}); err != nil {
			return fmt.Errorf("create access organization: %w", err)
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: first.ID, ProviderID: "provider_first", AllocationGeneration: 1}); err != nil {
			return fmt.Errorf("create access tenant: %w", err)
		}
		personalHash := apikey.HashToken("personal_user_token")
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: first.ID, ID: "personal-first", Name: "Personal key", Hash: personalHash, Fingerprint: tokenFingerprint(personalHash), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_example", MembershipID: "membership_example", KeyScope: string(apikey.ScopeAdmin), ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return fmt.Errorf("create personal credential: %w", err)
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: first.ID, UserID: "user_example", Email: "user@example.test", MembershipID: "membership_example", Role: "admin", Active: true, PrincipalID: "personal-first", NotifyIssues: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			return fmt.Errorf("create hosted member: %w", err)
		}
		expires := now.Add(time.Hour)
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: first.ID, ID: "runner-first", Name: "Runner", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return fmt.Errorf("create access credential: %w", err)
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: first.ID, ID: "enrollment_example", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("enrollment_secret"), Scope: "projects", Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "tenant-admin"}); err != nil {
			return fmt.Errorf("create runner enrollment: %w", err)
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: first.ID, ID: "machine_example", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return fmt.Errorf("register machine: %w", err)
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: first.ID, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "runner-first", EnrollmentID: "enrollment_example", Scope: "projects", Operations: []string{"read", "claim"}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, OS: "darwin", Architecture: "arm64", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
			return fmt.Errorf("create runner identity: %w", err)
		}
		return nil
	}); err != nil {
		var postgresError *pq.Error
		if errors.As(err, &postgresError) {
			t.Fatalf("seed runner records failed on constraint %q: %s", postgresError.Constraint, postgresError.Message)
		}
		t.Fatal(err)
	}
	hostedMember := auth.Membership{ID: "membership_example", UserID: "user_example", OrganizationID: "provider_first", Status: "active"}
	hostedMember.Role.Slug = "admin"
	provider.members[hostedMember.ID] = hostedMember
	if err := repository.WriteTenant(t.Context(), second.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: second.ID, Name: second.Name, Slug: second.Slug, Local: true, CreatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	request := func(path, organization string) (apiCredential, int, error) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+credential)
		req.Header.Set("X-Detent-Organization", organization)
		c := echo.New().NewContext(req, httptest.NewRecorder())
		c.SetPath(path)
		if path == "/api/v2/organizations/:organization/projects/:project/native" {
			c.SetParamNames("organization", "project")
			c.SetParamValues(string(first.ID), "prj_example")
		}
		return service.authenticateAPIRequestContext(req.Context(), c)
	}
	got, status, err := request("/api/v2/capabilities", string(first.ID))
	if err != nil || status != http.StatusOK || got.Runner.RunnerID != "runner_example" || got.Runner.OrganizationID != first.ID {
		t.Fatalf("capabilities authentication=(%+v,%d,%v)", got, status, err)
	}
	rotated, err := service.changeRunnerCredentialTenant(t.Context(), got, "tenant_scoped_runner_replacement")
	if err != nil || rotated.RunnerID != got.Runner.RunnerID || !rotated.ExpiresAt.After(got.Runner.ExpiresAt) {
		t.Fatalf("credential rotation=(%+v,%v)", rotated, err)
	}
	if _, status, err := request("/api/v2/capabilities", string(first.ID)); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("old credential authenticated after rotation: status=%d err=%v", status, err)
	}
	replacementRequest := httptest.NewRequest(http.MethodGet, "/api/v2/capabilities", nil)
	replacementRequest.Header.Set(echo.HeaderAuthorization, "Bearer tenant_scoped_runner_replacement")
	replacementRequest.Header.Set("X-Detent-Organization", string(first.ID))
	replacementContext := echo.New().NewContext(replacementRequest, httptest.NewRecorder())
	replacementContext.SetPath("/api/v2/capabilities")
	replacementCredential, status, err := service.authenticateAPIRequestContext(replacementRequest.Context(), replacementContext)
	if err != nil || status != http.StatusOK || replacementCredential.Runner.RunnerID != got.Runner.RunnerID {
		t.Fatalf("replacement credential authentication=(%+v,%d,%v)", replacementCredential, status, err)
	}
	if _, status, err := request("/api/v2/capabilities", string(second.ID)); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("wrong organization authenticated: status=%d err=%v", status, err)
	}
	if _, status, err := request("/api/v2/organizations/:organization/projects/:project/native", string(second.ID)); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("conflicting organization hint authenticated: status=%d err=%v", status, err)
	}
	personal, status, err := service.authenticateTenantAPIHash(t.Context(), first.ID, apikey.HashToken("personal_user_token"), "", "")
	if err != nil || status != http.StatusOK || personal.Hosted == nil || personal.Hosted.Subject != "user_example" || personal.Hosted.OrganizationID != "provider_first" {
		t.Fatalf("personal credential authentication=(%+v,%d,%v)", personal, status, err)
	}
	if err := repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.RevokeAccessMember(t.Context(), "user_example", service.config.now()); err != nil {
			return err
		}
		scope := nativeScope{organization: first.ID, credential: personal}
		if err := service.authorizeAccessScope(t.Context(), w, scope, true); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("revoked membership authorized inside writer: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	lockResult := make(chan error, 1)
	go func() {
		lockResult <- repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RunnerIdentity(t.Context(), got.Runner.RunnerID); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	orderRepository := &runnerCredentialOrderRepository{Repository: repository, firstMethod: make(chan string, 1)}
	service.config.TenantRepository = orderRepository
	type rotationResult struct {
		replacement string
		runnerID    string
		err         error
	}
	results := make(chan rotationResult, 2)
	for _, next := range []string{"concurrent_runner_token_a", "concurrent_runner_token_b"} {
		go func(replacement string) {
			identity, err := service.changeRunnerCredentialTenant(t.Context(), replacementCredential, replacement)
			results <- rotationResult{replacement: replacement, runnerID: identity.RunnerID, err: err}
		}(next)
	}
	firstMethod := <-orderRepository.firstMethod
	close(release)
	lockErr := <-lockResult
	firstRotation, secondRotation := <-results, <-results
	service.config.TenantRepository = repository
	if lockErr != nil {
		t.Fatalf("concurrent authority rotation transaction: %v", lockErr)
	}
	if firstMethod != "identity" {
		t.Fatalf("credential rotation locked %q before runner identity", firstMethod)
	}
	succeeded := 0
	unauthorized := 0
	winningToken := ""
	for _, result := range []rotationResult{firstRotation, secondRotation} {
		if result.err == nil && result.runnerID == got.Runner.RunnerID {
			succeeded++
			winningToken = result.replacement
		} else {
			var nativeErr *nativeError
			if errors.As(result.err, &nativeErr) && nativeErr.Code == "unauthorized" {
				unauthorized++
			} else {
				t.Fatalf("concurrent credential rotation result=%+v", result)
			}
		}
	}
	if succeeded != 1 || unauthorized != 1 {
		t.Fatalf("concurrent credential rotations succeeded=%d unauthorized=%d", succeeded, unauthorized)
	}

	currentCredential, status, err := service.authenticateTenantAPIHash(t.Context(), first.ID, apikey.HashToken(winningToken), got.Runner.RunnerID, string(first.ID))
	if err != nil || status != http.StatusOK {
		t.Fatalf("expired identity renewal authentication=(%d,%v)", status, err)
	}
	originalNow := service.config.now
	clockNow := originalNow()
	service.config.now = func() time.Time { return clockNow }
	if err := repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.RunnerIdentity(t.Context(), got.Runner.RunnerID)
		if err != nil {
			return err
		}
		record.ExpiresAt = clockNow.Add(time.Second)
		return w.UpdateRunnerIdentity(t.Context(), record)
	}); err != nil {
		t.Fatal(err)
	}
	expiryLockHeld := make(chan struct{})
	expiryRelease := make(chan struct{})
	expiryLockResult := make(chan error, 1)
	go func() {
		expiryLockResult <- repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RunnerIdentity(t.Context(), got.Runner.RunnerID); err != nil {
				return err
			}
			close(expiryLockHeld)
			<-expiryRelease
			return nil
		})
	}()
	<-expiryLockHeld
	expiryOrder := &runnerCredentialOrderRepository{Repository: repository, firstMethod: make(chan string, 1)}
	service.config.TenantRepository = expiryOrder
	expiryRotation := make(chan error, 1)
	go func() {
		_, err := service.changeRunnerCredentialTenant(t.Context(), currentCredential, "rotation_after_waiting_past_expiry")
		expiryRotation <- err
	}()
	if method := <-expiryOrder.firstMethod; method != "identity" {
		close(expiryRelease)
		t.Fatalf("expiry-boundary rotation locked %q before runner identity", method)
	}
	clockNow = clockNow.Add(2 * time.Second)
	close(expiryRelease)
	if err := <-expiryLockResult; err != nil {
		t.Fatalf("expiry-boundary lock transaction: %v", err)
	}
	if err := <-expiryRotation; err == nil {
		t.Fatal("credential rotation accepted identity that expired while waiting for its lock")
	}
	service.config.TenantRepository = repository
	service.config.now = originalNow
	if err := repository.WriteTenant(t.Context(), first.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.RunnerIdentity(t.Context(), got.Runner.RunnerID)
		if err != nil {
			return err
		}
		record.ExpiresAt = service.config.now().Add(-time.Second)
		return w.UpdateRunnerIdentity(t.Context(), record)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.changeRunnerCredentialTenant(t.Context(), currentCredential, "rotation_after_identity_expiry"); err == nil {
		t.Fatal("credential rotation accepted an identity that expired after authentication")
	}
	if renewed, err := service.changeRunnerCredentialTenant(t.Context(), currentCredential, ""); err != nil || !renewed.ExpiresAt.After(service.config.now()) {
		t.Fatalf("expired identity renewal=(%+v,%v)", renewed, err)
	}
}

func TestTenantRunnerHeartbeatPersistsIdentityAndReturnsCurrentRouting(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository, Clock: clock.Now})
	service.config.AccessRepository = repository
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.now()
	secret := "example_runner_heartbeat_secret"
	hash := apikey.HashToken(secret)
	adminHash := apikey.HashToken(testHubAdminToken)
	expires := now.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, Local: true, CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization.ID, ProviderID: "provider_example", AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "admin", Name: "Example admin", Hash: adminHash, Fingerprint: tokenFingerprint(adminHash), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "credential_example", Name: "Example runner", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: "enrollment_example", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine_example", Hostname: "example-host", DisplayName: "Before heartbeat", Capacity: 1, Version: "before"}); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_example", EnrollmentID: "enrollment_example", Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, DisplayName: "Example runner", CapacityLimit: 4, ReportedCapacity: 1, OS: "darwin", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
	}); err != nil {
		var postgresError *pq.Error
		if errors.As(err, &postgresError) {
			t.Fatalf("seed runner records failed on constraint %q: %s", postgresError.Constraint, postgresError.Message)
		}
		t.Fatal(err)
	}
	createdProject := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "create", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, createdProject, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, createdProject, &project)
	path := "/api/v2/organizations/" + string(organization.ID) + "/projects/" + string(project.ID) + "/machines/machine_example/heartbeat"
	clock.value = now.Add(time.Minute)
	providerReport := providercapacity.Report{Provider: "openai", Backend: "native/codex", AccountAlias: "example", Billing: "subscription", Models: []string{"model-a"}, ModelDetails: []providercapacity.ModelDetail{{ID: "model-a", Label: "Model A", Provider: "openai", Default: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"}}, Availability: "available", ObservedAt: clock.value, UsageWindows: []providercapacity.UsageWindow{{Kind: "primary", ResetAt: clock.value.Add(time.Hour), ObservedAt: clock.value}}}
	capacityConfiguration := &runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 4, ClientLimit: 3, RuntimeLimit: 3, Manageable: true, ObservedAt: clock.value}
	response := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2, "provider_reports": []providercapacity.Report{providerReport}, "capacity_configuration": capacityConfiguration})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.RunnerID != "runner_example" || snapshot.Revision != 1 || snapshot.Routing.DisplayName != "Example runner" || snapshot.Routing.State != "active" || snapshot.Routing.Validate() != nil {
		t.Fatalf("heartbeat routing snapshot = %+v", snapshot)
	}
	var persisted runnerstore.Identity
	var machine leasestore.Machine
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		persisted, err = r.RunnerIdentity(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		machine, err = r.Machine(t.Context(), "machine_example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if persisted.ReportedCapacity != 3 || persisted.OS != "darwin" || persisted.Architecture != "arm64" || !persisted.LastHeartbeatAt.Equal(clock.value) || persisted.CapacityConfig == nil || persisted.CapacityConfig.LocalLimit != 4 || !persisted.CapacityConfig.ObservedAt.Equal(clock.value) {
		t.Fatalf("persisted heartbeat identity = %+v", persisted)
	}
	if machine.DisplayName != "Example host" || machine.Capacity != 3 || machine.Version != "0.174.1" {
		t.Fatalf("persisted heartbeat machine = %+v", machine)
	}
	var providers []providercapacity.Report
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		providers, err = r.RunnerProviderReports(t.Context(), "runner_example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Provider != providerReport.Provider || providers[0].Backend != providerReport.Backend || providers[0].Models[0] != providerReport.Models[0] || len(providers[0].ModelDetails) != 1 || providers[0].ModelDetails[0].DefaultReasoningEffort != "high" || len(providers[0].UsageWindows) != 1 || providers[0].Billing != providerReport.Billing {
		t.Fatalf("persisted provider reports = %+v", providers)
	}
	clock.value = now.Add(2 * time.Minute)
	projectConfiguration := runnerauth.ProjectConfiguration{
		ProjectID: string(project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: clock.value,
		Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: clock.value, RuntimeObservedAt: clock.value, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{}, Records: []runnerauth.DiagnosticAttempt{{Key: "local:1", IssueID: "wi_example", LocalAttemptID: 1, Generation: 2, Fence: 3, Stage: "implementing", Membership: []string{"running"}}}},
	}
	observed := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2, "project_configuration": projectConfiguration})
	requireNativeStatus(t, observed, http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		observation, attempts, err := r.RunnerProjectObservation(t.Context(), "runner_example", project.ID, "wi_example")
		if err != nil {
			return err
		}
		if !observation.DiagnosticsAt.Equal(clock.value) || !observation.RuntimeObservedAt.Equal(clock.value) || len(attempts) != 1 || attempts[0].LocalAttemptID != 1 || attempts[0].Generation != 2 || attempts[0].Fence != 3 || !attempts[0].Running {
			t.Fatalf("persisted project diagnostics = %+v / %+v", observation, attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := clock.value
	clock.value = now.Add(3 * time.Minute)
	unsupported := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "checkout_repository": "owner/repository"})
	requireNativeStatus(t, unsupported, http.StatusServiceUnavailable)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		persisted, err = r.RunnerIdentity(t.Context(), "runner_example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !persisted.LastHeartbeatAt.Equal(before) {
		t.Fatalf("unsupported heartbeat partially changed timestamp: before=%s after=%s", before, persisted.LastHeartbeatAt)
	}
}

type runnerCredentialOrderRepository struct {
	tenantstore.Repository
	firstMethod chan string
}

func (r *runnerCredentialOrderRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, _ corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		return write(&runnerCredentialOrderWriter{Writer: writer, repository: r})
	})
}

type runnerCredentialOrderWriter struct {
	tenantstore.Writer
	repository *runnerCredentialOrderRepository
	first      sync.Once
}

func (w *runnerCredentialOrderWriter) RunnerIdentity(ctx context.Context, id string) (runnerstore.Identity, error) {
	w.first.Do(func() { w.repository.firstMethod <- "identity" })
	return w.Writer.RunnerIdentity(ctx, id)
}

func (w *runnerCredentialOrderWriter) CredentialByHash(ctx context.Context, hash string) (accessstore.Credential, error) {
	w.first.Do(func() { w.repository.firstMethod <- "credential" })
	return w.Writer.CredentialByHash(ctx, hash)
}
