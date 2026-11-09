package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	hubapi "github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
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
	if _, err := admin.ExecContext(t.Context(), `UPDATE machines SET routing_revision=CASE WHEN id='machine_one' THEN 7 ELSE 9 END`); err != nil {
		t.Fatal(err)
	}
	now = asOf.Add(time.Second)
	e := echo.New()
	service.registerPostgresRoutes(e)
	t.Run("expired runtime", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, id := range []string{"one", "two"} {
					lease, err := w.Lease(t.Context(), "lease_"+id)
					if err != nil {
						return err
					}
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_" + id, RunID: "run_" + id, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, Sequence: 1, Status: "running", PayloadObjectKey: "example/payload", RuntimeHeartbeatAt: new(now.Add(-90 * time.Second))}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
			defer cancel()
			if _, err := admin.ExecContext(ctx, `DELETE FROM native_attempt_events WHERE organization_id IN ('org_first','org_second')`); err != nil {
				t.Error(err)
			}
			if _, err := admin.ExecContext(ctx, `DELETE FROM native_attempts WHERE organization_id IN ('org_first','org_second')`); err != nil {
				t.Error(err)
			}
			if _, err := admin.ExecContext(ctx, `UPDATE leases SET renewed_at=$1,expires_at=$2,released_at=NULL WHERE organization_id IN ('org_first','org_second')`, asOf, asOf.Add(time.Hour)); err != nil {
				t.Error(err)
			}
		})
		for _, test := range []struct {
			name                                                     string
			age                                                      time.Duration
			missing, released, expired, terminal, unrenewed, unknown bool
			want                                                     bool
		}{
			{name: "fresh", age: 90*time.Second - time.Microsecond},
			{name: "expiry boundary", age: 90 * time.Second, want: true},
			{name: "stale", age: 10 * time.Minute, want: true},
			{name: "no runtime", missing: true},
			{name: "released lease", age: 10 * time.Minute, released: true},
			{name: "expired lease", age: 10 * time.Minute, expired: true},
			{name: "terminal attempt", age: 10 * time.Minute, terminal: true},
			{name: "no renewal after runtime", age: 90 * time.Second, unrenewed: true},
			{name: "unknown legacy summary", age: 10 * time.Minute, unknown: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				heartbeat := now.Add(-test.age)
				var runtime *time.Time
				if !test.missing {
					runtime = &heartbeat
				}
				renewed, expires := now, now.Add(90*time.Second)
				if test.unrenewed {
					renewed, expires = heartbeat, heartbeat.Add(time.Hour)
				}
				if test.expired {
					renewed, expires = now.Add(-90*time.Second), now
				}
				var released *time.Time
				if test.released {
					released = &now
				}
				status := "running"
				if test.terminal {
					status = "succeeded"
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=LEAST(acquired_at,$1),renewed_at=$1,expires_at=$2,released_at=$3 WHERE organization_id IN ('org_first','org_second')`, renewed, expires, released); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runtime_heartbeat_at=$1,runtime_summary_sequence=CASE WHEN $2 THEN NULL ELSE sequence END,status=$3 WHERE organization_id IN ('org_first','org_second')`, runtime, test.unknown, status); err != nil {
					t.Fatal(err)
				}
				for _, org := range []string{"org_first", "org_second"} {
					for _, id := range []string{"one", "two"} {
						req := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/"+org+"/runners/runner_"+id+"/routing", nil)
						req.Header.Set(echo.HeaderAuthorization, "Bearer "+org+"_admin")
						rec := httptest.NewRecorder()
						e.ServeHTTP(rec, req)
						if rec.Code != http.StatusOK {
							t.Fatalf("routing status=%d body=%s", rec.Code, rec.Body.String())
						}
						var got runnerauth.Runner
						if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
							t.Fatal(err)
						}
						if (len(got.Problems) == 1) != test.want || (got.Health == "needs_attention") != test.want {
							t.Fatalf("%s/%s health=%s problems=%+v want runtime warning=%t", org, id, got.Health, got.Problems, test.want)
						}
						if test.want {
							p := got.Problems[0]
							if p.Code != "runtime_expired" || p.Subject != "attempt_"+id || p.ProjectID != "project_"+id || !p.FirstSeen.Equal(heartbeat.Add(90*time.Second)) || !p.ReportedAt.Equal(now) {
								t.Fatalf("runtime problem=%+v", p)
							}
						}
					}
				}
			})
		}
	})
	t.Run("capacity", func(t *testing.T) {
		for _, org := range []string{"org_first", "org_second"} {
			t.Run(org, func(t *testing.T) {
				call := func(method, token, body string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(method, "/api/v2/organizations/"+org+"/runners/runner_one/capacity?backend=example", strings.NewReader(body))
					req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
					req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
					rec := httptest.NewRecorder()
					e.ServeHTTP(rec, req)
					return rec
				}
				read := call(http.MethodGet, org+"_admin", "")
				if read.Code != http.StatusOK {
					t.Fatalf("GET status=%d body=%s", read.Code, read.Body)
				}
				var got runnerCapacityRead
				if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Desired != 3 || got.Recommendation.Partial || len(got.ProviderCapacity) != 1 || got.Projects["project_one"].Used != 1 {
					t.Fatalf("capacity=%+v", got)
				}
				body := fmt.Sprintf(`{"expected_revision":2,"expected_config_revision":%q,"capacity":4,"backend":"example","idempotency_key":"capacity"}`, strings.Repeat("a", 64))
				write := call(http.MethodPut, org+"_admin", body)
				if write.Code != http.StatusOK {
					t.Fatalf("PUT status=%d body=%s", write.Code, write.Body)
				}
				if err := json.Unmarshal(write.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Desired != 4 {
					t.Fatalf("updated capacity=%+v", got)
				}
				replay := call(http.MethodPut, org+"_admin", body)
				if replay.Code != http.StatusOK || replay.Body.String() != write.Body.String() {
					t.Fatalf("replay=%d %s", replay.Code, replay.Body)
				}
				for _, tc := range []struct {
					name, token, body string
					status            int
				}{
					{"changed replay", org + "_admin", strings.Replace(body, `"capacity":4`, `"capacity":5`, 1), http.StatusConflict},
					{"stale runner", org + "_admin", strings.Replace(body, `"idempotency_key":"capacity"`, `"idempotency_key":"stale"`, 1), http.StatusConflict},
					{"stale configuration", org + "_admin", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(body, `"expected_revision":2`, `"expected_revision":3`), strings.Repeat("a", 64), strings.Repeat("b", 64)), `"idempotency_key":"capacity"`, `"idempotency_key":"config"`), http.StatusConflict},
					{"worker", org + "_runner_one", body, http.StatusForbidden},
					{"other organization", map[string]string{"org_first": "org_second_admin", "org_second": "org_first_admin"}[org], body, http.StatusUnauthorized},
				} {
					t.Run(tc.name, func(t *testing.T) {
						rec := call(http.MethodPut, tc.token, tc.body)
						if rec.Code != tc.status {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
						}
					})
				}
				for _, token := range []string{org + "_runner_one", map[string]string{"org_first": "org_second_admin", "org_second": "org_first_admin"}[org]} {
					denied := call(http.MethodGet, token, "")
					if denied.Code != http.StatusForbidden && denied.Code != http.StatusUnauthorized {
						t.Fatalf("GET authority=%d %s", denied.Code, denied.Body)
					}
				}
				freshNow := now
				now = now.Add(runnerauth.HeartbeatTimeout)
				stale := call(http.MethodPut, org+"_admin", strings.ReplaceAll(strings.ReplaceAll(body, `"expected_revision":2`, `"expected_revision":3`), `"idempotency_key":"capacity"`, `"idempotency_key":"expired-evidence"`))
				now = freshNow
				if stale.Code != http.StatusConflict {
					t.Fatalf("expired evidence=%d %s", stale.Code, stale.Body)
				}
				if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
					identity, err := r.RunnerIdentity(t.Context(), "runner_one")
					if err == nil && (identity.Revision != 3 || identity.CapacityRequest == nil || identity.CapacityRequest.Backend != "example" || identity.CapacityLimit != 4) {
						t.Fatalf("persisted=%+v", identity)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET revision=2,capacity_limit=3,capacity_request_revision='',capacity_request_limit=0,capacity_request_backend='' WHERE organization_id=$1 AND runner_id='runner_one'`, org); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
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
					if got.RunnerID != "runner_"+tc.id || got.OrganizationID != tracker.OrganizationID(org) || got.Hostname != org+"-"+tc.id || got.CapacityLimit != 3 || got.HostCapacity != 5 || got.HostRevision != map[string]int64{"one": 7, "two": 9}[tc.id] || got.HostUsed != 1 || got.Used != 1 || got.ProjectUsed[project] != 1 || got.ProjectRanks[project] != 7 || got.ProjectCapacityLimits[project] != 2 || got.CapacityConfig == nil {
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
			var pageMu sync.Mutex
			var pages []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pageMu.Lock()
				pages = append(pages, r.URL.RawQuery)
				pageMu.Unlock()
				hosted.ServeHTTP(w, r)
			}))
			defer server.Close()
			client, err := hubapi.New(hubapi.Config{URL: server.URL, TokenSource: func() string { return org + "_list" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := hubapi.NewFleetClient(client, tracker.OrganizationID(org), nil)
			if err != nil {
				t.Fatal(err)
			}
			fleet, err := adapter.Fleet(t.Context())
			if err != nil || len(fleet.Runners) != 51 || !fleet.Editable {
				t.Fatalf("consumer fleet: count=%d editable=%v error=%v", len(fleet.Runners), fleet.Editable, err)
			}
			pageMu.Lock()
			observedPages := slices.Clone(pages)
			pageMu.Unlock()
			if !slices.Equal(observedPages, []string{"limit=50&offset=0", "limit=50&offset=50"}) {
				t.Fatalf("consumer pages=%v", observedPages)
			}
			seen := map[string]bool{}
			for _, runner := range fleet.Runners {
				if seen[runner.RunnerID] || runner.OrganizationID != tracker.OrganizationID(org) {
					t.Fatalf("duplicate or cross-organization runner: %+v", runner)
				}
				seen[runner.RunnerID] = true
				wantRevision := int64(7)
				if runner.MachineID == "machine_two" {
					wantRevision = 9
				}
				if runner.HostRevision != wantRevision {
					t.Fatalf("consumer host revision=%d want=%d", runner.HostRevision, wantRevision)
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

			t.Run("receipt and proposed access authority", func(t *testing.T) {
				initial := read("runner_one")
				var saved runnerstore.Identity
				var creator string
				if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					saved, err = r.RunnerIdentity(t.Context(), "runner_one")
					if err != nil {
						return err
					}
					enrollment, err := r.RunnerEnrollment(t.Context(), saved.EnrollmentID)
					creator = enrollment.CreatedBy
					return err
				}); err != nil {
					t.Fatal(err)
				}
				saved.ProjectIDs = slices.Clone(initial.ProjectIDs)
				membership.Role.Slug = "member"
				provider.members[membership.ID] = membership
				if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE runner_enrollments SET created_by='credential_member' WHERE organization_id='org_first' AND id=$1", saved.EnrollmentID); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_first", TokenID: "credential_member", ProjectID: "project_two"}); err != nil {
						return err
					}
					return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_first", UserID: "user_example", ProjectID: "project_two", CanWrite: true, ManageRunner: true})
				}); err != nil {
					t.Fatal(err)
				}
				defer func() {
					membership.Role.Slug = "admin"
					provider.members[membership.ID] = membership
					if _, err := admin.ExecContext(parentContext, "UPDATE hosted_members SET role='admin' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
						t.Error(err)
					}
					if _, err := admin.ExecContext(parentContext, "UPDATE runner_enrollments SET created_by=$2 WHERE organization_id='org_first' AND id=$1", saved.EnrollmentID, creator); err != nil {
						t.Error(err)
					}
					if err := repository.WriteTenant(parentContext, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
						current, err := w.RunnerIdentity(parentContext, "runner_one")
						if err != nil {
							return err
						}
						saved.Revision = current.Revision
						if err := w.UpdateRunnerRouting(parentContext, current.Revision, saved); err != nil {
							return err
						}
						if err := w.DeleteAccessTokenGrant(parentContext, "runner_one", "project_two"); err != nil {
							return err
						}
						if err := w.SetAccessTokenGrant(parentContext, accessstore.TokenGrant{OrganizationID: "org_first", TokenID: "runner_one", ProjectID: "project_one"}); err != nil {
							return err
						}
						if err := w.DeleteAccessTokenGrant(parentContext, "credential_member", "project_two"); err != nil {
							return err
						}
						return w.DeleteAccessProjectGrant(parentContext, "user_example", "project_two")
					}); err != nil {
						t.Error(err)
					}
				}()
				t.Run("narrowed runner cannot disclose old B receipt", func(t *testing.T) {
					current := read("runner_one")
					original := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "receipt_AB"}
					original.ProjectIDs, original.ProjectRanks = []tracker.ProjectID{"project_one", "project_two"}, nil
					accepted := put("example_member_token", original)
					if accepted.Code != http.StatusOK {
						t.Fatalf("AB status=%d body=%s", accepted.Code, accepted.Body.String())
					}
					current = read("runner_one")
					narrow := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "receipt_A"}
					narrow.ProjectIDs, narrow.ProjectRanks = []tracker.ProjectID{"project_one"}, nil
					if rec := put("example_member_token", narrow); rec.Code != http.StatusOK {
						t.Fatalf("narrow status=%d body=%s", rec.Code, rec.Body.String())
					}
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner=false WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_two'"); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := admin.ExecContext(parentContext, "UPDATE hosted_project_grants SET manage_runner=true WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_two'"); err != nil {
							t.Error(err)
						}
					}()
					counter := &routingReplayRepository{Repository: repository}
					details := &routingReplayDetails{DetailStore: service.runDetailStore()}
					priorRepository, priorDetails := service.config.TenantRepository, service.config.RunDetails
					service.config.TenantRepository, service.config.RunDetails = counter, details
					defer func() { service.config.TenantRepository, service.config.RunDetails = priorRepository, priorDetails }()
					before := read("runner_one")
					rec := put("example_member_token", original)
					if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
						t.Errorf("old receipt disclosed after B revocation: status=%d body=%s", rec.Code, rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), "project_two") || strings.Contains(rec.Body.String(), "runner_id") || rec.Body.String() == accepted.Body.String() {
						t.Error("denied replay disclosed receipt payload")
					}
					if counter.writes != 0 || details.writes != 0 {
						t.Errorf("denied replay wrote state: transactions=%d objects=%d", counter.writes, details.writes)
					}
					if after := read("runner_one"); !reflect.DeepEqual(before, after) {
						t.Error("denied replay changed routing")
					}
				})
				t.Run("organization grant revoked during receipt storage", func(t *testing.T) {
					before := read("runner_one")
					input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}, IdempotencyKey: "org_revoke_at_store"}
					input.Scope, input.ProjectIDs, input.ProjectRanks = "organization", nil, nil
					hook := &routingGrantInterleaveDetails{DetailStore: service.runDetailStore(), afterStore: func(ctx context.Context) error {
						_, err := admin.ExecContext(ctx, "UPDATE hosted_project_grants SET manage_runner=false WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_two'")
						return err
					}}
					prior := service.config.RunDetails
					service.config.RunDetails = hook
					defer func() {
						service.config.RunDetails = prior
						if _, err := admin.ExecContext(parentContext, "UPDATE hosted_project_grants SET manage_runner=true WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_two'"); err != nil {
							t.Error(err)
						}
					}()
					rec := put("example_member_token", input)
					if !hook.fired {
						t.Fatal("grant revocation was not interleaved with receipt storage")
					}
					if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
						t.Errorf("revoked organization proposal committed: status=%d body=%s", rec.Code, rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), "runner_id") {
						t.Error("revoked proposal disclosed receipt")
					}
					if after := read("runner_one"); !reflect.DeepEqual(before, after) {
						t.Error("revoked proposal changed routing")
					}
					if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.Receipt(t.Context(), "credential_member", "runner_routing runner_one", input.IdempotencyKey)
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Errorf("revoked proposal persisted receipt: %v", err)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
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
				original := put("example_member_token", input)
				if original.Code != http.StatusOK {
					t.Fatalf("rename status=%d body=%s", original.Code, original.Body.String())
				}
				got := read("runner_one")
				if got.DisplayName != input.DisplayName || got.Revision != current.Revision+1 || !reflect.DeepEqual(got.CapacityRequest, current.CapacityRequest) {
					t.Fatal("rename lost routing or capacity request")
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=17 WHERE organization_id='org_first' AND id='unrelated_0'"); err != nil {
					t.Fatal(err)
				}
				counter := &routingReplayRepository{Repository: repository}
				objects := &routingReplayDetails{DetailStore: service.runDetailStore()}
				priorRepository, priorDetails := service.config.TenantRepository, service.config.RunDetails
				service.config.TenantRepository, service.config.RunDetails = counter, objects
				defer func() { service.config.TenantRepository, service.config.RunDetails = priorRepository, priorDetails }()
				beforeReplay := read("runner_one")
				for _, test := range []struct {
					name   string
					alter  bool
					status int
				}{
					{"identical replay after default changes", false, http.StatusOK},
					{"same key different hash", true, http.StatusConflict},
				} {
					t.Run(test.name, func(t *testing.T) {
						request := input
						if test.alter {
							request.DisplayName = "Different name"
						}
						rec := put("example_member_token", request)
						if rec.Code != test.status {
							t.Fatalf("replay status=%d want=%d body=%s", rec.Code, test.status, rec.Body.String())
						}
						if !test.alter && rec.Body.String() != original.Body.String() {
							t.Fatal("default change replaced original receipt")
						}
					})
				}
				t.Run("replay current grant denied", func(t *testing.T) {
					membership.Role.Slug = "member"
					provider.members[membership.ID] = membership
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner=false WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_one'"); err != nil {
						t.Fatal(err)
					}
					defer func() {
						membership.Role.Slug = "admin"
						provider.members[membership.ID] = membership
						if _, err := admin.ExecContext(parentContext, "UPDATE hosted_members SET role='admin' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
							t.Error(err)
						}
						if _, err := admin.ExecContext(parentContext, "UPDATE hosted_project_grants SET manage_runner=true WHERE organization_id='org_first' AND user_id='user_example' AND project_id='project_one'"); err != nil {
							t.Error(err)
						}
					}()
					rec := put("example_member_token", input)
					if rec.Code != http.StatusNotFound {
						t.Fatalf("revoked replay status=%d body=%s", rec.Code, rec.Body.String())
					}
					if rec.Body.String() == original.Body.String() || strings.Contains(rec.Body.String(), "runner_id") {
						t.Fatal("revoked replay disclosed receipt")
					}
				})
				if counter.writes != 0 || objects.writes != 0 {
					t.Fatalf("replay wrote state: transactions=%d objects=%d", counter.writes, objects.writes)
				}
				if after := read("runner_one"); !reflect.DeepEqual(beforeReplay, after) {
					t.Fatal("replay changed current routing")
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
		for _, test := range []struct{ name, query string }{
			{"rank override", `INSERT INTO runner_project_ranks (organization_id,runner_id,project_id,rank) VALUES ('org_first','runner_one','project_two',9)`},
			{"capacity override", `INSERT INTO runner_project_capacity_limits (organization_id,runner_id,project_id,capacity_limit) VALUES ('org_first','runner_one','project_two',1)`},
			{"project grant", `INSERT INTO token_grants (organization_id,token_id,project_id) VALUES ('org_first','runner_one','project_two')`},
		} {
			t.Run("hidden "+test.name, func(t *testing.T) {
				if _, err := admin.ExecContext(t.Context(), test.query); err != nil {
					t.Fatal(err)
				}
				defer func() {
					for _, table := range []string{"runner_project_ranks", "runner_project_capacity_limits", "token_grants"} {
						column := "runner_id"
						if table == "token_grants" {
							column = "token_id"
						}
						if _, err := admin.ExecContext(parentContext, "DELETE FROM "+table+" WHERE organization_id='org_first' AND "+column+"='runner_one' AND project_id='project_two'"); err != nil {
							t.Fatal(err)
						}
					}
				}()
				rec := request("org_first", "runner_one", "example_member_token")
				if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "project_two") {
					t.Fatalf("hidden routing project leaked: status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}

		t.Run("proposed organization access requires current grants", func(t *testing.T) {
			membership.Role.Slug = "admin"
			provider.members[membership.ID] = membership
			if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET role='admin' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				membership.Role.Slug = "member"
				provider.members[membership.ID] = membership
				if _, err := admin.ExecContext(parentContext, "UPDATE hosted_members SET role='member' WHERE organization_id='org_first' AND user_id='user_example'"); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(parentContext, "UPDATE runner_identities SET scope='projects' WHERE organization_id='org_first' AND runner_id='runner_one'"); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(parentContext, "INSERT INTO token_grants(organization_id,token_id,project_id) VALUES('org_first','runner_one','project_one') ON CONFLICT DO NOTHING"); err != nil {
					t.Error(err)
				}
			}()
			hostedConfig := service.config.Hosted
			service.config.Hosted = nil
			response := request("org_first", "runner_one", "org_first_admin")
			service.config.Hosted = hostedConfig
			if response.Code != http.StatusOK {
				t.Fatalf("routing read status=%d", response.Code)
			}
			var current runnerauth.Runner
			decodeHubResponse(t, response, &current)
			input := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, IdempotencyKey: "scope_requires_all_grants"}
			input.Scope, input.ProjectIDs, input.ProjectRanks = "organization", nil, nil
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/v2/organizations/org_first/runners/runner_one/routing", strings.NewReader(string(raw)))
			req.Header.Set(echo.HeaderAuthorization, "Bearer example_member_token")
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "unrelated_0") {
				t.Fatalf("proposed organization scope leaked ranks: status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
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

func TestPostgresRunnerHostChangeWithoutLegacyDatabase(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin", Name: "Example admin", Hash: apikey.HashToken(string(org)), Scope: "admin", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ProjectAccess: "all", Kind: "service"}); err != nil {
				return err
			}
			for _, id := range []string{"machine_one", "machine_two"} {
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: "example", DisplayName: "Original", Capacity: 5, Version: "example"}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			scope := nativeScope{organization: org, credential: apiCredential{ID: "admin", Hash: apikey.HashToken(string(org)), Scope: apiScopeAdmin}}
			for _, tc := range []struct {
				name, machine, token string
				revision             int64
				capacity             int
				wantError            bool
			}{
				{"first", "machine_one", string(org), 1, 7, false},
				{"stale", "machine_one", string(org), 1, 9, true},
				{"second", "machine_two", string(org), 1, 7, false},
				{"foreign credential", "machine_one", "foreign", 2, 9, true},
				{"missing", "missing", string(org), 1, 9, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					scope.credential.Hash = apikey.HashToken(tc.token)
					value, err := service.updateRunnerHostCommand(t.Context(), scope, tc.machine, runnerauth.HostChange{ExpectedRevision: tc.revision, DisplayName: " Updated ", Capacity: tc.capacity})
					if (err != nil) != tc.wantError {
						t.Fatalf("value=%+v error=%v", value, err)
					}
					if !tc.wantError {
						got := value.(runnerauth.HostChange)
						if got.ExpectedRevision != 2 || got.DisplayName != "Updated" || got.Capacity != 7 {
							t.Fatalf("change=%+v", got)
						}
					}
				})
			}
			for _, machine := range []string{"machine_one", "machine_two"} {
				var name string
				var capacity, revision int64
				if err := admin.QueryRowContext(t.Context(), "SELECT display_name,capacity,routing_revision FROM machines WHERE organization_id=$1 AND id=$2", org, machine).Scan(&name, &capacity, &revision); err != nil {
					t.Fatal(err)
				}
				if name != "Updated" || capacity != 7 || revision != 2 {
					t.Fatalf("machine=%s name=%s capacity=%d revision=%d", machine, name, capacity, revision)
				}
			}
		})
	}
}

type routingReplayRepository struct {
	*postgres.Repository
	writes int
}

func (r *routingReplayRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, work corestore.Workload, write func(tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteTenant(ctx, org, work, write)
}

type routingReplayDetails struct {
	attachment.DetailStore
	writes int
}

func (s *routingReplayDetails) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	s.writes++
	return s.DetailStore.StoreDetail(ctx, org, raw)
}

type routingGrantInterleaveDetails struct {
	attachment.DetailStore
	afterStore func(context.Context) error
	fired      bool
}

func (s *routingGrantInterleaveDetails) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := s.DetailStore.StoreDetail(ctx, org, raw)
	if err != nil {
		return ref, err
	}
	var value runnerauth.Runner
	if !s.fired && json.Unmarshal(raw, &value) == nil && value.RunnerID == "runner_one" && value.Scope == "organization" {
		s.fired = true
		err = s.afterStore(ctx)
	}
	return ref, err
}
