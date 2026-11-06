package hubserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantMachineRegistrationWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Logger: slog.Default(), now: func() time.Time { return now }}}
	e := echo.New()
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{DisablePrintStack: true}))
	service.registerPostgresRoutes(e)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			expires := now.Add(time.Hour)
			for _, token := range []string{"runner", "worker", "other"} {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: token, Name: token, Hash: apikey.HashToken(string(org) + "_" + token), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
					return err
				}
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine", Hostname: "example", DisplayName: "Original", Capacity: 5, Version: "old"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment", RunnerID: "runner", MachineID: "machine", TokenHash: apikey.HashToken(string(org) + "_enrollment"), Scope: "organization", Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example"}); err != nil {
				return err
			}
			return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: "runner", MachineID: "machine", TokenID: "runner", EnrollmentID: "enrollment", Scope: "organization", Operations: operations, DisplayName: "Example runner", CapacityLimit: 4, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			call := func(token, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+org+"/projects/project/machines/register", strings.NewReader(body))
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec
			}
			body := `{"id":"machine","hostname":"ignored","display_name":"ignored","capacity":3,"version":"0.177.4","os":"linux","architecture":"amd64","backend_isolation":{"example":["sandbox"]},"workspace_capabilities":{"terminal":true,"files":true},"workspace_isolation":"sandbox","checkout_repository":"owner/repository","skills":[{"name":"example","scope":"project","invocation":"$example"}],"provider_reports":[{"backend":"example","provider":"example","account_alias":"example","billing":"api_key","availability":"available","models":["example"],"observed_at":"` + now.Format(time.RFC3339Nano) + `"}]}`
			for range 2 {
				rec := call(org+"_runner", body)
				if rec.Code != http.StatusOK {
					t.Fatalf("register status=%d body=%s", rec.Code, rec.Body)
				}
				var snapshot runnerauth.RoutingSnapshot
				if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.RunnerID != "runner" || snapshot.ClaimState == nil || snapshot.ClaimState.HostCapacity != 5 || snapshot.ClaimState.RunnerCapacity != 3 {
					t.Fatalf("routing=%+v", snapshot)
				}
			}
			otherToken := "org_first_runner"
			if org == "org_first" {
				otherToken = "org_second_runner"
			}
			for _, tc := range []struct {
				token, body string
				status      int
			}{
				{org + "_runner", strings.Replace(body, `"id":"machine"`, `"id":"different"`, 1), http.StatusNotFound},
				{"invalid", body, http.StatusUnauthorized},
				{otherToken, body, http.StatusUnauthorized},
				{org + "_worker", body, http.StatusUnprocessableEntity},
				{org + "_worker", `{"id":"new","hostname":"example","capacity":2,"version":"0.177.4","os":"linux","architecture":"amd64"}`, http.StatusOK},
				{org + "_other", `{"id":"new","hostname":"example","capacity":9,"version":"0.177.4","os":"linux","architecture":"amd64"}`, http.StatusNotFound},
			} {
				rec := call(tc.token, tc.body)
				if rec.Code != tc.status {
					t.Errorf("status=%d want %d body=%s", rec.Code, tc.status, rec.Body)
				}
			}
			if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
				machine, err := r.Machine(t.Context(), "machine")
				if err != nil {
					return err
				}
				if machine.DisplayName != "Original" || machine.Capacity != 5 || machine.Version != "0.177.4" {
					t.Errorf("machine=%+v", machine)
				}
				reports, err := r.RunnerProviderReports(t.Context(), "runner")
				if err != nil {
					return err
				}
				if len(reports) != 1 {
					t.Errorf("reports=%+v", reports)
				}
				skills, err := r.RunnerProjectSkills(t.Context(), "project", "runner", now, 100)
				if err != nil {
					return err
				}
				if len(skills) != 1 {
					t.Errorf("skills=%+v", skills)
				}
				capabilities, isolation, err := r.RunnerWorkspaceReport(t.Context(), "runner")
				if err != nil {
					return err
				}
				if !capabilities.Terminal || !capabilities.Files || isolation != "sandbox" {
					t.Errorf("workspace=%+v isolation=%s", capabilities, isolation)
				}
				backends, err := r.RunnerBackendIsolation(t.Context(), "runner")
				if err != nil {
					return err
				}
				if len(backends["example"]) != 1 || backends["example"][0] != "sandbox" {
					t.Errorf("backend isolation=%+v", backends)
				}
				verified, err := r.RunnerCheckoutVerified(t.Context(), "project", "owner/repository", now)
				if err != nil {
					return err
				}
				if !verified {
					t.Error("checkout report was not persisted")
				}
				machine, err = r.Machine(t.Context(), "new")
				if err == nil && machine.Capacity != 2 {
					t.Errorf("credential takeover changed capacity: %+v", machine)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
