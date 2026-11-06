package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRunnerCapabilitiesOrganizationHintScopesTenantAuthentication(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, now: time.Now}}
	first := nativeOrganization{ID: "org_first", Name: "First organization", Slug: "first"}
	second := nativeOrganization{ID: "org_second", Name: "Second organization", Slug: "second"}
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

func TestTenantRunnerHeartbeatHandlesMultipleLiveAssignedLeases(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)}
	service := &Service{database: &database{postgres: repository, now: clock.Now}, notifications: newNotificationBroker(), echo: echo.New(), secretRewrapReady: make(chan struct{}), config: (Config{RunDetails: &memoryRunDetails{}, TenantRepository: repository, AccessRepository: repository, Clock: clock.Now, Logger: discardLogger(), RunnerReleaseClient: &runnerReleaseFixture{}}).normalized()}
	service.registerPostgresRoutes(service.echo)
	organization := nativeOrganization{ID: "org_example", Name: "Example organization", Slug: "example"}
	service.database.hostedOrganization = organization.ID
	service.database.billingRepository = repository
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	usagePlan := billingstore.PlanReference{ID: "runner_protocol", Version: 1}
	now := service.config.now()
	secret := "example_runner_heartbeat_secret"
	hash := apikey.HashToken(secret)
	secondSecret := "example_runner_heartbeat_secret_two"
	secondHash := apikey.HashToken(secondSecret)
	adminHash := apikey.HashToken(testHubAdminToken)
	expires := now.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, Local: true, CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization.ID, ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization.ID, PlanReference: usagePlan, Name: "Runner protocol", Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 100}}); err != nil {
			return err
		}
		if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization.ID, Base: usagePlan}, 0); err != nil {
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
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "credential_example_two", Name: "Example runner two", Hash: secondHash, Fingerprint: tokenFingerprint(secondHash), Scope: "worker", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: "enrollment_example", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine_example", Hostname: "example-host", DisplayName: "Before heartbeat", Capacity: 1, Version: "before"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_example", EnrollmentID: "enrollment_example", Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, DisplayName: "Example runner", CapacityLimit: 4, ReportedCapacity: 1, OS: "darwin", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: "enrollment_example_two", RunnerID: "runner_example_two", MachineID: "machine_example_two", TokenHash: apikey.HashToken("example_enrollment_secret_two"), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine_example_two", Hostname: "example-host-two", DisplayName: "Before heartbeat", Capacity: 1, Version: "before"}); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: "runner_example_two", MachineID: "machine_example_two", TokenID: "credential_example_two", EnrollmentID: "enrollment_example_two", Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, DisplayName: "Example runner two", CapacityLimit: 4, ReportedCapacity: 1, OS: "darwin", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
	}); err != nil {
		var postgresError *pq.Error
		if errors.As(err, &postgresError) {
			t.Fatalf("seed runner records failed on constraint %q: %s", postgresError.Constraint, postgresError.Message)
		}
		t.Fatal(err)
	}
	service.config.SecretKeys = secretTestKeys(t, "1", "1")
	cursorKey := []byte(strings.Repeat("k", 32))
	cursorEnvelope, err := service.config.SecretKeys.Seal(cursorKey, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	createdProject := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "create", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, createdProject, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, createdProject, &project)
	usageWindow := time.Unix(now.Unix()/60*60, 0).UTC()
	enrollmentPath := "/api/v2/organizations/" + string(organization.ID) + "/runner-enrollments"
	var enrollmentIDs []string
	var enrollmentTokens []string
	for _, identity := range []struct {
		runnerID  string
		machineID string
	}{
		{runnerID: "runner_00000000000000000000000000000001", machineID: "machine_00000000000000000000000000000001"},
		{runnerID: "runner_00000000000000000000000000000002", machineID: "machine_00000000000000000000000000000002"},
	} {
		response := performHubAPIRequest(t, service, http.MethodPost, enrollmentPath, testHubAdminToken, map[string]any{
			"runner_id": identity.runnerID, "machine_id": identity.machineID,
			"scope": "projects", "project_ids": []string{string(project.ID)}, "operations": []string{runnerauth.Read}, "ttl_seconds": 300,
		})
		requireNativeStatus(t, response, http.StatusCreated)
		var enrollment runnerauth.Enrollment
		decodeHubResponse(t, response, &enrollment)
		if enrollment.ID == "" || enrollment.Token == "" || !enrollment.ExpiresAt.After(now) {
			t.Fatalf("created enrollment = %+v", enrollment)
		}
		enrollmentIDs = append(enrollmentIDs, enrollment.ID)
		enrollmentTokens = append(enrollmentTokens, enrollment.Token)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		for i, id := range enrollmentIDs {
			row, err := r.RunnerEnrollment(t.Context(), id)
			if err != nil {
				return err
			}
			if row.RunnerID != []string{"runner_00000000000000000000000000000001", "runner_00000000000000000000000000000002"}[i] || row.TokenHash == "" || row.Scope != "projects" || len(row.ProjectIDs) != 1 || row.ProjectIDs[0] != project.ID || row.RevokedAt != nil {
				t.Fatalf("stored enrollment %s = %+v", id, row)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	revoke := performHubAPIRequest(t, service, http.MethodDelete, enrollmentPath+"/"+enrollmentIDs[0], testHubAdminToken, nil)
	requireNativeStatus(t, revoke, http.StatusNoContent)
	redemptionIsolation := isolation.Report{"example-project/codex": {isolation.Sandbox, isolation.NativeTrusted}}
	spriteNow := service.config.now()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: organization.ID, Name: "example-sprite", ProviderOrganization: "provider_example", EnrollmentID: enrollmentIDs[1], State: "enrolled", IdleSince: spriteNow, CreatedAt: spriteNow})
	}); err != nil {
		t.Fatal(err)
	}
	spriteCredential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{Binding: runnerauth.Binding{RunnerID: "runner_00000000000000000000000000000002", MachineID: "machine_00000000000000000000000000000002"}, Credential: spriteCredential, Hostname: "example-sprite", SpriteName: "example-sprite", DisplayName: "Example sprite runner", Capacity: 1, Version: "0.174.1", OS: "linux", Architecture: "amd64", BackendIsolation: redemptionIsolation}
	redeemed := performHubAPIRequest(t, service, http.MethodPost, enrollmentPath+"/redeem", enrollmentTokens[1], redemption)
	requireNativeStatus(t, redeemed, http.StatusCreated)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		identity, err := r.RunnerIdentity(t.Context(), redemption.RunnerID)
		if err != nil {
			return err
		}
		machine, err := r.Machine(t.Context(), string(redemption.MachineID))
		if err != nil {
			return err
		}
		isolationReport, err := r.RunnerBackendIsolation(t.Context(), identity.RunnerID)
		if err != nil {
			return err
		}
		if len(identity.Tags) != 1 || identity.Tags[0] != "sprite" || machine.SpriteName != "example-sprite" || len(isolationReport) != 1 || !slices.Contains(isolationReport["example-project/codex"], isolation.NativeTrusted) {
			t.Fatalf("redeemed identity/machine/isolation = %+v / %+v / %+v", identity, machine, isolationReport)
		}
		enrollment, err := r.RunnerEnrollment(t.Context(), enrollmentIDs[1])
		if err != nil {
			return err
		}
		if enrollment.RedeemedAt == nil || enrollment.RunnerID != redemption.RunnerID || enrollment.MachineID != redemption.MachineID {
			t.Fatalf("redeemed enrollment = %+v", enrollment)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.RunnerEnrollment(t.Context(), enrollmentIDs[0])
		if err != nil {
			return err
		}
		if row.RevokedAt == nil {
			t.Fatalf("revoked enrollment remains active: %+v", row)
		}
		row, err = r.RunnerEnrollment(t.Context(), enrollmentIDs[1])
		if err != nil {
			return err
		}
		if row.RevokedAt != nil {
			t.Fatalf("revoking one enrollment changed another: %+v", row)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/" + string(organization.ID) + "/projects/" + string(project.ID) + "/machines/machine_example/heartbeat"
	clock.value = now.Add(time.Minute)
	providerReport := providercapacity.Report{Provider: "openai", Backend: "native/codex", AccountAlias: "example", Billing: "subscription", Models: []string{"model-a"}, ModelDetails: []providercapacity.ModelDetail{{ID: "model-a", Label: "Model A", Provider: "openai", Default: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"}}, Availability: "available", ObservedAt: clock.value, UsageWindows: []providercapacity.UsageWindow{{Kind: "primary", ResetAt: clock.value.Add(time.Hour), ObservedAt: clock.value}}}
	capacityConfiguration := &runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 4, ClientLimit: 3, RuntimeLimit: 3, Manageable: true, ObservedAt: clock.value}
	backendIsolation := isolation.Report{"example-project/codex": {isolation.Sandbox, isolation.NativeTrusted}}
	localChecks := runnerauth.LocalChecks{CommitName: "Example Runner", CommitEmail: "runner@example.invalid", CommitSigning: "ssh", Setup: "passed", Checkout: "passed", Configuration: "passed", Doctor: "warning", Provider: "passed", ProviderKinds: []string{"codex"}, CheckoutAt: clock.value, ConfigurationAt: clock.value, DoctorAt: clock.value, ProviderAt: clock.value}
	localChecksObservedAt := clock.value
	workspaceCapabilities := workspacesession.Capabilities{Terminal: true, Files: true, Diff: true, Exec: true, Git: true}
	reportedSkills := []skills.ProviderSkill{{Name: "review", Description: "Review this project", Path: "/project/.agents/skills/review/SKILL.md", Scope: "project", Enabled: true, UserInvocable: true, Invocation: "$review"}}
	hostSummary := testHostSummary(clock.value)
	secondHostSummary := hostSummary
	secondHostSummary.SegmentID = clock.value.Add(-30 * time.Second)
	admission := tracker.NativeAdmissionObservation{RunnerRevision: 1, Context: tracker.NativeAdmissionContext{ObservedAt: clock.value, PolicyID: "policy_example", WorkflowStates: []string{"Todo"}}}
	problem := runnerauth.NewProblem("backend_missing")
	problem.ProjectID = string(project.ID)
	problem.Subject = "example-project/codex"
	problem.Check = "backend probe"
	response := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2, "provider_reports": []providercapacity.Report{providerReport}, "capacity_configuration": capacityConfiguration, "backend_isolation": backendIsolation, "sprite_name": "example-host", "host_metrics": []hostmetrics.Summary{hostSummary, secondHostSummary}, "admission": admission, "problems": []runnerauth.Problem{problem}, "local_checks": localChecks, "workspace_capabilities": workspaceCapabilities, "workspace_isolation": "sandbox", "skills": reportedSkills, "checkout_repository": "example-org/example-repo"})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.RunnerID != "runner_example" || snapshot.Revision != 1 || snapshot.Routing.DisplayName != "Example runner" || snapshot.Routing.State != "active" || snapshot.Routing.Validate() != nil || len(snapshot.HostMetricsAcknowledged) != 2 || !snapshot.HostMetricsAcknowledged[0].Hour.Equal(hostSummary.Hour) || !snapshot.HostMetricsAcknowledged[0].SegmentID.Equal(hostSummary.SegmentID) || !snapshot.HostMetricsAcknowledged[1].SegmentID.Equal(secondHostSummary.SegmentID) {
		t.Fatalf("heartbeat routing snapshot = %+v", snapshot)
	}
	var persisted runnerstore.Identity
	var machine leasestore.Machine
	var isolationReport isolation.Report
	var hostSegments []hostmetrics.Summary
	var storedAdmission tracker.NativeAdmissionObservation
	var storedProblems []runnerauth.Problem
	var storedLocalChecks runnerauth.LocalChecks
	var storedWorkspaceCapabilities workspacesession.Capabilities
	var storedWorkspaceIsolation string
	var storedSkills []runnerstore.ProjectSkill
	var checkoutVerified bool
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		persisted, err = r.RunnerIdentity(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		machine, err = r.Machine(t.Context(), "machine_example")
		if err != nil {
			return err
		}
		isolationReport, err = r.RunnerBackendIsolation(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		hostSegments, err = r.RunnerHostMetrics(t.Context(), "runner_example", clock.value.Add(-time.Hour), clock.value.Add(time.Hour))
		if err != nil {
			return err
		}
		storedAdmission, err = r.RunnerAdmissionObservation(t.Context(), "runner_example", project.ID)
		if err != nil {
			return err
		}
		storedProblems, err = r.RunnerProblems(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		storedLocalChecks, err = r.RunnerLocalChecks(t.Context(), "runner_example", project.ID)
		if err != nil {
			return err
		}
		if _, err := r.RunnerLocalChecks(t.Context(), "runner_example_two", project.ID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("second runner local checks = %v, want not found", err)
		}
		storedWorkspaceCapabilities, storedWorkspaceIsolation, err = r.RunnerWorkspaceReport(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		storedSkills, err = r.RunnerProjectSkills(t.Context(), project.ID, "runner_example", clock.value, runnerstore.MaxProjectSkills)
		if err != nil {
			return err
		}
		checkoutVerified, err = r.RunnerCheckoutVerified(t.Context(), project.ID, "example-org/example-repo", clock.value)
		if err != nil {
			return err
		}
		if _, _, err := r.RunnerWorkspaceReport(t.Context(), "runner_example_two"); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("second runner workspace report = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if persisted.ReportedCapacity != 3 || persisted.OS != "darwin" || persisted.Architecture != "arm64" || !persisted.LastHeartbeatAt.Equal(clock.value) || persisted.CapacityConfig == nil || persisted.CapacityConfig.LocalLimit != 4 || !persisted.CapacityConfig.ObservedAt.Equal(clock.value) {
		t.Fatalf("persisted heartbeat identity = %+v", persisted)
	}
	if machine.DisplayName != "Example host" || machine.Capacity != 3 || machine.Version != "0.174.1" || machine.SpriteName != "example-host" {
		t.Fatalf("persisted heartbeat machine = %+v", machine)
	}
	if len(isolationReport) != 1 || !slices.Contains(isolationReport["example-project/codex"], isolation.NativeTrusted) {
		t.Fatalf("persisted backend isolation = %+v", isolationReport)
	}
	if len(hostSegments) != 2 || !hostSegments[0].Hour.Equal(hostSummary.Hour) || !hostSegments[0].SegmentID.Equal(hostSummary.SegmentID) || !hostSegments[1].SegmentID.Equal(secondHostSummary.SegmentID) {
		t.Fatalf("persisted runner host segments = %+v", hostSegments)
	}
	if len(storedProblems) != 1 || storedProblems[0].Code != problem.Code || storedProblems[0].ProjectID != problem.ProjectID || storedProblems[0].FirstSeen.IsZero() || storedProblems[0].ReportedAt.IsZero() {
		t.Fatalf("persisted runner problems = %+v", storedProblems)
	}
	if storedLocalChecks.CommitName != localChecks.CommitName || storedLocalChecks.CommitEmail != localChecks.CommitEmail || storedLocalChecks.CommitSigning != localChecks.CommitSigning || storedLocalChecks.Checkout != localChecks.Checkout || storedLocalChecks.Configuration != localChecks.Configuration || storedLocalChecks.Doctor != localChecks.Doctor || storedLocalChecks.Provider != localChecks.Provider || !slices.Equal(storedLocalChecks.ProviderKinds, localChecks.ProviderKinds) || !storedLocalChecks.ObservedAt.Equal(localChecksObservedAt) {
		t.Fatalf("persisted runner local checks = %+v", storedLocalChecks)
	}
	if storedWorkspaceCapabilities != workspaceCapabilities || storedWorkspaceIsolation != "sandbox" {
		t.Fatalf("persisted runner workspace report = %+v / %q", storedWorkspaceCapabilities, storedWorkspaceIsolation)
	}
	if len(storedSkills) != 1 || storedSkills[0].RunnerID != "runner_example" || storedSkills[0].Skill != reportedSkills[0] || !storedSkills[0].ReportedAt.Equal(clock.value) || !checkoutVerified {
		t.Fatalf("persisted runner skills = %+v, checkout verified=%v", storedSkills, checkoutVerified)
	}
	hostHistory, err := service.readRunnerHostHistory(t.Context(), organization.ID, "runner_example", hostSummary.Hour, hostSummary.Hour.Add(time.Hour))
	if err != nil || len(hostHistory) != 1 || hostHistory[0].SampleCount != hostSummary.SampleCount+secondHostSummary.SampleCount || hostHistory[0].CPUBusyAveragePercent == nil || *hostHistory[0].CPUBusyAveragePercent != hostSummary.CPUBusySumPercent/float64(hostSummary.CPUSampleCount) {
		t.Fatalf("typed runner host history = %+v, error=%v", hostHistory, err)
	}
	recommendationSegment := hostSummary
	recommendationSegment.Hour = hostSummary.Hour.Add(-time.Hour)
	recommendationSegment.SegmentID = recommendationSegment.Hour.Add(time.Minute)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendRunnerHostMetricSegments(t.Context(), "runner_example", []hostmetrics.Summary{recommendationSegment}, clock.value)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recommendationScope := nativeScope{organization: organization.ID, credential: apiCredential{ID: "admin", Hash: adminHash, Scope: apiScopeAdmin}}
	recommendationRunner, err := service.readRunnerRouting(t.Context(), recommendationScope, "runner_example")
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := service.readRunnerCapacity(t.Context(), recommendationScope, "runner_example", "")
	if err != nil {
		t.Fatalf("read typed host metrics for recommendation: %v", err)
	}
	recommendation := capacity.(runnerCapacityRead).Recommendation
	recommendationHours, err := recommendationHoursFromSegments([]hostmetrics.Summary{recommendationSegment})
	if err != nil {
		t.Fatal(err)
	}
	expectedRecommendation, err := recommendRunner(recommendationRunner.Routing, recommendationHours, nil)
	if err != nil || len(recommendation.Periods) != len(expectedRecommendation.Periods) || len(recommendation.Periods) == 0 || recommendation.Periods[0].HoursCovered != expectedRecommendation.Periods[0].HoursCovered || recommendation.Periods[0].MemoryReserveBytes != expectedRecommendation.Periods[0].MemoryReserveBytes || recommendation.Periods[0].DiskReserveBytes != expectedRecommendation.Periods[0].DiskReserveBytes {
		t.Fatalf("typed runner recommendation = %+v, want host projection %+v, error=%v", recommendation, expectedRecommendation, err)
	}
	persistedSummary := hostSegments[0]
	persistedSummary.Hour, persistedSummary.SegmentID = time.Time{}, time.Time{}
	expectedSummary := hostSummary
	expectedSummary.Hour, expectedSummary.SegmentID = time.Time{}, time.Time{}
	if persistedSummary != expectedSummary {
		t.Fatalf("persisted runner host metric values = %+v, want %+v", persistedSummary, expectedSummary)
	}
	if storedAdmission.RunnerRevision != admission.RunnerRevision || !storedAdmission.ReceivedAt.Equal(clock.value) || storedAdmission.Context.PolicyID != admission.Context.PolicyID || !slices.Equal(storedAdmission.Context.WorkflowStates, admission.Context.WorkflowStates) {
		t.Fatalf("persisted runner admission = %+v", storedAdmission)
	}
	clock.value = clock.value.Add(time.Second)
	unsupportedProtocol := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 3})
	requireNativeStatus(t, unsupportedProtocol, http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		problems, err := r.RunnerProblems(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		if len(problems) != 1 || problems[0].Code != "version_unsupported" {
			t.Fatalf("unsupported protocol problem = %+v", problems)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		reports, err := r.RunnerProjectSkills(t.Context(), project.ID, "runner_example", clock.value, runnerstore.MaxProjectSkills)
		if err != nil {
			return err
		}
		verified, err := r.RunnerCheckoutVerified(t.Context(), project.ID, "example-org/example-repo", clock.value)
		if err != nil {
			return err
		}
		providers, err := r.RunnerProviderReports(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		if len(providers) != 1 {
			t.Errorf("omitted provider reports cleared stored values: %d", len(providers))
		}
		if len(reports) != 1 || !verified {
			t.Fatalf("omitted heartbeat reports changed stored values: skills=%+v checkout=%v", reports, verified)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(time.Second)
	cleared := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "skills": []skills.ProviderSkill{}, "checkout_repository": ""})
	requireNativeStatus(t, cleared, http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		reports, err := r.RunnerProjectSkills(t.Context(), project.ID, "runner_example", clock.value, runnerstore.MaxProjectSkills)
		if err != nil {
			return err
		}
		verified, err := r.RunnerCheckoutVerified(t.Context(), project.ID, "example-org/example-repo", clock.value)
		if err != nil {
			return err
		}
		if len(reports) != 0 || verified {
			t.Fatalf("empty heartbeat reports did not clear values: skills=%+v checkout=%v", reports, verified)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	leaseProjectResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects", testHubAdminToken, map[string]any{
		"idempotency_key": "heartbeat-leases-project",
		"name":            "Heartbeat leases",
		"states":          nativeFixtureStates(),
	})
	requireNativeStatus(t, leaseProjectResponse, http.StatusOK)
	var leaseProject tracker.NativeProject
	decodeHubResponse(t, leaseProjectResponse, &leaseProject)
	var heartbeatWorkItems []corestore.WorkItem
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		for index := range 2 {
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
				OrganizationID: organization.ID,
				NativeID:       tracker.NativeWorkItemID(fmt.Sprintf("wi_heartbeat_%d", index+1)),
				ProjectID:      leaseProject.ID,
				Number:         int64(9001 + index),
				Title:          "Heartbeat lease example",
			})
			if err != nil {
				return err
			}
			heartbeatWorkItems = append(heartbeatWorkItems, item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, item := range heartbeatWorkItems {
		var lease leasestore.Lease
		if err := repository.WriteLeases(t.Context(), organization.ID, corestore.Serving, func(w leasestore.Writer) error {
			var err error
			lease, err = w.Claim(t.Context(), leasestore.Claim{
				IssueID:          item.ID,
				ExpectedRevision: item.Revision,
				MachineID:        "machine_example",
				SessionID:        fmt.Sprintf("heartbeat_session_%d", index+1),
				LeaseID:          fmt.Sprintf("lease_heartbeat_%d", index+1),
				TTL:              time.Hour,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization.ID, LeaseID: lease.LeaseID, RunnerID: "runner_example", AssignedAt: lease.AcquiredAt})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, runner := range []struct {
		runnerID  string
		machineID string
		secret    string
	}{
		{runnerID: "runner_example", machineID: "machine_example", secret: secret},
		{runnerID: "runner_example_two", machineID: "machine_example_two", secret: secondSecret},
	} {
		adminRequest := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": runner.runnerID, "idempotency_key": "repositories-" + runner.runnerID})
		requireNativeStatus(t, adminRequest, http.StatusOK)
		var requestResult struct {
			RequestID string `json:"request_id"`
		}
		decodeHubResponse(t, adminRequest, &requestResult)
		adminReplay := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": runner.runnerID, "idempotency_key": "repositories-" + runner.runnerID})
		requireNativeStatus(t, adminReplay, http.StatusOK)
		var replayResult struct {
			RequestID string `json:"request_id"`
		}
		decodeHubResponse(t, adminReplay, &replayResult)
		if replayResult.RequestID != requestResult.RequestID {
			t.Fatalf("runner %s repository request replay changed ID: %q != %q", runner.runnerID, replayResult.RequestID, requestResult.RequestID)
		}
		heartbeatPayload := map[string]any{"display_name": "Example host", "capacity": 2, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2}
		var updateReport *runnerauth.UpdateObservation
		updateClock := clock.value
		if runner.runnerID == "runner_example" {
			clock.value = updateClock.Add(123456100 * time.Nanosecond)
			heartbeatPayload["provider_reports"] = []providercapacity.Report{providerReport}
			service.config.Version = "v0.174.2"
			service.runnerPublishedReleases.Store("v0.174.2/darwin/arm64", true)
			build := runnerauth.BuildEvidence{Version: "0.174.1", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "darwin", Architecture: "arm64", ObservedAt: clock.value}
			updateReport = &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, Pending: true, AvailableObservedAt: clock.value, AvailableVersion: "0.174.2", Running: build, ObservedAt: clock.value}
			updateReport.Revision = updateReport.BuildRevision()
			heartbeatPayload["update"] = updateReport
		}
		heartbeat := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/machines/"+runner.machineID+"/heartbeat", runner.secret, heartbeatPayload)
		requireNativeStatus(t, heartbeat, http.StatusOK)
		var routing runnerauth.RoutingSnapshot
		decodeHubResponse(t, heartbeat, &routing)
		if routing.RepositoryListRequest != requestResult.RequestID {
			t.Fatalf("runner %s heartbeat repository request=%q want=%q", runner.runnerID, routing.RepositoryListRequest, requestResult.RequestID)
		}
		if runner.runnerID == "runner_example" {
			if routing.TargetRunnerVersion != "0.174.2" || routing.Routing.UpdateRequest == nil || !routing.Routing.UpdateRequest.FollowHub {
				t.Fatalf("published Hub update was not delivered: %+v", routing)
			}
			runnerUpdatePath := "/api/v2/organizations/" + string(organization.ID) + "/runners/runner_example/update"
			readUpdate := func() runnerauth.UpdateView {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodGet, runnerUpdatePath, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view runnerauth.UpdateView
				decodeHubResponse(t, response, &view)
				return view
			}
			view := readUpdate()
			if view.Status != "requested" || view.Desired == nil || !view.Desired.FollowHub || view.Observation == nil || view.Observation.Revision != updateReport.Revision {
				t.Fatalf("persisted follow-Hub update=%+v", view)
			}
			if !routing.Routing.UpdateRequest.RequestedAt.Equal(view.Desired.RequestedAt) {
				t.Fatalf("follow-Hub response request time=%s, persisted=%s", routing.Routing.UpdateRequest.RequestedAt, view.Desired.RequestedAt)
			}
			target := runnerauth.BuildEvidence{Version: "0.174.2", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "darwin", Architecture: "arm64", VerifiedRelease: true, ObservedAt: clock.value}
			updateReport.Running = target
			updateReport.AvailableVersion = "0.174.3"
			updateReport.Revision = updateReport.BuildRevision()
			updateReport.Receipt = &runnerauth.UpdateReceipt{Request: *view.Desired, Status: "running", Applied: &target, ObservedAt: clock.value}
			service.config.Version = "v0.174.1"
			settledPayload := map[string]any{"display_name": "Example host", "capacity": 2, "version": target.Version, "os": "darwin", "architecture": "arm64", "protocol_major": 2, "provider_reports": []providercapacity.Report{providerReport}, "update": updateReport}
			settled := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/machines/"+runner.machineID+"/heartbeat", runner.secret, settledPayload)
			requireNativeStatus(t, settled, http.StatusOK)
			var settledRouting runnerauth.RoutingSnapshot
			decodeHubResponse(t, settled, &settledRouting)
			view = readUpdate()
			if view.Status != "running" || view.Desired == nil || view.Desired.ID != routing.Routing.UpdateRequest.ID {
				t.Fatalf("settled follow-Hub update status=%s desired=%+v observation=%+v receipt=%+v", view.Status, view.Desired, view.Observation, view.Observation.Receipt)
			}
			manual := runnerUpdateChange{ExpectedRevision: view.Revision, ExpectedBuildRevision: updateReport.Revision, Service: "detent", Version: "0.174.3", Release: true, FromRelease: true, Confirm: true, IdempotencyKey: "pg-runner-update"}
			applyPath := runnerUpdatePath + "/apply"
			applied := performHubAPIRequest(t, service, http.MethodPost, applyPath, testHubAdminToken, manual)
			requireNativeStatus(t, applied, http.StatusAccepted)
			var appliedView runnerauth.UpdateView
			decodeHubResponse(t, applied, &appliedView)
			if appliedView.Status != "requested" || appliedView.Desired == nil || appliedView.Desired.Version != manual.Version || appliedView.Observation == nil {
				t.Fatalf("selected runner update response=%+v", appliedView)
			}
			if persisted := readUpdate(); persisted.Desired == nil || !appliedView.Desired.RequestedAt.Equal(persisted.Desired.RequestedAt) {
				t.Fatalf("selected runner update response request time=%s, persisted=%+v", appliedView.Desired.RequestedAt, persisted.Desired)
			}
			replay := performHubAPIRequest(t, service, http.MethodPost, applyPath, testHubAdminToken, manual)
			requireNativeStatus(t, replay, http.StatusAccepted)
			if !bytes.Equal(applied.Body.Bytes(), replay.Body.Bytes()) {
				t.Fatal("selected runner update replay changed")
			}
			urgentPath := "/api/v2/organizations/" + string(organization.ID) + "/runner-update/urgent"
			urgentRequest := urgentRunnerUpdateChange{ExpectedRevision: 0, Version: "0.174.4", Confirm: true, IdempotencyKey: "pg-urgent-runner-update"}
			urgentResponse := performHubAPIRequest(t, service, http.MethodPut, urgentPath, testHubAdminToken, urgentRequest)
			requireNativeStatus(t, urgentResponse, http.StatusAccepted)
			var urgent urgentRunnerUpdate
			decodeHubResponse(t, urgentResponse, &urgent)
			if urgent.Revision != 1 || urgent.Request == nil || !urgent.Request.Urgent || urgent.Request.Version != urgentRequest.Version {
				t.Fatalf("typed urgent update=%+v", urgent)
			}
			urgentRead := performHubAPIRequest(t, service, http.MethodGet, urgentPath, testHubAdminToken, nil)
			requireNativeStatus(t, urgentRead, http.StatusOK)
			var storedUrgent urgentRunnerUpdate
			decodeHubResponse(t, urgentRead, &storedUrgent)
			if storedUrgent.Revision != urgent.Revision || storedUrgent.Request == nil || !storedUrgent.Request.Matches(*urgent.Request) {
				t.Fatalf("typed urgent update read=%+v want=%+v", storedUrgent, urgent)
			}
			if !urgent.Request.RequestedAt.Equal(storedUrgent.Request.RequestedAt) {
				t.Fatalf("urgent response request time=%s, persisted=%s", urgent.Request.RequestedAt, storedUrgent.Request.RequestedAt)
			}
			urgentReplay := performHubAPIRequest(t, service, http.MethodPut, urgentPath, testHubAdminToken, urgentRequest)
			requireNativeStatus(t, urgentReplay, http.StatusAccepted)
			if !bytes.Equal(urgentResponse.Body.Bytes(), urgentReplay.Body.Bytes()) {
				t.Fatal("urgent runner update replay changed")
			}
			startUrgentWrites := make(chan struct{})
			urgentResponses := make(chan *httptest.ResponseRecorder, 2)
			for index := 0; index < 2; index++ {
				request := newHubAPIRequest(t, http.MethodPut, urgentPath, testHubAdminToken, urgentRunnerUpdateChange{ExpectedRevision: urgent.Revision, Version: urgentRequest.Version, Confirm: true, IdempotencyKey: fmt.Sprintf("pg-urgent-race-%d", index)})
				go func() {
					<-startUrgentWrites
					response := httptest.NewRecorder()
					service.Handler().ServeHTTP(response, request)
					urgentResponses <- response
				}()
			}
			close(startUrgentWrites)
			accepted, conflicted := 0, 0
			for range 2 {
				response := <-urgentResponses
				switch response.Code {
				case http.StatusAccepted:
					accepted++
				case http.StatusConflict:
					conflict := requireNativeError(t, response, http.StatusConflict, "revision_conflict")
					if conflict.CurrentRevision != 2 {
						t.Fatalf("concurrent urgent update conflict revision=%d want=2", conflict.CurrentRevision)
					}
					conflicted++
				default:
					t.Fatalf("concurrent urgent update status=%d body=%s", response.Code, response.Body.String())
				}
			}
			if accepted != 1 || conflicted != 1 {
				t.Fatalf("concurrent urgent update outcomes accepted=%d conflicted=%d want=1/1", accepted, conflicted)
			}
			service.config.Version = ""
		}
		clock.value = updateClock
		report := map[string]any{"idempotency_key": requestResult.RequestID, "request_id": requestResult.RequestID, "repositories": []runnerauth.Repository{{Owner: "example-org", Name: runner.runnerID, Private: true}}}
		response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories/report", runner.secret, report)
		requireNativeStatus(t, response, http.StatusOK)
		response = performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories/report", runner.secret, report)
		requireNativeStatus(t, response, http.StatusOK)
		if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			list, err := r.RunnerRepositoryList(t.Context(), runner.runnerID)
			if err != nil {
				return err
			}
			repositories, err := r.RunnerRepositories(t.Context(), runner.runnerID, 10)
			if err != nil {
				return err
			}
			if list.Pending || list.RequestID != requestResult.RequestID || len(repositories) != 1 || repositories[0].Name != runner.runnerID || !repositories[0].Private {
				t.Fatalf("runner %s repository state=(%+v,%+v)", runner.runnerID, list, repositories)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		nextRequest := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": runner.runnerID, "idempotency_key": "repositories-next-" + runner.runnerID})
		requireNativeStatus(t, nextRequest, http.StatusOK)
		var nextResult struct {
			RequestID string `json:"request_id"`
		}
		decodeHubResponse(t, nextRequest, &nextResult)
		if nextResult.RequestID == requestResult.RequestID {
			t.Fatalf("runner %s new repository request reused old ID %q", runner.runnerID, nextResult.RequestID)
		}
		report["idempotency_key"] = "stale-" + runner.runnerID
		staleResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/onboarding/repositories/report", runner.secret, report)
		requireNativeStatus(t, staleResponse, http.StatusUnprocessableEntity)
		if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			list, err := r.RunnerRepositoryList(t.Context(), runner.runnerID)
			if err != nil {
				return err
			}
			if !list.Pending || list.RequestID != nextResult.RequestID {
				t.Fatalf("stale report changed current request for %s: %+v", runner.runnerID, list)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
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
	var currentIdentity runnerstore.Identity
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		currentIdentity, err = r.RunnerIdentity(t.Context(), "runner_example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runningBuild := runnerauth.BuildEvidence{Version: "0.174.1", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "darwin", Architecture: "arm64", VerifiedRelease: true, ObservedAt: clock.value}
	ownerPolicy := hubTestPolicy()
	projectConfiguration := runnerauth.ProjectConfiguration{
		ProjectID: string(project.ID), Authority: "local_global_configuration", Source: "configured_committed_workflow", ObservedAt: clock.value,
		RunnerID: currentIdentity.RunnerID, RunnerRevision: currentIdentity.Revision,
		ConfigRevision: strings.Repeat("c", 64), Registered: true, RuntimeRegistered: true, Draining: true, UnsettledAttempts: 1,
		AllowLocalBinding: true, LocalIntakeEnabled: true,
		EffectivePolicy: &ownerPolicy, SelectedPolicy: &ownerPolicy, LocalBindingPolicy: &ownerPolicy, RestrictedBindingPolicy: &ownerPolicy,
		LocalIntakeRemaining: []string{"wi_example"}, LocalIntakeBlocked: []string{"wi_blocked"},
		Diagnostics: &runnerauth.ProjectDiagnostics{
			ObservedAt: clock.value, RuntimeObservedAt: clock.value, Source: "runner_runtime_and_durable_attempt_owners", RunningBuild: &runningBuild,
			Counts: map[string]int{"native_claim": 1, "distinct_records": 1}, Unavailable: map[string]string{},
			Records: []runnerauth.DiagnosticAttempt{{Key: "local:1", IssueID: "wi_example", LocalAttemptID: 1, Generation: 2, Fence: 3, Stage: "implementing", Membership: []string{"running", "native_claim"}, ProviderCompletedAt: clock.value.Add(-3 * time.Second), HostOperation: "append.run.observed", OperationPending: true, OperationStartedAt: clock.value.Add(-2 * time.Second), OperationObservedAt: clock.value.Add(-time.Second), LatestErrorCode: "invalid_request", DeferredAt: clock.value.Add(-time.Second), RetryAt: clock.value.Add(time.Second), RetryAttempt: 1, RecoveryOwner: "instance", LeaseRenewedAt: clock.value, LeaseExpiresAt: clock.value.Add(time.Hour)}},
		},
	}
	if err := projectConfiguration.Validate(); err != nil {
		t.Fatalf("invalid project owner projection fixture: %v", err)
	}
	if err := validateTenantHeartbeatProjection(nativeMachineHeartbeatRequest{ProjectConfiguration: &projectConfiguration}); err != nil {
		t.Fatalf("rejected project owner projection fixture: %v", err)
	}
	for name, mutate := range map[string]func(*runnerauth.ProjectConfiguration){
		"runner identity": func(view *runnerauth.ProjectConfiguration) { view.RunnerID = "runner_other" },
		"runner revision": func(view *runnerauth.ProjectConfiguration) { view.RunnerRevision++ },
	} {
		invalid := projectConfiguration
		mutate(&invalid)
		response := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2, "project_configuration": invalid})
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s mismatch status = %d, want %d: %s", name, response.Code, http.StatusUnprocessableEntity, response.Body.String())
		}
	}
	for name, mutate := range map[string]func(*runnerauth.ProjectConfiguration){
		"authority":              func(view *runnerauth.ProjectConfiguration) { view.Authority = "other" },
		"configuration revision": func(view *runnerauth.ProjectConfiguration) { view.ConfigRevision = "invalid" },
		"configuration time":     func(view *runnerauth.ProjectConfiguration) { view.ObservedAt = time.Time{} },
		"diagnostics time":       func(view *runnerauth.ProjectConfiguration) { view.Diagnostics.ObservedAt = time.Time{} },
	} {
		invalid := projectConfiguration
		diagnostics := *projectConfiguration.Diagnostics
		invalid.Diagnostics = &diagnostics
		mutate(&invalid)
		if err := validateTenantHeartbeatProjection(nativeMachineHeartbeatRequest{ProjectConfiguration: &invalid}); err == nil {
			t.Fatalf("accepted owner projection with invalid %s", name)
		}
	}
	observed := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "protocol_major": 2, "project_configuration": projectConfiguration})
	requireNativeStatus(t, observed, http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		observation, attempts, err := r.RunnerProjectObservation(t.Context(), "runner_example", project.ID, "wi_example")
		if err != nil {
			return err
		}
		wantDigest, err := projectConfigurationDigest(projectConfiguration)
		if err != nil {
			return err
		}
		if observation.SnapshotDigest != wantDigest {
			t.Fatalf("persisted project configuration digest = %q, want enriched projection digest %q", observation.SnapshotDigest, wantDigest)
		}
		if !observation.DiagnosticsAt.Equal(clock.value) || !observation.RuntimeObservedAt.Equal(clock.value) || len(attempts) != 1 || attempts[0].LocalAttemptID != 1 || attempts[0].Generation != 2 || attempts[0].Fence != 3 || !attempts[0].Running || !attempts[0].NativeClaim || attempts[0].HostOperation != "append.run.observed" || !attempts[0].OperationPending || attempts[0].ProviderCompletedAt == nil || !attempts[0].ProviderCompletedAt.Equal(clock.value.Add(-3*time.Second)) || attempts[0].LeaseRenewedAt == nil || !attempts[0].LeaseRenewedAt.Equal(clock.value) {
			t.Fatalf("persisted project diagnostics = %+v / %+v", observation, attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	baseline := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "change_cursor": ""})
	requireNativeStatus(t, baseline, http.StatusOK)
	var baselineSnapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, baseline, &baselineSnapshot)
	if baselineSnapshot.Routing.UpdateRequest == nil || !baselineSnapshot.Routing.UpdateRequest.Urgent || baselineSnapshot.Routing.UpdateRequest.Version != "0.174.4" {
		t.Fatalf("urgent update was not included in runner routing: %+v", baselineSnapshot.Routing)
	}
	if baselineSnapshot.Changes == nil || !baselineSnapshot.Changes.Reset || baselineSnapshot.Changes.Cursor == "" {
		t.Fatalf("initial heartbeat changes = %+v", baselineSnapshot.Changes)
	}
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "heartbeat-change"}, Title: "Heartbeat change", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var heartbeatIssue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &heartbeatIssue)
	changed := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "change_cursor": baselineSnapshot.Changes.Cursor})
	requireNativeStatus(t, changed, http.StatusOK)
	var changedSnapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, changed, &changedSnapshot)
	if changedSnapshot.Changes == nil || changedSnapshot.Changes.Reset || len(changedSnapshot.Changes.Items) != 1 || changedSnapshot.Changes.Items[0].ID != heartbeatIssue.WorkItemID {
		t.Fatalf("heartbeat event delta = %+v, issue=%s", changedSnapshot.Changes, heartbeatIssue.WorkItemID)
	}
	heartbeatCredential, status, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, hash, "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate heartbeat cursor credential: status=%d err=%v", status, err)
	}
	_, legacyCursor, cursorKey, err := service.readNativePage(t.Context(), nativeScope{organization: organization.ID, project: project.ID, credential: heartbeatCredential}, path+"/changes", url.Values{"cursor": {""}})
	if err != nil {
		t.Fatal(err)
	}
	legacyPosition, err := json.Marshal(struct {
		Sequence int64     `json:"sequence"`
		Observed time.Time `json:"observed_at"`
	}{Sequence: 1, Observed: clock.value})
	if err != nil {
		t.Fatal(err)
	}
	legacyCursor.After = string(legacyPosition)
	legacyToken, err := encodeNativeCursor(legacyCursor, cursorKey)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "change_cursor": legacyToken})
	requireNativeStatus(t, legacyResponse, http.StatusOK)
	var legacySnapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, legacyResponse, &legacySnapshot)
	if legacySnapshot.Changes == nil || !legacySnapshot.Changes.Reset || legacySnapshot.Changes.Cursor == "" {
		t.Fatalf("legacy rowid cursor did not reset against matching PG sequence: %+v", legacySnapshot.Changes)
	}
	sqlitePosition, err := json.Marshal(heartbeatPosition{Version: sqliteHeartbeatCursorVersion, Sequence: 1, Observed: clock.value})
	if err != nil {
		t.Fatal(err)
	}
	legacyCursor.After = string(sqlitePosition)
	sqliteToken, err := encodeNativeCursor(legacyCursor, cursorKey)
	if err != nil {
		t.Fatal(err)
	}
	sqliteResponse := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "change_cursor": sqliteToken})
	requireNativeStatus(t, sqliteResponse, http.StatusOK)
	var sqliteSnapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, sqliteResponse, &sqliteSnapshot)
	if sqliteSnapshot.Changes == nil || !sqliteSnapshot.Changes.Reset || sqliteSnapshot.Changes.Cursor == "" {
		t.Fatalf("SQLite cursor version %d did not reset against matching PG sequence: %+v", sqliteHeartbeatCursorVersion, sqliteSnapshot.Changes)
	}
	secondIssueResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "heartbeat-change-next"}, Title: "Heartbeat change next", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, secondIssueResponse, http.StatusOK)
	var secondHeartbeatIssue tracker.NativeIssue
	decodeHubResponse(t, secondIssueResponse, &secondHeartbeatIssue)
	nextChanged := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "change_cursor": sqliteSnapshot.Changes.Cursor})
	requireNativeStatus(t, nextChanged, http.StatusOK)
	var nextChangedSnapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, nextChanged, &nextChangedSnapshot)
	if nextChangedSnapshot.Changes == nil || nextChangedSnapshot.Changes.Reset || len(nextChangedSnapshot.Changes.Items) != 1 || nextChangedSnapshot.Changes.Items[0].ID != secondHeartbeatIssue.WorkItemID {
		t.Fatalf("versioned heartbeat event delta = %+v, issue=%s", nextChangedSnapshot.Changes, secondHeartbeatIssue.WorkItemID)
	}
	before := clock.value
	clock.value = now.Add(3 * time.Minute)
	checkoutHeartbeat := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"display_name": "Example host", "capacity": 3, "version": "0.174.1", "os": "darwin", "architecture": "arm64", "checkout_repository": "owner/repository"})
	requireNativeStatus(t, checkoutHeartbeat, http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		persisted, err = r.RunnerIdentity(t.Context(), "runner_example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !persisted.LastHeartbeatAt.Equal(clock.value) || !persisted.LastHeartbeatAt.After(before) {
		t.Fatalf("checkout heartbeat did not advance timestamp: before=%s after=%s", before, persisted.LastHeartbeatAt)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		verified, err := r.RunnerCheckoutVerified(t.Context(), project.ID, "owner/repository", clock.value)
		if err != nil {
			return err
		}
		if !verified {
			t.Fatal("accepted checkout report is not visible to its authorized project")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		record.Settings.CheckoutRepository = "owner/repository"
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	connector, repositoryID, enabled, err := service.projectRepositoryBinding(t.Context(), nil, nativeScope{organization: organization.ID, project: project.ID})
	if err != nil || connector == nil || connector.Repository != "owner/repository" || repositoryID != 0 || enabled {
		t.Fatalf("postgres checkout binding=%+v repository_id=%d enabled=%t error=%v", connector, repositoryID, enabled, err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), "runner_example")
		if err != nil {
			return err
		}
		identity.ExpiresAt = clock.value.Add(-time.Second)
		return w.UpdateRunnerIdentity(t.Context(), identity)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		verified, err := r.RunnerCheckoutVerified(t.Context(), project.ID, "owner/repository", clock.value)
		if err != nil {
			return err
		}
		if verified {
			t.Fatal("expired runner identity verified a fresh checkout report")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		deletedAt := clock.value
		record.Core.DeletedAt = &deletedAt
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deletedProjectEnrollment := performHubAPIRequest(t, service, http.MethodPost, enrollmentPath, testHubAdminToken, map[string]any{
		"runner_id": "runner_deleted_project", "machine_id": "machine_deleted_project",
		"scope": "projects", "project_ids": []string{string(project.ID)}, "operations": []string{runnerauth.Read}, "ttl_seconds": 300,
	})
	requireNativeStatus(t, deletedProjectEnrollment, http.StatusUnprocessableEntity)
	var mutations, heartbeats int64
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		for offset := 0; offset <= 3; offset++ {
			window := usageWindow.Add(time.Duration(offset) * time.Minute)
			mutationCount, err := r.BillingUsage(t.Context(), window, "api_mutations")
			if errors.Is(err, corestore.ErrNotFound) {
				mutationCount = 0
			} else if err != nil {
				return err
			}
			heartbeatCount, err := r.BillingUsage(t.Context(), window, "heartbeats")
			if errors.Is(err, corestore.ErrNotFound) {
				heartbeatCount = 0
			} else if err != nil {
				return err
			}
			mutations += mutationCount
			heartbeats += heartbeatCount
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if mutations != 25 || heartbeats != 13 {
		t.Fatalf("runner protocol usage api_mutations=%d heartbeats=%d want=25/13", mutations, heartbeats)
	}
}

func TestRunnerTenantUsageUsesBillingReceiptsAndPreservesExemptions(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)}
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, now: clock.Now}}
	organization := nativeOrganization{ID: "org_example", Name: "Example organization", Slug: "example"}
	service.database.hostedOrganization = organization.ID
	service.database.billingRepository = repository
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	plan := billingstore.PlanReference{ID: "runner_usage", Version: 1}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, Local: true, CreatedAt: clock.Now()}); err != nil {
			return err
		}
		if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization.ID, PlanReference: plan, Name: "Runner usage", Allowances: map[string]int64{"api_mutations": 1}}); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization.ID, Base: plan}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(ctx context.Context, id string, completion, heartbeat bool) error {
		return repository.WriteTenant(ctx, organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return service.applyRunnerTenantUsage(ctx, w, clock.Now(), id, completion, heartbeat)
		})
	}
	if err := apply(t.Context(), "mutation_one", false, false); err != nil {
		t.Fatal(err)
	}
	if err := apply(t.Context(), "mutation_one", false, false); err != nil {
		t.Fatalf("same receipt replay: %v", err)
	}
	if err := apply(t.Context(), "mutation_denied", false, false); err == nil {
		t.Fatal("mutation beyond the allowance was accepted")
	} else {
		var limit *hostedLimitError
		if !errors.As(err, &limit) || limit.Resource != "api_mutations" {
			t.Fatalf("over-limit error=%v", err)
		}
	}
	if err := apply(t.Context(), "completion", true, false); err != nil {
		t.Fatalf("completion was denied at the limit: %v", err)
	}
	budgeted := context.WithValue(t.Context(), runnerRequestBudgetContext{}, true)
	if err := apply(budgeted, "budgeted", false, true); err != nil {
		t.Fatalf("pre-budgeted heartbeat: %v", err)
	}
	window := time.Unix(clock.value.Unix()/60*60, 0).UTC()
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		mutations, err := r.BillingUsage(t.Context(), window, "api_mutations")
		if err != nil {
			return err
		}
		heartbeats, err := r.BillingUsage(t.Context(), window, "heartbeats")
		if err != nil {
			return err
		}
		if mutations != 2 || heartbeats != 1 {
			t.Fatalf("usage api_mutations=%d heartbeats=%d", mutations, heartbeats)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	missingAllowancePlan := billingstore.PlanReference{ID: "runner_usage_missing_allowance", Version: 1}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization.ID, PlanReference: missingAllowancePlan, Name: "Runner usage without mutation allowance"}); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization.ID, Base: missingAllowancePlan}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := apply(t.Context(), "mutation_missing_allowance", false, false); err == nil {
		t.Fatal("mutation with omitted api_mutations allowance was accepted")
	} else {
		var limit *hostedLimitError
		if !errors.As(err, &limit) || limit.Resource != "api_mutations" || limit.Allowance != 0 {
			t.Fatalf("omitted allowance error=%v, want api_mutations allowance 0", err)
		}
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
