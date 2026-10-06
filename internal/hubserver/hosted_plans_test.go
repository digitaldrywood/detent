package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const testHostedPlanAdminToken = "plan-admin-token-for-isolated-tests-2195"

func TestHostedProjectRetryAfterDowngrade(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		projects int64
		features bool
	}{
		{"at project limit", 3, true},
		{"over project limit", 0, true},
		{"collaboration disabled", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowserHostedFixture(t, true)
			config := hostedTestPlans(t, f.service, map[string]int64{"projects": 3})
			project := f.createProject(t, "Resumable allowance project")
			plan := config.Plans[0]
			plan.Version++
			plan.Allowances = maps.Clone(plan.Allowances)
			plan.Allowances["projects"] = test.projects
			if !test.features {
				plan.Features = nil
			}
			config.Plans = append(config.Plans, plan)
			cfg := *f.service.config.Hosted
			cfg.Plans = &config
			if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "downgrade", Action: "base", ExpectedRevision: 1, Plan: plan.PlanReference, Reason: "pilot downgrade"}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			before, err := f.service.database.hostedPlanUsage(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() {
					response := f.form(t, "owner", "/projects", url.Values{"name": {"Resumable allowance project"}, "grant_access": {"true"}})
					if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/projects/"+project {
						t.Errorf("project retry = %d %s", response.Code, response.Body.String())
					}
				})
			}
			group.Wait()
			requireNativeStatus(t, f.form(t, "owner", "/projects", url.Values{"name": {"Excess project"}, "grant_access": {"true"}}), http.StatusTooManyRequests)
			after, err := f.service.database.hostedPlanUsage(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			for resource := range before.Allowances {
				if before.Usage[resource] != after.Usage[resource] {
					t.Errorf("retry or rejection changed %s usage: before=%d after=%d", resource, before.Usage[resource], after.Usage[resource])
				}
			}
			for _, table := range []string{"hosted_project_grants", "token_grants", "workflow_states"} {
				var orphans int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id NOT IN (SELECT id FROM projects)").Scan(&orphans); err != nil || orphans != 0 {
					t.Fatalf("rejected allocation left %s records: %d %v", table, orphans, err)
				}
			}
			for _, path := range []string{"/projects/" + project, "/api/v2/organizations/org_browser_preview/projects/" + project + "/onboarding", "/organization/plan", "/api/cloud/billing"} {
				requireNativeStatus(t, f.page(t, "owner", path), http.StatusOK)
			}
		})
	}
}

func hostedTestPlans(t *testing.T, service *Service, limits map[string]int64) HostedPlansConfig {
	t.Helper()
	service.config.Hosted.EntitlementAdminToken = []byte(testHostedPlanAdminToken)
	service.config.Hosted.EntitlementAdministrator = "test-operator"
	config := pilotHostedPlans()
	free := config.Plans[0]
	free.ID = "test_free"
	maps.Copy(free.Allowances, limits)
	paid := free
	paid.ID = "test_paid"
	paid.Allowances = maps.Clone(free.Allowances)
	paid.Allowances["projects"] = 20
	paid.Allowances["concurrent_work"] = 20
	paid.Features = append(slices.Clone(free.Features), "hosted_artifacts")
	config.Plans = []HostedPlan{free, paid}
	config.Base = free.PlanReference
	cfg := *service.config.Hosted
	cfg.Plans = &config
	if err := service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id = ?,base_version = ?", free.ID, free.Version); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestHostedPlanResolutionBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		subscription bool
		grant        bool
		revoke       bool
		at           time.Duration
		want         string
		projects     int64
		feature      bool
		modelChoice  bool
		modelEnabled bool
	}{
		{"free without payment", false, false, false, 0, "base", 2, false, false, false},
		{"paid base", true, false, false, 0, "subscription", 20, true, false, false},
		{"scoped grant", false, true, false, 0, "base", 20, false, false, false},
		{"before grant expiry", false, true, false, time.Minute - time.Nanosecond, "base", 20, false, false, false},
		{"exact grant expiry", false, true, false, time.Minute, "base", 2, false, false, false},
		{"expiry returns to paid", true, true, false, time.Minute, "subscription", 20, true, false, false},
		{"subscription deadline", true, false, false, 2 * time.Minute, "base", 2, false, false, false},
		{"revocation", false, true, true, 0, "base", 2, false, false, false},
		{"model choice only", false, true, false, 0, "base", 2, false, true, true},
		{"model choice on paid", true, true, false, 0, "subscription", 20, true, true, true},
		{"model choice before expiry", false, true, false, time.Minute - time.Nanosecond, "base", 2, false, true, true},
		{"model choice at expiry", false, true, false, time.Minute, "base", 2, false, true, false},
		{"model choice revoked on paid", true, true, true, 0, "subscription", 20, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			config := hostedTestPlans(t, f.service, map[string]int64{"projects": 2})
			f.service.database.now = func() time.Time { return now }
			revision := int64(1)
			apply := func(command hostedPlanCommand) {
				t.Helper()
				command.ExpectedRevision = revision
				command.Reason = "pilot testing"
				if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, command); err != nil {
					t.Fatal(err)
				}
				revision++
			}
			if test.subscription {
				until := now.Add(2 * time.Minute)
				apply(hostedPlanCommand{ID: "paid", Action: "subscription", Plan: config.Plans[1].PlanReference, ExpiresAt: &until})
			}
			if test.grant {
				until := now.Add(time.Minute)
				plan, scope := config.Plans[1].PlanReference, []string{"projects"}
				if test.modelChoice {
					plan, scope = config.Base, []string{"model_choice"}
				}
				apply(hostedPlanCommand{ID: "grant", Action: "grant", GrantID: "pilot_grant", Plan: plan, Scope: scope, ExpiresAt: &until})
			}
			if test.revoke {
				apply(hostedPlanCommand{ID: "revoke", Action: "revoke", GrantID: "pilot_grant"})
			}
			got, err := f.service.database.hostedPlanUsage(t.Context(), now.Add(test.at))
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != test.want || got.Allowances["projects"] != test.projects || slices.Contains(got.Features, "hosted_artifacts") != test.feature || slices.Contains(got.Features, "model_choice") != test.modelEnabled {
				t.Fatalf("unexpected entitlement: %+v", got)
			}
			if got.Usage["projects"] != 1 {
				t.Fatal("plan change deleted existing project")
			}
			f.service.config.now = func() time.Time { return now.Add(test.at) }
			fleet, err := f.service.hostedFleetUsage(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if fleet.WindowEndsAt != got.WindowEndsAt.UTC().Format(time.RFC3339) || len(fleet.Allowances) != len(got.Allowances) {
				t.Fatalf("Fleet projection window or allowances differ: %#v", fleet)
			}
			for name, limit := range got.Allowances {
				if fleet.Allowances[name] != (hostedFleetAllowance{Used: got.Usage[name], Limit: limit}) {
					t.Fatalf("Fleet %s=%+v full usage=%d limit=%d", name, fleet.Allowances[name], got.Usage[name], limit)
				}
			}
		})
	}
}

func TestHostedPlanCommandsAndPermissions(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, nil)
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	member := f.user(t, "member", "member", "member@example.test", "", "")
	command := hostedPlanCommand{ID: "grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp", Plan: config.Plans[1].PlanReference, Scope: []string{"projects"}, Reason: "pilot approval"}
	path := "/api/v2/organizations/org_security/entitlements"
	for _, test := range []struct {
		name, path string
		user       hostedSecurityUser
		want       int
	}{
		{"owner cannot self grant", path, owner, http.StatusForbidden},
		{"member cannot self grant", path, member, http.StatusForbidden},
		{"foreign organization", strings.Replace(path, "org_security", "org_foreign", 1), owner, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, test.user, http.MethodPost, test.path, command), test.want)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, command), http.StatusForbidden)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, command), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, command), http.StatusNoContent)
	command.Scope = []string{"concurrent_work"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, command), http.StatusConflict)
	var audits int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_audit").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("duplicate command produced %d audits", audits)
	}
	for _, test := range []struct {
		name string
		user hostedSecurityUser
		want int
	}{
		{"owner sees billing", owner, http.StatusOK}, {"member cannot see billing", member, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, test.user, http.MethodGet, "/api/cloud/billing", nil), test.want)
		})
	}
	report := performHubAPIRequest(t, f.service, http.MethodGet, "/api/cloud/metadata", testHubAdminToken, nil)
	requireNativeStatus(t, report, http.StatusOK)
	for _, forbidden := range []string{"pilot approval", "private-project-sentinel", "owner@example.test", "record_json", "request_hash"} {
		if strings.Contains(report.Body.String(), forbidden) {
			t.Fatalf("metadata leaked %q", forbidden)
		}
	}
}

func TestHostedPlanReportRequiresEntitlementAdministrator(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, nil)
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	path := "/api/v2/organizations/org_security/entitlements"
	until := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	grant := hostedPlanCommand{ID: "report-grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp_report", Plan: config.Plans[1].PlanReference, Scope: []string{"projects", "hosted_artifacts"}, ExpiresAt: &until, Reason: "design partner"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
	for _, test := range []struct {
		name     string
		response func() int
		want     int
	}{
		{"owner session", func() int { return f.request(t, owner, http.MethodGet, path, nil).Code }, http.StatusForbidden},
		{"hub admin token", func() int {
			return performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil).Code
		}, http.StatusForbidden},
		{"entitlement administrator", func() int {
			return performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil).Code
		}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.response(); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
	var report hostedEntitlementReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Revision != 2 || report.Base != config.Base || report.EffectiveBase != config.Base || len(report.Plans) != 2 || len(report.Grants) != 1 {
		t.Fatalf("report = %+v", report)
	}
	recorded := report.Grants[0]
	if recorded.ID != "comp_report" || recorded.Reason != "design partner" || recorded.GrantedBy != "test-operator" || recorded.GrantedAt == nil || recorded.ExpiresAt == nil || !recorded.ExpiresAt.Equal(until) || recorded.Plan != config.Plans[1].PlanReference {
		t.Fatalf("grant = %+v", recorded)
	}
}

func TestHostedConcurrentClaimsDowngradeRelease(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, map[string]int64{"concurrent_work": 1})
	now := time.Now().UTC()
	clock := &leaseTestClock{value: now}
	f.service.database.now = clock.Now
	const contenders = 8
	requests := make([]tracker.ClaimRequest, contenders)
	for i := range contenders {
		item := f.seedIssue(t, i+1)
		var id tracker.WorkItemID
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", item).Scan(&id); err != nil {
			t.Fatal(err)
		}
		machine := tracker.MachineID(fmt.Sprintf("machine-%d", i))
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) VALUES(?,?,?,1,'test',?,?,?,'org_security','bootstrap-admin')`, machine, machine, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		requests[i] = tracker.ClaimRequest{WorkItemID: id, MachineID: machine, SessionID: fmt.Sprintf("session-%d", i), TTL: time.Minute}
	}
	type outcome struct {
		lease tracker.Lease
		err   error
		index int
	}
	start := make(chan struct{})
	results := make(chan outcome, contenders)
	var group sync.WaitGroup
	for i, request := range requests {
		group.Go(func() {
			<-start
			lease, err := f.service.Tracker().Claim(t.Context(), request)
			results <- outcome{lease, err, i}
		})
	}
	close(start)
	group.Wait()
	close(results)
	var winner outcome
	var leases []tracker.Lease
	winners := 0
	for result := range results {
		if result.err == nil {
			winner = result
			leases = append(leases, result.lease)
			winners++
			continue
		}
		var limit *hostedLimitError
		if !errors.As(result.err, &limit) || limit.Resource != "concurrent_work" {
			t.Errorf("claim error: %v", result.err)
		}
	}
	if winners != contenders {
		t.Fatalf("got %d winners", winners)
	}
	again, err := f.service.Tracker().Claim(t.Context(), requests[winner.index])
	if err != nil || again.ID != winner.lease.ID {
		t.Fatalf("retry = %v %v", again, err)
	}
	zero := config.Plans[0]
	zero.Version = 2
	zero.Allowances = maps.Clone(zero.Allowances)
	zero.Allowances["concurrent_work"] = 0
	config.Plans = append(config.Plans, zero)
	cfg := *f.service.config.Hosted
	cfg.Plans = &config
	if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "downgrade", Action: "base", ExpectedRevision: 1, Plan: zero.PlanReference, Reason: "pilot downgrade"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Tracker().Renew(t.Context(), tracker.RenewRequest{LeaseID: winner.lease.ID, FencingToken: winner.lease.FencingToken, TTL: time.Minute}); err != nil {
		t.Fatalf("downgrade prevented safe renewal: %v", err)
	}
	for _, lease := range leases {
		if lease.ID != winner.lease.ID {
			if err := f.service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: lease.ID, FencingToken: lease.FencingToken, Reason: "work_item_hydration_failed"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 2 {
		err := f.service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: winner.lease.ID, FencingToken: winner.lease.FencingToken, Reason: "work_item_hydration_failed"})
		if i == 0 && err != nil || i == 1 && !errors.Is(err, tracker.ErrStaleFencingToken) {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	usage, err := f.service.database.hostedPlanUsage(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["concurrent_work"] != 0 {
		t.Fatal("failed start retained capacity")
	}
	if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "restore", Action: "base", ExpectedRevision: 2, Plan: config.Base, Reason: "pilot restore"}); err != nil {
		t.Fatal(err)
	}
	next := requests[(winner.index+1)%contenders]
	next.SessionID = "restored-session"
	lease, err := f.service.Tracker().Claim(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	next = requests[(winner.index+2)%contenders]
	next.SessionID = "expired-session"
	if _, err := f.service.Tracker().Claim(t.Context(), next); err != nil {
		t.Fatalf("exact expiry retained capacity: %v", err)
	}
	if err := f.service.Tracker().Release(t.Context(), tracker.ReleaseRequest{LeaseID: lease.ID, FencingToken: lease.FencingToken}); !errors.Is(err, tracker.ErrStaleFencingToken) {
		t.Fatalf("stale release=%v", err)
	}
}

func TestHostedMutationQuotaRollbackAndRetry(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		limits map[string]int64
		want   int
	}{
		{"event exhaustion", map[string]int64{"ingested_events": 0}, http.StatusTooManyRequests},
		{"storage exhaustion", map[string]int64{"collaboration_bytes": 1}, http.StatusTooManyRequests},
		{"history exhaustion", map[string]int64{"history_records": 0}, http.StatusTooManyRequests},
		{"allowed", nil, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			hostedTestPlans(t, f.service, test.limits)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			request := map[string]any{"idempotency_key": "create", "title": "private-title", "body": "private-body", "state": "Todo"}
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", request)
			requireNativeStatus(t, response, test.want)
			var events, commands int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_events),(SELECT count(*) FROM native_commands)").Scan(&events, &commands); err != nil {
				t.Fatal(err)
			}
			if test.want == http.StatusTooManyRequests {
				if events != 0 || commands != 0 {
					t.Fatal("rejected mutation persisted data or idempotency receipt")
				}
				return
			}
			before, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", request), http.StatusOK)
			after, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range []string{"http_requests", "http_duration_microseconds", "http_response_bytes", "http_request_bytes", "http_request_bytes_known"} {
				delete(before.Usage, metric)
				delete(after.Usage, metric)
			}
			if !maps.Equal(before.Usage, after.Usage) {
				t.Fatalf("retry counted twice: %v -> %v", before.Usage, after.Usage)
			}
		})
	}
}

func TestHostedPlanConfigurationAndTelemetry(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*HostedPlansConfig)
	}{
		{"missing base", func(c *HostedPlansConfig) { c.Base.ID = "absent" }},
		{"negative allowance", func(c *HostedPlansConfig) { c.Plans[0].Allowances["projects"] = -1 }},
		{"unknown allowance", func(c *HostedPlansConfig) { c.Plans[0].Allowances["unlimited"] = 1 }},
		{"unbounded retention", func(c *HostedPlansConfig) { c.RetentionWindows = 721 }},
		{"unknown feature", func(c *HostedPlansConfig) { c.Plans[0].Features = []string{"admin_bypass"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := pilotHostedPlans()
			test.change(&c)
			if c.validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, nil)
	config.Plans[0].Allowances["projects"]++
	cfg := *f.service.config.Hosted
	cfg.Plans = &config
	if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err == nil {
		t.Fatal("mutated existing plan version")
	}
	now := time.Now().UTC()
	for i := range 30 {
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.recordHostedUsage(t.Context(), tx, now.Add(time.Duration(i)*time.Hour), "heartbeats", 1); err != nil {
			t.Fatal(errors.Join(err, tx.Rollback()))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_usage_windows WHERE metric='heartbeats'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 24 {
		t.Fatalf("retained %d windows", count)
	}
}

func TestHostedPlansDisabledWithoutNetwork(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "local.db")})
	if service.database.hostedPlans != nil {
		t.Fatal("self-hosted plan restrictions enabled")
	}
	tx, err := service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, check := range []func(context.Context) error{
		func(ctx context.Context) error { return service.database.checkHostedClaim(ctx, tx, time.Now()) },
		func(ctx context.Context) error {
			return service.database.checkHostedGrowth(ctx, tx, nil, time.Now(), false)
		},
		func(ctx context.Context) error {
			return service.database.requireHostedFeature(ctx, tx, "hosted_artifacts", time.Now())
		},
	} {
		if err := check(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_assignments").Scan(&count); err != nil || count != 0 {
		t.Fatalf("local assignments %d: %v", count, err)
	}
}

func TestHostedConcurrentProjectsAndInvitationSeats(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	hostedTestPlans(t, f.service, map[string]int64{"projects": 3, "members": 3})
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	for _, kind := range []string{"project", "invitation"} {
		t.Run(kind, func(t *testing.T) {
			start := make(chan struct{})
			results := make(chan int, 8)
			var group sync.WaitGroup
			for i := range 8 {
				group.Go(func() {
					<-start
					if kind == "invitation" {
						err := f.service.reserveHostedInvitation(t.Context(), fmt.Sprintf("pilot%d@example.test", i))
						if err == nil {
							results <- http.StatusOK
							return
						}
						var limit *hostedLimitError
						if !errors.As(err, &limit) {
							t.Errorf("invitation reservation: %v", err)
						}
						results <- http.StatusTooManyRequests
						return
					}
					response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{
						"idempotency_key": fmt.Sprintf("project%d", i), "name": fmt.Sprintf("project%d", i), "grant_access": true,
					})
					results <- response.Code
				})
			}
			close(start)
			group.Wait()
			close(results)
			winners := 0
			for status := range results {
				if status == http.StatusOK || status == http.StatusCreated {
					winners++
				} else if status != http.StatusTooManyRequests {
					t.Errorf("allocation status = %d", status)
				}
			}
			want := 2
			if kind == "invitation" {
				want = 8
			}
			if winners != want {
				t.Fatalf("%s allocations = %d, want %d", kind, winners, want)
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_member_reservations SET expires_at = ?", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reserveHostedInvitation(t.Context(), "replacement@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.releaseHostedInvitation(t.Context(), "replacement@example.test"); err != nil {
		t.Fatal(err)
	}
	usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil || usage.Usage["members"] != 1 {
		t.Fatalf("expired/failed reservations retained seats: %+v %v", usage.Usage, err)
	}
}

func TestHostedRunnerAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, metric string
		limit        int64
		want         int
	}{
		{"available registration", "registered_runners", 1, http.StatusCreated},
		{"registration exhausted", "registered_runners", 0, http.StatusTooManyRequests},
		{"connections exhausted", "connected_runners", 0, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			hostedTestPlans(t, f.service, map[string]int64{test.metric: test.limit})
			owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
			f.grant(t, owner, true, true)
			binding := runnerauth.NewBinding()
			path := "/api/v2/organizations/org_security/runner-enrollments"
			request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60}
			response := f.request(t, owner, http.MethodPost, path, request)
			requireNativeStatus(t, response, http.StatusCreated)
			var enrollment runnerauth.Enrollment
			decodeHubResponse(t, response, &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "pilot-host", DisplayName: "Pilot runner", Capacity: 1, Version: "test"}
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/redeem", enrollment.Token, redemption)
			requireNativeStatus(t, response, test.want)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_identities").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.want == http.StatusCreated && count != 1 || test.want == http.StatusTooManyRequests && count != 0 {
				t.Fatalf("registered runners = %d after status %d", count, test.want)
			}
			if test.want == http.StatusTooManyRequests && !strings.Contains(response.Body.String(), test.metric) {
				t.Fatalf("missing contextual limit: %s", response.Body.String())
			}
		})
	}
}

func TestHostedPlanPages(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	hostedTestPlans(t, f.service, map[string]int64{"projects": 0})
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "", "")
	for _, test := range []struct {
		name string
		user hostedSecurityUser
		want int
	}{
		{"owner sees downgrade", owner, http.StatusOK},
		{"viewer cannot inspect organization allowances", viewer, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, test.user, http.MethodGet, "/organization/plan", nil)
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK {
				for _, expected := range []string{"Above allowance", "Maximum:", "separate from charges by your model providers", "Billing usage export"} {
					if !strings.Contains(response.Body.String(), expected) {
						t.Errorf("plan page omitted %q", expected)
					}
				}
			}
		})
	}
}

func TestHostedPlanReportListsOnlyActiveGrants(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		after func(t *testing.T, f *hostedSecurityFixture, path string, clock *leaseTestClock)
	}{
		{name: "revoked", after: func(t *testing.T, f *hostedSecurityFixture, path string, _ *leaseTestClock) {
			revoke := hostedPlanCommand{ID: "report-revoke", Action: "revoke", ExpectedRevision: 2, GrantID: "comp_report", Reason: "partnership ended"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, revoke), http.StatusNoContent)
		}},
		{name: "expired", after: func(_ *testing.T, _ *hostedSecurityFixture, _ string, clock *leaseTestClock) {
			clock.value = clock.value.Add(2 * time.Hour)
		}},
		{name: "not started", after: func(_ *testing.T, _ *hostedSecurityFixture, _ string, clock *leaseTestClock) {
			clock.value = clock.value.Add(-time.Hour)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			config := hostedTestPlans(t, f.service, nil)
			clock := &leaseTestClock{value: time.Now().UTC()}
			f.service.database.now = clock.Now
			path := "/api/v2/organizations/org_security/entitlements"
			until := clock.value.Add(time.Hour).Truncate(time.Second)
			grant := hostedPlanCommand{ID: "report-grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp_report", Plan: config.Plans[1].PlanReference, Scope: []string{"projects"}, ExpiresAt: &until, Reason: "design partner"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHostedPlanAdminToken, grant), http.StatusNoContent)
			test.after(t, &f, path, clock)
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, testHostedPlanAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var report hostedEntitlementReport
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Grants) != 0 {
				t.Fatalf("report grants = %+v, want none", report.Grants)
			}
		})
	}
}

func TestHostedEmptyEntitlementsUseDefaultCatalog(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		plans *HostedPlansConfig
		zero  bool
	}{
		{name: "absent", zero: true},
		{name: "empty mapping", plans: &HostedPlansConfig{}, zero: true},
		{name: "empty plan list", plans: &HostedPlansConfig{Plans: []HostedPlan{}}, zero: true},
		{name: "windows only", plans: &HostedPlansConfig{WindowSeconds: 3600}},
		{name: "base only", plans: &HostedPlansConfig{Base: PlanReference{ID: "pilot_free", Version: 1}}},
		{name: "default catalog", plans: func() *HostedPlansConfig { c := pilotHostedPlans(); return &c }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.plans.IsZero(); got != test.zero {
				t.Fatalf("IsZero() = %v, want %v", got, test.zero)
			}
			f := newHostedSecurityFixture(t)
			cfg := *f.service.config.Hosted
			cfg.Billing = nil
			cfg.Plans = test.plans
			err := ValidateHostedConfig(&cfg)
			if test.zero {
				if err != nil || cfg.Plans != nil {
					t.Fatalf("ValidateHostedConfig() = %v, plans = %+v; want default catalog", err, cfg.Plans)
				}
				return
			}
			if test.name == "default catalog" {
				if err != nil {
					t.Fatalf("ValidateHostedConfig() = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("partial entitlements accepted")
			}
		})
	}
}

func TestHostedPlansConfigDecodesStrictly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		wantErr    bool
		written    bool
	}{
		{name: "valid catalog", body: "base: {id: free, version: 1}\nplans:\n  - {id: free, version: 1, allowances: {projects: 3}}\n", written: true},
		{name: "empty section", body: "{}\n"},
		{name: "alias to an anchor outside the section", body: "base: *free\nplans:\n  - {id: free, version: 1}\n", written: true},
		{name: "merge key from an anchor outside the section", body: "base: {<<: *free}\n", written: true},
		{name: "misspelled nested key", body: "base: {id: free, version: 1}\nplans:\n  - {id: free, version: 1, allowences: {projects: 3}}\n", wantErr: true},
		{name: "unknown plan reference key", body: "base: {id: free, release: 1}\n", wantErr: true},
		{name: "misspelled section key", body: "windows_seconds: 3600\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decoder := yaml.NewDecoder(strings.NewReader("free: &free {id: free, version: 1}\nentitlements:\n" + indentYAML(test.body)))
			decoder.KnownFields(true)
			var document struct {
				Free         PlanReference     `yaml:"free"`
				Entitlements HostedPlansConfig `yaml:"entitlements"`
			}
			err := decoder.Decode(&document)
			config := document.Entitlements
			if test.wantErr {
				if err == nil {
					t.Fatalf("Decode() = %+v, want an unknown-field error", config)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.written != test.written {
				t.Fatalf("written = %t, want %t", config.written, test.written)
			}
		})
	}
}

func indentYAML(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestHostedPlansConfigRejectsAliasCycles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, document string
		wantErr        bool
	}{
		{name: "self-referential section", document: "entitlements: &e {base: *e}\n", wantErr: true},
		{name: "cycle through a plan", document: "entitlements:\n  plans:\n    - &p {id: free, version: 1, features: [*p]}\n", wantErr: true},
		{name: "one anchor used twice", document: "free: &free {id: free, version: 1}\nentitlements:\n  base: *free\n  plans:\n    - {<<: *free}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var document struct {
				Free         PlanReference     `yaml:"free"`
				Entitlements HostedPlansConfig `yaml:"entitlements"`
			}
			done := make(chan error, 1)
			go func() { done <- yaml.Unmarshal([]byte(test.document), &document) }()
			select {
			case err := <-done:
				if (err != nil) != test.wantErr {
					t.Fatalf("Unmarshal() error = %v, want error %t", err, test.wantErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("decoding did not finish")
			}
		})
	}
}

func TestCapacityCatalog(t *testing.T) {
	f := newHostedSecurityFixture(t)
	cfg := f.service.config
	cfg.DatabasePath = filepath.Join(t.TempDir(), "capacity.db")
	hosted := *cfg.Hosted
	hosted.Plans = nil
	cfg.Hosted = &hosted
	f.service = openTestService(t, cfg)
	for _, test := range []struct {
		id               string
		projects, issues int64
		execution        bool
	}{
		{"free", 1, 200, false}, {"starter", 5, 2000, true}, {"growth", 25, 10000, true}, {"scale", 100, 50000, true},
	} {
		t.Run(test.id, func(t *testing.T) {
			plan, err := readHostedPlan(t.Context(), f.service.database.db, PlanReference{ID: test.id, Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Allowances["projects"] != test.projects || plan.Allowances["unarchived_issues"] != test.issues || slices.Contains(plan.Features, "native_execution") != test.execution || slices.Contains(plan.Features, "model_choice") {
				t.Fatalf("catalog = %#v", plan)
			}
		})
	}
	var customers int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_accounts").Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if customers != 0 || f.service.billing != nil {
		t.Fatal("Free signup created billing state")
	}
	entitlement, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.EffectiveBase.ID != "free" {
		t.Fatalf("new organization plan = %#v", entitlement.EffectiveBase)
	}
	fleetUsage, err := f.service.hostedFleetUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"members", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
		if _, present := entitlement.Usage[name]; !present {
			t.Fatalf("full plan usage omitted unrestricted metric %s", name)
		}
		if _, limited := fleetUsage.Allowances[name]; limited {
			t.Fatalf("unrestricted fleet allowance %s reported a limit", name)
		}
	}
	if fleetUsage.Allowances["projects"].Limit != 1 || fleetUsage.Allowances["unarchived_issues"].Limit != 200 {
		t.Fatalf("fleet capacity = %#v", fleetUsage.Allowances)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	response := f.request(t, owner, http.MethodGet, "/organization/billing", nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), "Free access requires no card") {
		t.Fatal("Free billing page omitted the cardless policy")
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.database.checkHostedClaim(t.Context(), tx, time.Now()); err == nil || !strings.Contains(err.Error(), "Free") {
		t.Fatalf("Free claim refusal = %v", err)
	}
}

func TestCapacityLegacyAssignmentsAndGrants(t *testing.T) {
	f := newHostedSecurityFixture(t)
	d := f.service.database
	var original string
	if err := d.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id='pilot_free' AND version=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "legacy-grant", Action: "grant", ExpectedRevision: 1, GrantID: "legacy", Plan: PlanReference{ID: "comp_team", Version: 1}, Scope: []string{"projects", "native_execution"}, Reason: "existing complimentary access"}); err != nil {
		t.Fatal(err)
	}
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{}); err != nil {
		t.Fatal(err)
	}
	var retained string
	if err := d.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id='pilot_free' AND version=1").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	entitlement, err := d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if retained != original || entitlement.Base.ID != "pilot_free" || len(entitlement.Grants) != 1 || entitlement.Allowances["projects"] != 20 || entitlement.Allowances["unarchived_issues"] != 2000 || !slices.Contains(entitlement.Features, "native_execution") {
		t.Fatalf("legacy migration = %#v", entitlement)
	}
	for _, resource := range []string{"members", "concurrent_work"} {
		if _, limited := entitlement.Allowances[resource]; limited {
			t.Fatalf("legacy still limits %s", resource)
		}
	}
}

func TestCapacityCostDriversUnknownInputs(t *testing.T) {
	f := newHostedSecurityFixture(t)
	f.seedIssue(t, 1)
	f.seedIssue(t, 2)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE number=2"); err != nil {
		t.Fatal(err)
	}
	entitlement, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	drivers, err := f.service.hostedCostDrivers(t.Context(), entitlement)
	if err != nil {
		t.Fatal(err)
	}
	if drivers.ActiveIssues != 1 || drivers.ArchivedIssues != 1 || drivers.DatabaseBytes == nil || drivers.WALBytes == nil {
		t.Fatalf("drivers=%#v", drivers)
	}
	if drivers.RequestBytes != nil || drivers.ResponseBytes != nil || drivers.ArtifactRetainedBytes != nil || drivers.RelayBytes != nil {
		t.Fatalf("unknown inputs reported as measured: %#v", drivers)
	}
	raw, err := json.Marshal(drivers)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"private-title", "private-body", testHubAdminToken, "prj_security"} {
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("cost drivers disclosed %s", sentinel)
		}
	}
	for _, test := range []struct {
		name     string
		observed int64
		known    bool
	}{{"unobserved", 0, false}, {"measured zero", 1, true}} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_artifact_usage(singleton,service_id,usage_json,observed_at) VALUES(1,'service_capacity','{}',?) ON CONFLICT(singleton) DO UPDATE SET observed_at=excluded.observed_at", test.observed); err != nil {
				t.Fatal(err)
			}
			entitlement, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			drivers, err := f.service.hostedCostDrivers(t.Context(), entitlement)
			if err != nil {
				t.Fatal(err)
			}
			if (drivers.ArtifactRetainedBytes != nil) != test.known || (drivers.ArtifactReservedBytes != nil) != test.known {
				t.Fatalf("artifact measurement = %#v", drivers)
			}
		})
	}
}

func TestCapacityGrantPreservesUnrestrictedResources(t *testing.T) {
	f := newHostedSecurityFixture(t)
	config := capacityHostedPlans()
	grant := HostedPlan{PlanReference: PlanReference{ID: "capacity_grant", Version: 1}, Allowances: map[string]int64{"projects": 9, "repositories": 1, "registered_runners": 1, "connected_runners": 1}}
	config.Plans = append(config.Plans, grant)
	d := f.service.database
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &config}); err != nil {
		t.Fatal(err)
	}
	if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "capacity-base", Action: "base", ExpectedRevision: 1, Plan: config.Base, Reason: "capacity access"}); err != nil {
		t.Fatal(err)
	}
	if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "capacity-grant", Action: "grant", ExpectedRevision: 2, GrantID: "capacity", Plan: grant.PlanReference, Scope: []string{"projects", "repositories", "registered_runners", "connected_runners"}, Reason: "additional capacity"}); err != nil {
		t.Fatal(err)
	}
	entitlement, err := d.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Allowances["projects"] != 9 {
		t.Fatalf("project grant = %#v", entitlement.Allowances)
	}
	for _, resource := range []string{"repositories", "registered_runners", "connected_runners"} {
		if _, limited := entitlement.Allowances[resource]; limited {
			t.Fatalf("grant introduced a %s restriction", resource)
		}
	}
}

func TestCapacityLegacyMetadataKeepsImmutablePlan(t *testing.T) {
	f := newHostedSecurityFixture(t)
	cfg := *f.service.config.Hosted
	cfg.Plans = nil
	cfg.PlanID = "pilot_free"
	cfg.StorageQuotaBytes = 64 << 20
	cfg.EventQuota = 10000
	if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	entitlement, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entitlement.Base.ID != "pilot_free" || entitlement.Allowances["projects"] != 10 || !slices.Contains(entitlement.Features, "native_execution") {
		t.Fatalf("legacy metadata lost access: %#v", entitlement)
	}
}

func TestCapacityFreeNativeClaimRefusal(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Free exploration")
	worker := f.worker(t, "free-worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "free-machine", "hostname": "free-machine", "version": "test", "capacity": 1}), http.StatusOK)
	d := f.service.database
	d.hostedOrganization = f.project.OrganizationID
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: "free-machine", SessionID: "free-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusTooManyRequests)
	if !strings.Contains(response.Body.String(), "Free") || !strings.Contains(response.Body.String(), "Upgrade") {
		t.Fatalf("claim refusal=%s", response.Body.String())
	}
	var leases int
	if err := d.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatal("Free dispatch allocated a lease")
	}
}

type hostedConsumptionQueries struct {
	nativeQueryer
	statements []string
	resultRows int64
}

func (q *hostedConsumptionQueries) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	q.statements = append(q.statements, statement)
	q.resultRows++
	return q.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func (q *hostedConsumptionQueries) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.statements = append(q.statements, statement)
	var count int64
	if err := q.nativeQueryer.QueryRowContext(ctx, "SELECT count(*) FROM ("+statement+")", args...).Scan(&count); err != nil {
		return nil, err
	}
	q.resultRows += count
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func TestHostedNativeMutationConsumption(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "accounting")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "worker")
	credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
	if err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	now := time.Now()
	previous := 0
	var runEvent tracker.NativeRunEvent
	var runItem tracker.NativeWorkItemID
	for _, retained := range []int{1, 32} {
		d.hostedPlans = nil
		for i := previous; i < retained; i++ {
			issue := f.create(t, fmt.Sprintf("retained-%d", i))
			body := strings.Repeat("x", 32768)
			if _, err := d.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", body, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token,
				tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", i)}, Body: body}), http.StatusOK)
			lease := claimNativeAttempt(t, f, worker, fmt.Sprintf("machine-%d", i), fmt.Sprintf("session-%d", i), issue.WorkItemID)
			runEvent = nativeStartedEvent(lease)
			runItem = issue.WorkItemID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, runEvent), http.StatusOK)
			if _, err := d.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.retained',?) WHERE lease_id=?", body, lease.ID); err != nil {
				t.Fatal(err)
			}
		}
		previous = retained
		f.service.config.Hosted = &HostedConfig{}
		d.hostedOrganization = f.project.OrganizationID
		plans := hostedTestPlans(t, f.service, nil)
		f.service.config.Hosted = nil
		window := now.Unix() / plans.WindowSeconds * plans.WindowSeconds
		if _, err := d.db.ExecContext(t.Context(), "INSERT OR REPLACE INTO hosted_usage_windows(window_start,metric,amount) VALUES(?,'api_mutations',7),(?,'ingested_events',3)", window, window); err != nil {
			t.Fatal(err)
		}
		fullQueries := &hostedConsumptionQueries{nativeQueryer: d.db}
		full, err := d.hostedConsumption(t.Context(), fullQueries, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(fullQueries.statements) != 14 || full["collaboration_bytes"] < int64(retained*3*32768) {
			t.Fatalf("full queries=%d bytes=%d", len(fullQueries.statements), full["collaboration_bytes"])
		}
		for index, test := range []struct {
			name       string
			allowances map[string]int64
			at         time.Duration
			queries    int
		}{
			{"limited retained data", map[string]int64{"collaboration_bytes": 1 << 30, "history_records": 10000, "api_mutations": 10000, "ingested_events": 10000}, 0, 7},
			{"unlimited retained data", map[string]int64{"api_mutations": 0, "ingested_events": 0, "members": 10, "concurrent_work": 5}, 0, 5},
			{"next usage window", map[string]int64{"api_mutations": 0, "ingested_events": 0}, time.Duration(plans.WindowSeconds) * time.Second, 5},
			{"window overrides gauge", map[string]int64{"projects": 10}, 0, 6},
		} {
			t.Run(fmt.Sprintf("%d retained/Fleet %s", retained, test.name), func(t *testing.T) {
				tx, err := d.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				plan := HostedPlan{PlanReference: PlanReference{ID: "fleet_projection", Version: int64(index + 1)}, Allowances: test.allowances}
				raw, err := json.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO hosted_plans(id,version,record_json) VALUES(?,?,?)", plan.ID, plan.Version, string(raw)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id=?,base_version=?", plan.ID, plan.Version); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO hosted_usage_windows(window_start,metric,amount) VALUES(?,'projects',99)", window); err != nil {
					t.Fatal(err)
				}
				at := now.Add(test.at)
				queries := &hostedConsumptionQueries{nativeQueryer: tx}
				selected, err := d.readHostedPlanUsage(t.Context(), queries, at, hostedAllowanceNames()...)
				if err != nil {
					t.Fatal(err)
				}
				authoritative, err := d.readHostedPlanUsage(t.Context(), tx, at)
				if err != nil {
					t.Fatal(err)
				}
				if !maps.Equal(selected.Allowances, authoritative.Allowances) || !selected.WindowEndsAt.Equal(authoritative.WindowEndsAt) {
					t.Fatalf("projection changed entitlement: %#v", selected)
				}
				for name := range authoritative.Allowances {
					if selected.Usage[name] != authoritative.Usage[name] {
						t.Fatalf("%s=%d full=%d", name, selected.Usage[name], authoritative.Usage[name])
					}
				}
				if len(queries.statements) != test.queries {
					t.Fatalf("Fleet queries=%d want=%d: %v", len(queries.statements), test.queries, queries.statements)
				}
				for _, statement := range queries.statements {
					if strings.Contains(statement, "hosted_members") || strings.Contains(statement, "hosted_member_reservations") || strings.Contains(statement, "FROM leases") || strings.Contains(statement, "runner_identities") || strings.Contains(statement, "repositories") || strings.Contains(statement, "hosted_artifact_usage") || statement == "SELECT count(*) FROM collaboration_events" {
						t.Fatalf("Fleet read unused metric: %s", statement)
					}
					if _, limited := selected.Allowances["collaboration_bytes"]; !limited && strings.Contains(statement, "CAST(") {
						t.Fatalf("Fleet summed unlimited retained content: %s", statement)
					}
					if _, limited := selected.Allowances["history_records"]; !limited && strings.Contains(statement, "native_attempt_events") {
						t.Fatalf("Fleet counted unlimited history: %s", statement)
					}
				}
				if authoritative.Usage["collaboration_bytes"] != full["collaboration_bytes"] || authoritative.Usage["history_records"] != full["history_records"] {
					t.Fatal("full plan reader omitted unlimited retained usage")
				}
				wantMutations, wantEvents := int64(7), int64(3)
				if test.at > 0 {
					wantMutations, wantEvents = 0, 0
				}
				if selected.Usage["api_mutations"] != wantMutations || selected.Usage["ingested_events"] != wantEvents {
					t.Fatalf("Fleet window usage=%v want mutations=%d events=%d", selected.Usage, wantMutations, wantEvents)
				}
				if test.name == "window overrides gauge" && selected.Usage["projects"] != 99 {
					t.Fatalf("Fleet discarded window gauge override: %v", selected.Usage)
				}
				t.Logf("retained_issue_comment_attempt_rows=%d Fleet_queries=%d result_rows=%d allowances=%v window=%s", retained*3, len(queries.statements), queries.resultRows, selected.Allowances, selected.WindowEndsAt)
			})
		}
		for _, test := range []struct {
			name    string
			metrics []string
			queries int
		}{
			{"ordinary mutation", hostedNativeMutationMetrics(struct{}{}, false), 9},
			{"ordinary completion", hostedNativeMutationMetrics(struct{}{}, true), 6},
			{"run observation", hostedNativeMutationMetrics(tracker.NativeRunEvent{Type: "run.observed"}, false), 4},
			{"run completion", hostedNativeMutationMetrics(tracker.NativeRunEvent{Type: "run.finished"}, true), 2},
			{"lease validation", hostedRunnerTransactionMetrics(nativeBase + "/leases/:lease/validate"), 2},
			{"provider preview", hostedRunnerTransactionMetrics(nativeBase + "/claims/preview"), 2},
			{"heartbeat", hostedRunnerTransactionMetrics(nativeBase + "/machines/:machine/heartbeat"), 3},
			{"enrollment redemption", hostedRunnerTransactionMetrics(enrollmentBase + "/redeem"), 14},
			{"runner mutation", hostedRunnerTransactionMetrics(runnerBase + "/:runner/rotate"), 14},
			{"attachment preflight", []string{"collaboration_bytes"}, 1},
			{"attachment growth", []string{"collaboration_bytes", "usage_windows"}, 2},
		} {
			t.Run(fmt.Sprintf("%d retained/%s", retained, test.name), func(t *testing.T) {
				queries := &hostedConsumptionQueries{nativeQueryer: d.db}
				actual, err := d.hostedConsumption(t.Context(), queries, now, test.metrics...)
				if err != nil {
					t.Fatal(err)
				}
				if len(queries.statements) != test.queries {
					t.Fatalf("queries=%d want=%d", len(queries.statements), test.queries)
				}
				for name, amount := range actual {
					if full[name] != amount {
						t.Fatalf("%s=%d full=%d", name, amount, full[name])
					}
				}
				for _, name := range test.metrics {
					if name != "usage_windows" && actual[name] != full[name] {
						t.Fatalf("omitted %s", name)
					}
				}
				if test.name == "lease validation" || test.name == "provider preview" || test.name == "heartbeat" {
					wantMetrics, wantRows := 3, int64(3)
					if test.name == "heartbeat" {
						wantMetrics++
						wantRows += int64(retained)
					}
					if actual["api_mutations"] != 7 || actual["ingested_events"] != 3 || len(actual) != wantMetrics || queries.resultRows != wantRows {
						t.Fatalf("%s accounting=%v rows=%d", test.name, actual, queries.resultRows)
					}
					for _, statement := range queries.statements {
						if strings.Contains(statement, "issues") || strings.Contains(statement, "native_comments") || strings.Contains(statement, "native_attempts") || strings.Contains(statement, "CAST(") {
							t.Fatalf("%s aggregated retained JSON: %s", test.name, statement)
						}
					}
				}
				if test.name == "attachment preflight" && (len(actual) != 1 || queries.resultRows != 1) {
					t.Fatalf("attachment accounting=%v rows=%d", actual, queries.resultRows)
				}
				if test.name == "attachment growth" && (actual["api_mutations"] != 7 || len(actual) != 3 || queries.resultRows != 3) {
					t.Fatalf("attachment growth=%v rows=%d", actual, queries.resultRows)
				}
				if test.name != "runner mutation" && test.name != "enrollment redemption" {
					for _, statement := range queries.statements {
						if strings.Contains(statement, "hosted_artifact_usage") || strings.Contains(statement, "hosted_members") || strings.Contains(statement, "hosted_member_reservations") || strings.Contains(statement, "FROM leases") {
							t.Fatalf("unrelated report query: %s", statement)
						}
					}
				}
				t.Logf("retained_issue_comment_attempt_rows=%d full_queries=%d full_result_rows=%d selected_queries=%d selected_result_rows=%d retained_bytes=%d", retained*3, len(fullQueries.statements), fullQueries.resultRows, len(queries.statements), queries.resultRows, full["collaboration_bytes"])
			})
		}

		for _, test := range []struct {
			name                             string
			unchanged, replay, legacy, start bool
		}{
			{name: "new attempt", start: true},
			{name: "published replacement"},
			{name: "heartbeat replacement", unchanged: true},
			{name: "sequence replay", replay: true},
			{name: "legacy event", legacy: true},
		} {
			t.Run(fmt.Sprintf("%d retained/local %s", retained, test.name), func(t *testing.T) {
				tx, err := d.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var stored string
				if err := tx.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", runEvent.Data.AttemptID).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				event := runEvent
				if err := json.Unmarshal([]byte(stored), &event.Data); err != nil {
					t.Fatal(err)
				}
				event.IdempotencyKey = "local-accounting"
				if test.start {
					if _, err := tx.ExecContext(t.Context(), "DELETE FROM native_attempt_events WHERE attempt_id=?", event.Data.AttemptID); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "DELETE FROM native_attempts WHERE id=?", event.Data.AttemptID); err != nil {
						t.Fatal(err)
					}
				} else if test.legacy {
					event.Data.AttemptID, event.Data.Sequence = newNativeID("attempt"), 0
				} else if !test.replay {
					event.Type, event.Data.Sequence = "run.observed", 2
					event.Data.Runtime = &tracker.NativeRuntimeObservation{HeartbeatAt: now, Phase: "implementation"}
					priorRuntime := "null"
					if test.unchanged {
						previous := *event.Data.Runtime
						previous.HeartbeatAt = now.Add(-time.Second)
						priorRuntime, err = marshalNative(previous)
						if err != nil {
							t.Fatal(err)
						}
					}
					evidence, err := marshalNative([]tracker.NativeEvidence{{AttachmentID: "att_retained", Caption: "保留された証拠", Reference: "retained"}})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime',json(?),'$.evidence',json(?)) WHERE id=?", priorRuntime, evidence, event.Data.AttemptID); err != nil {
						t.Fatal(err)
					}
				}
				queries := &hostedConsumptionQueries{nativeQueryer: tx}
				metrics := hostedNativeMutationMetrics(event, false)
				before, err := d.hostedConsumption(t.Context(), queries, now, metrics...)
				if err != nil {
					t.Fatal(err)
				}
				state, err := readHostedRunEventState(t.Context(), queries, event)
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
				publish := true
				if !test.legacy {
					appended, history, err := recordNativeAttempt(t.Context(), tx, scope, runItem, event, now)
					if err != nil {
						t.Fatal(err)
					}
					publish = appended && history
				}
				if publish {
					if err := appendNativeHistory(t.Context(), tx, scope, string(runItem), event.Type, tracker.CollaborationData{Run: &event.Data}, now); err != nil {
						t.Fatal(err)
					}
				}
				response := `{"accepted":true}`
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?)", scope.organization, credential.ID, "local-event", event.IdempotencyKey, "hash", response, formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				after, err := d.hostedRunEventConsumption(t.Context(), queries, now, before, state, event, response)
				if err != nil {
					t.Fatal(err)
				}
				full, err := d.hostedConsumption(t.Context(), tx, now, metrics...)
				if err != nil || !maps.Equal(after, full) {
					t.Fatalf("local=%v full=%v error=%v", after, full, err)
				}
				if len(queries.statements) != 7 || queries.resultRows != 9 {
					t.Fatalf("queries=%d rows=%d", len(queries.statements), queries.resultRows)
				}
				for _, statement := range []string{queries.statements[4], queries.statements[5]} {
					args := []any{event.Data.AttemptID, event.Data.AttemptID, event.Data.Sequence}
					if strings.Contains(statement, "rowid>?") {
						args = append(args, state.eventRowID)
					}
					rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						if strings.Contains(detail, "SCAN collaboration_events") || strings.Contains(detail, "SCAN native_attempts") || strings.Contains(detail, "SCAN native_attempt_events") {
							t.Fatalf("unbounded local accounting: %s", detail)
						}
					}
					if err := errors.Join(rows.Err(), rows.Close()); err != nil {
						t.Fatal(err)
					}
				}
				t.Logf("retained=%d ordinary_accounting_queries=%d tenant_byte_aggregates=1 local_state_rows=2", retained, len(queries.statements))
			})
		}
	}
}

func TestHostedRunnerTransactionQuotas(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		preview  bool
		stale    bool
		active   bool
		capacity int
		limits   map[string]int64
		status   int
	}{
		{name: "heartbeat reconnect exhaustion", stale: true, capacity: 2, limits: map[string]int64{"connected_runners": 0}, status: http.StatusTooManyRequests},
		{name: "heartbeat drained reconnect exhaustion", stale: true, limits: map[string]int64{"connected_runners": 0}, status: http.StatusTooManyRequests},
		{name: "heartbeat connected without growth", capacity: 2, limits: map[string]int64{"connected_runners": 0}, status: http.StatusOK},
		{name: "heartbeat unrelated exhaustion", stale: true, capacity: 8, limits: map[string]int64{"registered_runners": 0, "projects": 0, "repositories": 0, "unarchived_issues": 0, "collaboration_bytes": 1, "history_records": 0, "ingested_events": 0}, status: http.StatusOK},
		{name: "heartbeat API exhaustion", capacity: 2, limits: map[string]int64{"api_mutations": 0}, status: http.StatusTooManyRequests},
		{name: "heartbeat active completion while exhausted", stale: true, active: true, capacity: 2, limits: map[string]int64{"connected_runners": 0, "api_mutations": 0}, status: http.StatusOK},
		{name: "preview API exhaustion", preview: true, capacity: 2, limits: map[string]int64{"api_mutations": 0}, status: http.StatusTooManyRequests},
		{name: "preview unrelated exhaustion", preview: true, capacity: 2, limits: map[string]int64{"connected_runners": 0, "registered_runners": 0, "projects": 0, "repositories": 0, "unarchived_issues": 0, "collaboration_bytes": 1, "history_records": 0, "ingested_events": 0}, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "runner-quota")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "running", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}), http.StatusOK)
			}
			d := f.service.database
			if test.stale {
				stale := formatHubTime(time.Now().Add(-time.Hour))
				if _, err := d.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", stale, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := d.db.ExecContext(t.Context(), "UPDATE machines SET last_heartbeat_at=? WHERE id=?", stale, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			f.service.config.Hosted = &HostedConfig{}
			d.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, test.limits)
			f.service.config.Hosted = nil
			before, err := d.hostedConsumption(t.Context(), d.db, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
			var body any = map[string]any{"capacity": test.capacity, "version": "updated", "backend_isolation": r.redemption.BackendIsolation}
			if test.preview {
				path = f.base + "/claims/preview"
				body = tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID}}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, body), test.status)
			after, err := d.hostedConsumption(t.Context(), d.db, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				if !maps.Equal(before, after) {
					t.Fatalf("rejected operation changed usage: %v -> %v", before, after)
				}
				var version string
				if err := d.db.QueryRowContext(t.Context(), "SELECT version FROM machines WHERE id=?", r.binding.MachineID).Scan(&version); err != nil || version != "test" {
					t.Fatalf("rejected heartbeat version=%s error=%v", version, err)
				}
				return
			}
			if after["api_mutations"] != before["api_mutations"]+1 || after["ingested_events"] != before["ingested_events"] || after["history_records"] != before["history_records"] || after["registered_runners"] != before["registered_runners"] {
				t.Fatalf("operation accounting: %v -> %v", before, after)
			}
			wantHeartbeats := before["heartbeats"]
			if !test.preview {
				wantHeartbeats++
			}
			if after["heartbeats"] != wantHeartbeats {
				t.Fatalf("heartbeat usage=%d want=%d", after["heartbeats"], wantHeartbeats)
			}
		})
	}
}

func TestHostedNativeRunMutationQuotas(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		limits map[string]int64
		kind   string
		status int
	}{
		{"unrelated allocation exhaustion", map[string]int64{"projects": 0, "unarchived_issues": 0, "repositories": 0, "registered_runners": 0, "connected_runners": 0}, "run.observed", http.StatusOK},
		{"storage exhaustion", map[string]int64{"collaboration_bytes": 1}, "run.observed", http.StatusTooManyRequests},
		{"history exhaustion", map[string]int64{"history_records": 0}, "run.observed", http.StatusTooManyRequests},
		{"event exhaustion", map[string]int64{"ingested_events": 0}, "run.observed", http.StatusTooManyRequests},
		{"window exhaustion", map[string]int64{"api_mutations": 0}, "run.observed", http.StatusTooManyRequests},
		{"failed completion while exhausted", map[string]int64{"collaboration_bytes": 1, "history_records": 0, "ingested_events": 0, "api_mutations": 0}, "run.finished", http.StatusOK},
		{"checkpoint while exhausted", map[string]int64{"collaboration_bytes": 1, "history_records": 0, "ingested_events": 0, "api_mutations": 0}, "run.checkpointed", http.StatusOK},
		{"start allowed", nil, "run.started", http.StatusOK},
		{"start storage exhaustion", map[string]int64{"collaboration_bytes": 1}, "run.started", http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}), "", "runtime-quota")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			event := nativeStartedEvent(lease)
			if test.kind != "run.started" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			}
			f.service.config.Hosted = &HostedConfig{}
			f.service.database.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, test.limits)
			f.service.config.Hosted = nil
			if test.kind != "run.started" {
				event.Type, event.IdempotencyKey, event.Data.Sequence = test.kind, "next", 2
				event.Data.Runtime = &tracker.NativeRuntimeObservation{HeartbeatAt: time.Now(), Phase: "implementation"}
			}
			if test.kind == "run.finished" {
				event.Data.Outcome = "failed"
			}
			if test.kind == "run.checkpointed" {
				event.Data.Handoff = nativeTestCheckpoint()
			}
			before, err := f.service.database.hostedConsumption(t.Context(), f.service.database.db, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), test.status)
			after, err := f.service.database.hostedConsumption(t.Context(), f.service.database.db, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				if !maps.Equal(before, after) {
					t.Fatalf("rejected observation changed usage: %v -> %v", before, after)
				}
				return
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			replay, err := f.service.database.hostedConsumption(t.Context(), f.service.database.db, time.Now())
			if err != nil || !maps.Equal(after, replay) {
				t.Fatalf("replay usage changed: %v -> %v error=%v", after, replay, err)
			}
		})
	}
}
