package hubserver

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/gotestreceiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attestGoTestReceipts(t *testing.T, service *Service, base, token string) gotestreceipt.Attestation {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts/attestations", token, nil)
	requireNativeStatus(t, response, http.StatusCreated)
	var attestation gotestreceipt.Attestation
	decodeHubResponse(t, response, &attestation)
	if err := attestation.Validate(); err != nil {
		t.Fatal(err)
	}
	return attestation
}

func lookupGoTestReceipts(t *testing.T, service *Service, base, token, key string) []gotestreceipt.Candidate {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodGet, base+"/go-test-receipts?key="+key, token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var body struct {
		Candidates []gotestreceipt.Candidate `json:"candidates"`
	}
	decodeHubResponse(t, response, &body)
	return body.Candidates
}

func TestGoTestReceiptRoutesUseSelectedTenantStoreWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{database: &database{}, echo: echo.New(), secretRewrapReady: make(chan struct{}), config: Config{TenantRepository: repository, AccessRepository: repository, SecretKeys: secretTestKeys(t, "1", "1"), Logger: discardLogger(), now: func() time.Time { return now }}}
	service.registerNativeRoutes(service.echo)
	var credentials = map[tracker.OrganizationID]string{}
	var projects = map[tracker.OrganizationID]tracker.ProjectID{}
	for _, organization := range []tracker.OrganizationID{"org_receipt_first", "org_receipt_second"} {
		project := tracker.ProjectID("prj_" + string(organization))
		binding := runnerauth.NewBinding()
		machineID := binding.MachineID
		runnerID := binding.RunnerID
		enrollmentID := "enrollment_" + string(organization)
		token, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		tokenHash := apikey.HashToken(token)
		expires := now.Add(time.Hour)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization)}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "worker", Name: "Example runner", Hash: tokenHash, Scope: string(apiScopeWorker), Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "worker", ProjectID: project}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: "example-host", Capacity: 1}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_token"), Scope: "projects", Operations: []string{runnerauth.Read, runnerauth.Events}, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example_admin", RedeemedAt: &now}); err != nil {
				return err
			}
			return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: machineID, TokenID: "worker", EnrollmentID: enrollmentID, Scope: "projects", Operations: []string{runnerauth.Read, runnerauth.Events}, DisplayName: "Example runner", CapacityLimit: 1, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
		}); err != nil {
			t.Fatalf("seed organization %s: %v", organization, err)
		}
		credentials[organization] = token
		projects[organization] = project
		credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, tokenHash, "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("authenticate organization %s: status=%d err=%v", organization, status, err)
		}
		scope := nativeScope{organization: organization, project: project, credential: credential}
		if err := service.authorizeProjectRecord(t.Context(), scope, true); err != nil {
			t.Fatalf("authorize organization %s: %v", organization, err)
		}
		if _, err := service.readProjectRecord(t.Context(), scope); err != nil {
			t.Fatalf("read project %s: %v", project, err)
		}
	}
	key := strings.Repeat("a", 64)
	var previousDigestKey []byte
	for organization, token := range credentials {
		base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(projects[organization])
		attestation := attestGoTestReceipts(t, service, base, token)
		repeated := attestGoTestReceipts(t, service, base, token)
		if repeated.ID == attestation.ID || !bytes.Equal(repeated.DigestKey, attestation.DigestKey) || bytes.Equal(previousDigestKey, attestation.DigestKey) {
			t.Fatal("attestations must be distinct with shared project keys and isolated tenant keys")
		}
		previousDigestKey = attestation.DigestKey
		receipt := gotestreceipt.Receipt{Attestation: attestation.ID, Key: key, Package: "internal/example", Inputs: []string{"env\tHOME"}, Digest: strings.Repeat("b", 64)}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts", token, receipt), http.StatusNoContent)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts", token, receipt), http.StatusNoContent)
		for _, invalid := range []struct {
			name    string
			runner  string
			created time.Time
			expires time.Time
		}{
			{name: "another_runner", runner: "other_runner", created: now, expires: now.Add(time.Hour)},
			{name: "expired", runner: "other_runner", created: now.Add(-2 * time.Hour), expires: now.Add(-time.Hour)},
		} {
			id := "gtra_" + invalid.name
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentityByCredential(t.Context(), "worker")
				if err != nil {
					return err
				}
				runner := invalid.runner
				if invalid.name == "expired" {
					runner = identity.RunnerID
				}
				return w.CreateGoTestReceiptAttestation(t.Context(), gotestreceiptstore.Attestation{OrganizationID: organization, ProjectID: projects[organization], ID: id, CredentialID: "worker", RunnerID: runner, CreatedAt: invalid.created, ExpiresAt: invalid.expires})
			}); err != nil {
				t.Fatal(err)
			}
			forged := receipt
			forged.Attestation = id
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts", token, forged)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("%s accepted: %d %s", invalid.name, response.Code, response.Body.String())
			}
		}
		for _, invalid := range []gotestreceipt.Receipt{
			{Key: key, Package: "internal/example", Digest: receipt.Digest},
			{Attestation: "gtra_missing", Key: key, Package: "internal/example", Digest: receipt.Digest},
			{Attestation: attestation.ID, Key: key, Package: "internal/example", Digest: receipt.Digest, Inputs: []string{"open\ta\t/etc/hosts"}},
			{Attestation: attestation.ID, Key: key, Package: "internal/example", Digest: receipt.Digest, Inputs: []string{"open\tm\t/etc/hosts"}},
			{Attestation: attestation.ID, Key: key, Package: "internal/example", Digest: receipt.Digest, Inputs: []string{"open\tm\tline\nbreak"}},
		} {
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts", token, invalid)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("invalid receipt accepted: %d %s", response.Code, response.Body.String())
			}
		}
		candidates := lookupGoTestReceipts(t, service, base, token, key)
		if len(candidates) != 1 || candidates[0].Digest != receipt.Digest || !slices.Equal(candidates[0].Inputs, receipt.Inputs) {
			t.Fatalf("organization %s selected receipt candidates = %+v", organization, candidates)
		}
		for other, otherToken := range credentials {
			if other == organization {
				continue
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"/go-test-receipts?key="+key, otherToken, nil), http.StatusUnauthorized)
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"/go-test-receipts?key=ABC", token, nil), http.StatusUnprocessableEntity)
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			expired, err := r.GoTestReceipts(t.Context(), projects[organization], key, now.Add(time.Hour), gotestreceipt.MaxCandidates)
			if len(expired) != 0 {
				t.Fatalf("expired candidates before pruning: %+v", expired)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		service.config.Hosted = &HostedConfig{OrganizationID: string(organization)}
		if err := service.expireGoTestReceipts(t.Context(), now.Add(goTestReceiptRetention+time.Hour), time.Now); err != nil {
			t.Fatal(err)
		}
		service.config.Hosted = nil
		if candidates := lookupGoTestReceipts(t, service, base, token, key); len(candidates) != 0 {
			t.Fatalf("pruned candidates: %+v", candidates)
		}
	}
}

func TestGoTestReceiptOperationsRequireTypedOwner(t *testing.T) {
	for _, operation := range []string{"lookup", "attest", "record", "prune"} {
		t.Run(operation, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("receipt operation accessed missing legacy storage: %v", recovered)
				}
			}()
			service := &Service{config: Config{Logger: discardLogger(), now: time.Now}}
			if operation == "prune" {
				var unavailable *nativeError
				if err := service.expireGoTestReceipts(t.Context(), time.Now(), time.Now); !errors.As(err, &unavailable) || unavailable.status != http.StatusServiceUnavailable {
					t.Fatalf("missing typed owner error = %v", err)
				}
				return
			}
			body := `{"attestation":"gtra_example","key":"` + strings.Repeat("a", 64) + `","digest":"` + strings.Repeat("b", 64) + `","package":"internal/example","inputs":[]}`
			request := httptest.NewRequest(http.MethodPost, "/?key="+strings.Repeat("a", 64), strings.NewReader(body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			ctx := echo.New().NewContext(request, response)
			var err error
			switch operation {
			case "lookup":
				err = service.lookupGoTestReceipts(ctx)
			case "attest":
				err = service.attestGoTestReceipts(ctx)
			case "record":
				err = service.recordGoTestReceipt(ctx)
			}
			if err != nil || response.Code != http.StatusServiceUnavailable {
				t.Fatalf("missing typed owner status=%d error=%v", response.Code, err)
			}
		})
	}
}

func TestGoTestReceiptAuthorityRejectsNonRunnerCredentials(t *testing.T) {
	for _, scope := range []apiScope{apiScopeAdmin, apiScopeOperator, apiScopeWorker} {
		t.Run(string(scope), func(t *testing.T) {
			service := &Service{}
			_, err := service.requireGoTestReceiptTenantAuthority(t.Context(), nil, nativeScope{credential: apiCredential{Scope: scope}}, time.Now())
			var unavailable *nativeError
			if !errors.As(err, &unavailable) || unavailable.status != http.StatusNotFound {
				t.Fatalf("non-runner authority: %v", err)
			}
		})
	}
}
