package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRunnerRoutingHTTPWithoutLegacyDatabase(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	asOf := now
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Logger: slog.Default(), now: func() time.Time { return now }, Version: "0.177.4"}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin_" + string(org), Name: "Example admin", Hash: apikey.HashToken(string(org) + "_admin"), Scope: "admin", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
				return err
			}
			for _, id := range []string{"one", "two"} {
				project := tracker.ProjectID("project_" + id)
				runner, machine := "runner_"+id, "machine_"+id
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + id, Slug: "example-" + id, Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				expires := now.Add(time.Hour)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: runner, Name: runner, Hash: apikey.HashToken(string(org) + "_" + runner), Scope: "worker", NativeOnly: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: runner, ProjectID: project}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: machine, Hostname: string(org) + "-" + id, DisplayName: "Example host " + id, Capacity: 5, Version: "0.177.4"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment_" + id, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken(string(org) + "_enroll_" + id), Scope: "projects", Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires, CreatedBy: "admin_" + string(org)}); err != nil {
					return err
				}
				identity := runnerstore.Identity{OrganizationID: org, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: runner, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", DisplayName: "Example runner " + id, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 3, ReportedCapacity: 3, OS: "linux", Architecture: "arm64", MachineUse: "shared", IsolationTier: "native-trusted", Tags: []string{"example"}, HostServices: []string{"tcp:127.0.0.1:5432"}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires, LastHeartbeatAt: now}
				if err := w.CreateRunnerIdentity(t.Context(), identity); err != nil {
					return err
				}
				identity.ProjectRanks = map[tracker.ProjectID]int{project: 7}
				identity.ProjectCapacityLimits = map[tracker.ProjectID]int{project: 2}
				if err := w.UpdateRunnerRouting(t.Context(), 1, identity); err != nil {
					return err
				}
				report := providercapacity.Report{Backend: "example", Provider: "example", AccountAlias: "example-account", SharedAccountAlias: "example-shared", Billing: "api_key", Models: []string{"example-model"}, Availability: "available", ObservedAt: now, MaxConcurrent: 3}
				if err := w.ReplaceRunnerProviderReports(t.Context(), runner, []providercapacity.Report{report}); err != nil {
					return err
				}
				if err := w.UpdateRunnerCapacityConfig(t.Context(), runner, runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 3, ClientLimit: 3, RuntimeLimit: 3, Manageable: true, ObservedAt: now}); err != nil {
					return err
				}
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID("item_" + id), Number: 1, Title: "Example issue " + id})
				if err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: machine, SessionID: "session_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
				if err != nil {
					return err
				}
				if lease.RenewedAt.After(asOf) {
					asOf = lease.RenewedAt
				}
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: runner, LeaseID: lease.LeaseID, AssignedAt: now}); err != nil {
					return err
				}
				if err := w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: org, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "example", Model: "example-model"}, Report: report, Reason: "Example capacity"}}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	now = asOf.Add(time.Second)
	e := echo.New()
	service.registerPostgresRoutes(e)
	request := func(org, runner, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v2/organizations/%s/runners/%s/routing", org, runner), nil)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	t.Run("hosted fleet pages", func(t *testing.T) {
		defer func() { service.config.Hosted = nil }()
		for _, org := range []string{"org_first", "org_second"} {
			provider := newHostedSecurityProvider()
			membership := auth.Membership{ID: "membership_list", UserID: "user_list", OrganizationID: "provider_" + org, Status: "active"}
			membership.Role.Slug = "admin"
			provider.members[membership.ID] = membership
			if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: "credential_list", Name: "Example list admin", Hash: apikey.HashToken(org + "_list"), Scope: "operator", NativeOnly: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, UserID: membership.UserID, MembershipID: membership.ID, KeyScope: string(apikey.ScopeAdmin), ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: tracker.OrganizationID(org), UserID: membership.UserID, Email: "list@example.test", MembershipID: membership.ID, Role: "admin", Active: true, PrincipalID: "credential_list", CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			service.config.Hosted = &HostedConfig{OrganizationID: org, Provider: provider}
			hosted := echo.New()
			service.registerPostgresRoutes(hosted)
			for _, tc := range []struct{ query, id, next string }{
				{"?limit=1", "one", "1"}, {"?limit=1&offset=1", "two", ""}, {"?limit=1&offset=2", "", ""},
			} {
				req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/"+org+"/runners"+tc.query, nil)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+org+"_list")
				rec := httptest.NewRecorder()
				hosted.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("fleet status=%d body=%s", rec.Code, rec.Body.String())
				}
				var runners []runnerauth.Runner
				if err := json.Unmarshal(rec.Body.Bytes(), &runners); err != nil {
					t.Fatal(err)
				}
				wantCount := 1
				if tc.id == "" {
					wantCount = 0
				}
				if rec.Header().Get("X-Next-Offset") != tc.next || len(runners) != wantCount {
					t.Fatalf("page %s: next=%s runners=%+v", tc.query, rec.Header().Get("X-Next-Offset"), runners)
				}
				for _, got := range runners {
					project := tracker.ProjectID("project_" + tc.id)
					if got.RunnerID != "runner_"+tc.id || got.OrganizationID != tracker.OrganizationID(org) || got.Hostname != org+"-"+tc.id || got.CapacityLimit != 3 || got.HostCapacity != 5 || got.HostUsed != 1 || got.Used != 1 || got.ProjectUsed[project] != 1 || got.ProjectRanks[project] != 7 || got.ProjectCapacityLimits[project] != 2 || got.CapacityConfig == nil {
						t.Fatalf("fleet facts=%+v", got)
					}
					if len(got.Leases) != 1 || got.Leases[0].ProjectID != project || got.Leases[0].ProviderReservation == nil || len(got.ProviderCapacity) != 1 || got.ProviderCapacity[0].Used != 2 || got.ProviderCapacity[0].MaxConcurrent != 3 {
						t.Fatalf("fleet leases/providers=%+v / %+v", got.Leases, got.ProviderCapacity)
					}
				}
			}
			for _, query := range []string{"?limit=0", "?limit=201", "?offset=-1", "?offset=invalid", "?offset=100001"} {
				req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/"+org+"/runners"+query, nil)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+org+"_list")
				rec := httptest.NewRecorder()
				hosted.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("invalid page %s: status=%d body=%s", query, rec.Code, rec.Body.String())
				}
			}
			if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
				for index := range 49 {
					id := fmt.Sprintf("runner_page_%02d", index)
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: id, Name: id, Hash: apikey.HashToken(org + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
						return err
					}
					if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: tracker.OrganizationID(org), ID: id, RunnerID: id, MachineID: "machine_one", TokenHash: apikey.HashToken(org + id + "enroll"), Scope: "organization", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin_" + org}); err != nil {
						return err
					}
					if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: tracker.OrganizationID(org), RunnerID: id, MachineID: "machine_one", TokenID: id, EnrollmentID: id, Scope: "organization", State: "active", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"", "?limit=200"} {
				req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/"+org+"/runners"+query, nil)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+org+"_list")
				rec := httptest.NewRecorder()
				hosted.ServeHTTP(rec, req)
				if query == "" && rec.Code != http.StatusUnprocessableEntity || query != "" && rec.Code != http.StatusOK {
					t.Fatalf("large fleet %q: status=%d body=%s", query, rec.Code, rec.Body.String())
				}
				if query != "" {
					var runners []runnerauth.Runner
					if err := json.Unmarshal(rec.Body.Bytes(), &runners); err != nil || len(runners) != 51 {
						t.Fatalf("large page count=%d error=%v", len(runners), err)
					}
				}
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_other/runners", nil)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+org+"_list")
			rec := httptest.NewRecorder()
			hosted.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("cross-organization list status=%d", rec.Code)
			}
		}
	})
	for _, org := range []string{"org_first", "org_second"} {
		for _, id := range []string{"one", "two"} {
			for _, token := range []string{org + "_runner_" + id, org + "_admin"} {
				t.Run(org+"/"+id+"/"+token, func(t *testing.T) {
					rec := request(org, "runner_"+id, token)
					if rec.Code != http.StatusOK {
						t.Fatalf("routing status=%d body=%s", rec.Code, rec.Body.String())
					}
					if rec.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("routing response is cacheable")
					}
					var got runnerauth.Runner
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					project := tracker.ProjectID("project_" + id)
					if got.RunnerID != "runner_"+id || got.OrganizationID != tracker.OrganizationID(org) || got.Hostname != org+"-"+id || got.HostDisplayName != "Example host "+id || got.HostCapacity != 5 || got.OS != "linux" || got.Architecture != "arm64" {
						t.Fatalf("identity/machine facts=%+v", got)
					}
					if got.CapacityLimit != 3 || got.ReportedCapacity != 3 || got.CapacityConfig == nil || got.CapacityConfig.RuntimeLimit != 3 || got.Used != 1 || got.HostUsed != 1 || got.ProjectUsed[project] != 1 {
						t.Fatalf("capacity facts=%+v", got)
					}
					if got.Scope != "projects" || !slices.Equal(got.ProjectIDs, []tracker.ProjectID{project}) || got.ProjectRanks[project] != 7 || got.ProjectCapacityLimits[project] != 2 || got.Availability.Timezone != "UTC" || got.IsolationTier != "native-trusted" || !slices.Contains(got.Tags, "example") {
						t.Fatalf("routing facts=%+v", got.Routing)
					}
					if len(got.ProviderCapacity) != 1 || got.ProviderCapacity[0].Provider != "example" || !slices.Equal(got.ProviderCapacity[0].Models, []string{"example-model"}) || got.ProviderCapacity[0].MaxConcurrent != 3 || got.ProviderCapacity[0].Used != 2 {
						t.Fatalf("provider facts=%+v", got.ProviderCapacity)
					}
					if len(got.Leases) != 1 || got.Leases[0].ProjectID != project || got.Leases[0].ProviderReservation == nil || got.Leases[0].ProviderReservation.Model != "example-model" {
						t.Fatalf("lease facts=%+v", got.Leases)
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		name, org, runner, token string
		status                   int
	}{
		{"cross runner", "org_first", "runner_two", "org_first_runner_one", http.StatusNotFound},
		{"cross organization", "org_second", "runner_one", "org_first_runner_one", http.StatusUnauthorized},
		{"missing runner", "org_first", "missing", "org_first_admin", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := request(tc.org, tc.runner, tc.token)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	for _, tc := range []struct{ name, query string }{
		{"credential revoked", `UPDATE api_tokens SET revoked_at=$1 WHERE organization_id='org_first' AND id='runner_one'`},
		{"credential expired", `UPDATE api_tokens SET expires_at=$1 WHERE organization_id='org_first' AND id='runner_one'`},
		{"identity expired", `UPDATE runner_identities SET expires_at=$1 WHERE organization_id='org_first' AND runner_id='runner_one'`},
		{"identity removed", `UPDATE runner_identities SET removed_at=$1 WHERE organization_id='org_first' AND runner_id='runner_one'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), tc.query, now.Add(-time.Second)); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			rec := request("org_first", "runner_one", "org_first_runner_one")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("invalid authority status=%d body=%s", rec.Code, rec.Body.String())
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=NULL,expires_at=$1 WHERE organization_id='org_first' AND id='runner_one'`, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET removed_at=NULL,expires_at=$1 WHERE organization_id='org_first' AND runner_id='runner_one'`, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("hosted grants cover every routing project", func(t *testing.T) {
		provider := newHostedSecurityProvider()
		membership := auth.Membership{ID: "membership_example", UserID: "user_example", OrganizationID: "provider_org_first", Status: "active"}
		membership.Role.Slug = "member"
		provider.members[membership.ID] = membership
		if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_first", ID: "credential_member", Name: "Example member", Hash: apikey.HashToken("example_member_token"), Scope: "operator", NativeOnly: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, UserID: "user_example", MembershipID: membership.ID, KeyScope: string(apikey.ScopeAdmin), ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_first", UserID: "user_example", Email: "user@example.test", MembershipID: membership.ID, Role: "member", Active: true, PrincipalID: "credential_member", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_first", TokenID: "credential_member", ProjectID: "project_one"}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_first", UserID: "user_example", ProjectID: "project_one", CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
		service.config.Hosted = &HostedConfig{OrganizationID: "org_first", Provider: provider}
		defer func() { service.config.Hosted = nil }()
		rec := request("org_first", "runner_one", "example_member_token")
		if rec.Code != http.StatusOK {
			t.Fatalf("manageable runner status=%d body=%s", rec.Code, rec.Body.String())
		}

		parentContext := t.Context()
		t.Run("hosted routing PUT preserves facts and replays", func(t *testing.T) {
			membership.Role.Slug = "admin"
			provider.members[membership.ID] = membership
			defer func() { membership.Role.Slug = "member"; provider.members[membership.ID] = membership }()
			if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_members SET role='admin' WHERE organization_id='org_first' AND user_id='user_example'`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := admin.ExecContext(parentContext, `UPDATE hosted_members SET role='member' WHERE organization_id='org_first' AND user_id='user_example'`); err != nil {
					t.Error(err)
				}
			})
			hosted := echo.New()
			service.registerPostgresRoutes(hosted)
			read := func(runner string) runnerauth.Runner {
				t.Helper()
				config := service.config.Hosted
				service.config.Hosted = nil
				rec := request("org_first", runner, "org_first_admin")
				service.config.Hosted = config
				var got runnerauth.Runner
				if rec.Code != http.StatusOK {
					t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			before, other := read("runner_one"), read("runner_two")
			change := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}, IdempotencyKey: "zero_runner_one"}
			change.ProjectCapacityLimits = map[tracker.ProjectID]int{"project_one": 0}
			put := func(token string, change runnerRoutingRequest) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPut, "/api/v2/organizations/org_first/runners/runner_one/routing", strings.NewReader(string(raw)))
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				rec := httptest.NewRecorder()
				hosted.ServeHTTP(rec, req)
				return rec
			}
			var receipt string
			for _, tc := range []struct {
				name, token, key string
				status           int
			}{
				{"apply", "example_member_token", "zero_runner_one", http.StatusOK},
				{"replay", "example_member_token", "zero_runner_one", http.StatusOK},
				{"stale", "example_member_token", "stale_runner_one", http.StatusConflict},
				{"receipt mismatch", "example_member_token", "zero_runner_one", http.StatusConflict},
				{"worker denied", "org_first_runner_one", "worker_runner_one", http.StatusNotFound},
			} {
				t.Run(tc.name, func(t *testing.T) {
					change.IdempotencyKey = tc.key
					input := change
					if tc.name == "receipt mismatch" {
						input.ProjectCapacityLimits = map[tracker.ProjectID]int{"project_one": 1}
					}
					rec := put(tc.token, input)
					if rec.Code != tc.status {
						t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
					}
					if tc.name == "apply" {
						receipt = rec.Body.String()
					}
					if tc.name == "replay" && rec.Body.String() != receipt {
						t.Fatal("replay changed receipt")
					}
				})
			}
			after := read("runner_one")
			if after.Revision != before.Revision+1 || after.ProjectCapacityLimits["project_one"] != 0 || len(after.ProjectCapacityLimits) != 1 {
				t.Fatalf("updated routing=%+v", after)
			}
			after.ProjectCapacityLimits = before.ProjectCapacityLimits
			if !reflect.DeepEqual(after.Routing, before.Routing) {
				t.Fatal("unrequested routing fields changed")
			}
			if got := read("runner_two"); !reflect.DeepEqual(got, other) {
				t.Fatal("unrelated runner changed")
			}
			t.Run("capacity increase reaches runner", func(t *testing.T) {
				current := read("runner_one")
				input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "raise_capacity"}
				input.CapacityLimit = 6
				if rec := put("example_member_token", input); rec.Code != http.StatusOK {
					t.Fatalf("raise status=%d body=%s", rec.Code, rec.Body.String())
				}
				rec := request("org_first", "runner_one", "org_first_runner_one")
				var received runnerauth.Runner
				if err := json.Unmarshal(rec.Body.Bytes(), &received); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusOK || received.CapacityLimit != 6 || received.CapacityRequest == nil || received.CapacityRequest.Capacity != 6 || received.CapacityRequest.ExpectedConfigRevision != strings.Repeat("a", 64) {
					t.Fatalf("runner capacity request=%+v status=%d", received.CapacityRequest, rec.Code)
				}
			})
			t.Run("organization edit ignores unrelated projects", func(t *testing.T) {
				if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					for i := 0; i < 201; i++ {
						id := tracker.ProjectID(fmt.Sprintf("unrelated_%d", i))
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_first", ID: id, Name: string(id), Slug: strings.ReplaceAll(string(id), "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
							return err
						}
					}
					identity, err := w.RunnerIdentity(t.Context(), "runner_one")
					if err != nil {
						return err
					}
					identity.Scope = "organization"
					if err := w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, "project_one"); err != nil {
						return err
					}
					return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET operator_project_access='all' WHERE organization_id='org_first' AND id='credential_member'`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := admin.ExecContext(parentContext, `UPDATE api_tokens SET operator_project_access='selected' WHERE organization_id='org_first' AND id='credential_member'`); err != nil {
						t.Error(err)
					}
					if _, err := admin.ExecContext(parentContext, `INSERT INTO token_grants (organization_id,token_id,project_id) VALUES ('org_first','runner_one','project_one')`); err != nil {
						t.Error(err)
					}
					if _, err := admin.ExecContext(parentContext, `UPDATE runner_identities SET scope='projects' WHERE organization_id='org_first' AND runner_id='runner_one'`); err != nil {
						t.Error(err)
					}
				}()
				current := read("runner_one")
				input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "rename_large_organization"}
				input.ProjectIDs = nil
				input.DisplayName = "Renamed runner"
				if rec := put("example_member_token", input); rec.Code != http.StatusOK {
					t.Fatalf("rename status=%d body=%s", rec.Code, rec.Body.String())
				}
				got := read("runner_one")
				if got.DisplayName != input.DisplayName || got.Revision != current.Revision+1 || !reflect.DeepEqual(got.CapacityRequest, current.CapacityRequest) {
					t.Fatal("rename lost routing or capacity request")
				}
			})
		})

		identity := auth.HostedIdentity{Subject: "user_example", OrganizationID: "provider_org_first", SessionID: "session_support_routing", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), SupportActor: "support@example.test", SupportReason: "customer-request"}
		provider.sessions[identity.SessionID] = identity
		service.config.Hosted.SupportActors = []string{identity.SupportActor}
		sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: time.Hour, PublicURL: "https://tenant.example.test"}, service, auth.WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		service.hostedSessions = sessions
		if err := service.CreateWebSession(t.Context(), auth.SessionRecord{TokenHash: apikey.HashToken("example_support_session"), Email: "user@example.test", Identity: &identity, CreatedAt: identity.CreatedAt, ExpiresAt: identity.ExpiresAt}); err != nil {
			t.Fatal(err)
		}
		supportRequest := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_first/runners/runner_one/routing", nil)
			req.AddCookie(&http.Cookie{Name: hostedCookie, Value: "example_support_session"})
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			return rec
		}
		t.Run("support administration audit", func(t *testing.T) {
			rec := supportRequest()
			if rec.Code != http.StatusOK {
				t.Fatalf("support routing status=%d body=%s", rec.Code, rec.Body.String())
			}
			var count, attributed int
			err := admin.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE event='administration' AND route=$1 AND actual_actor=$2 AND effective_user=$3 AND reason=$4) FROM hosted_audit WHERE organization_id='org_first' AND session_id=$5`, "GET "+runnerBase+"/:runner/routing", identity.SupportActor, identity.Subject, identity.SupportReason, identity.SessionID).Scan(&count, &attributed)
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 || attributed != 1 {
				t.Fatalf("support audit count=%d attributed=%d, want one attributed administration read", count, attributed)
			}
		})
		t.Run("support audit failure refuses routing", func(t *testing.T) {
			failed := &routingAuditFailureRepository{Repository: repository}
			service.config.AccessRepository = failed
			defer func() { service.config.AccessRepository = repository }()
			rec := supportRequest()
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "storage_unreachable") || strings.Contains(rec.Body.String(), "runner_id") {
				t.Fatalf("audit failure status=%d body=%s", rec.Code, rec.Body.String())
			}
			if failed.attempts != 1 {
				t.Fatalf("audit write attempts=%d, want 1", failed.attempts)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id='org_first' AND session_id=$1`, identity.SessionID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("failed audit persisted rows: count=%d, want 1 successful prior audit", count)
			}
		})
		for _, query := range []string{
			`INSERT INTO runner_project_ranks (organization_id,runner_id,project_id,rank) VALUES ('org_first','runner_one','project_two',9)`,
			`INSERT INTO runner_project_capacity_limits (organization_id,runner_id,project_id,capacity_limit) VALUES ('org_first','runner_one','project_two',1)`,
			`INSERT INTO token_grants (organization_id,token_id,project_id) VALUES ('org_first','runner_one','project_two')`,
		} {
			if _, err := admin.ExecContext(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			rec := request("org_first", "runner_one", "example_member_token")
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "project_two") {
				t.Fatalf("hidden routing project leaked: status=%d body=%s", rec.Code, rec.Body.String())
			}
			for _, table := range []string{"runner_project_ranks", "runner_project_capacity_limits", "token_grants"} {
				column := "runner_id"
				if table == "token_grants" {
					column = "token_id"
				}
				if _, err := admin.ExecContext(t.Context(), "DELETE FROM "+table+" WHERE organization_id='org_first' AND "+column+"='runner_one' AND project_id='project_two'"); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	for _, query := range []string{
		`EXPLAIN SELECT runner_id FROM runner_identities WHERE organization_id='org_first' AND runner_id='runner_one' AND removed_at IS NULL`,
		`EXPLAIN SELECT lease_id FROM leases WHERE organization_id='org_first' AND machine_id='machine_one' AND released_at IS NULL AND expires_at>now()`,
		`EXPLAIN SELECT runner_id FROM runner_provider_reports WHERE organization_id='org_first' AND runner_id='runner_one'`,
	} {
		rows, err := admin.QueryContext(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			t.Log(line)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ name, token, query string }{
		{"current machine", "org_first_runner_one", `UPDATE runner_identities SET machine_id='machine_two' WHERE organization_id='org_first' AND runner_id='runner_one'`},
		{"current credential revoked", "org_first_runner_one", `UPDATE api_tokens SET revoked_at=now() WHERE organization_id='org_first' AND id='runner_one'`},
		{"current credential expired", "org_first_runner_one", `UPDATE api_tokens SET expires_at=created_at+interval '1 second' WHERE organization_id='org_first' AND id='runner_one'`},
		{"current identity expired", "org_first_runner_one", `UPDATE runner_identities SET expires_at=created_at+interval '1 second' WHERE organization_id='org_first' AND runner_id='runner_one'`},
		{"current identity removed", "org_first_runner_one", `UPDATE runner_identities SET removed_at=now() WHERE organization_id='org_first' AND runner_id='runner_one'`},
		{"current read operation", "org_first_runner_one", `UPDATE runner_identities SET operations=ARRAY['claim'] WHERE organization_id='org_first' AND runner_id='runner_one'`},
		{"current admin scope", "org_first_admin", `UPDATE api_tokens SET scope='operator' WHERE organization_id='org_first' AND id='admin_org_first'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service.config.TenantRepository = &routingSnapshotHookRepository{Repository: repository, SnapshotRepository: repository, before: func(ctx context.Context) error { _, err := admin.ExecContext(ctx, tc.query); return err }}
			defer func() { service.config.TenantRepository = repository }()
			rec := request("org_first", "runner_one", tc.token)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Fatalf("stale authority status=%d body=%s", rec.Code, rec.Body.String())
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET machine_id='machine_one',removed_at=NULL,expires_at=$1,operations=ARRAY['read','claim'] WHERE organization_id='org_first' AND runner_id='runner_one'`, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=NULL,expires_at=$1 WHERE organization_id='org_first' AND id='runner_one'`, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET scope='admin' WHERE organization_id='org_first' AND id='admin_org_first'`); err != nil {
				t.Fatal(err)
			}
		})
	}

}

type routingSnapshotHookRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	before func(context.Context) error
}

func (r *routingSnapshotHookRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if r.before != nil {
		before := r.before
		r.before = nil
		if err := before(ctx); err != nil {
			return err
		}
	}
	return r.SnapshotRepository.ReadTenantSnapshot(ctx, organization, workload, read)
}

type routingAuditFailureRepository struct {
	tenantstore.Repository
	attempts int
}

func (r *routingAuditFailureRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error {
		return write(routingAuditFailureWriter{Writer: writer, attempts: &r.attempts})
	})
}

type routingAuditFailureWriter struct {
	tenantstore.Writer
	attempts *int
}

func (w routingAuditFailureWriter) RecordAccessAudit(context.Context, accessstore.Audit) (int64, error) {
	*w.attempts++
	return 0, artifact.ErrStorage
}
