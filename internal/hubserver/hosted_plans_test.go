package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
			if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
				t.Fatal(err)
			}
			if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "downgrade", Action: "base", ExpectedRevision: 1, Plan: plan.PlanReference, Reason: "pilot downgrade"})); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			before, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), now))
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
			after, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), now))
			if err != nil {
				t.Fatal(err)
			}
			for resource := range before.Allowances {
				if before.Usage[resource] != after.Usage[resource] {
					t.Errorf("retry or rejection changed %s usage: before=%d after=%d", resource, before.Usage[resource], after.Usage[resource])
				}
			}
			for _, table := range []string{"hosted_project_grants", "token_grants", "project_workflow_states"} {
				var orphans int
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" x WHERE organization_id=$1 AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.organization_id=x.organization_id AND p.id=x.project_id)", f.service.database.hostedOrganization).Scan(&orphans); err != nil || orphans != 0 {
					t.Fatalf("rejected allocation left %s records: %d %v", table, orphans, err)
				}
			}
			for _, path := range []string{"/api/v2/organizations/org_browser_preview/projects/" + project, "/api/v2/organizations/org_browser_preview/projects/" + project + "/onboarding", "/organization/plan", "/api/cloud/billing"} {
				requireNativeStatus(t, f.page(t, "owner", path), http.StatusOK)
			}
		})
	}
}

func hostedTestPlans(t *testing.T, service *Service, limits map[string]int64) hostedplans.HostedPlansConfig {
	t.Helper()
	service.config.Hosted.EntitlementAdminToken = []byte(testHostedPlanAdminToken)
	service.config.Hosted.EntitlementAdministrator = "test-operator"
	config := hostedplans.PilotPlans()
	free := config.Plans[0]
	free.ID = "test_free"
	maps.Copy(free.Allowances, limits)
	paid := free
	paid.ID = "test_paid"
	paid.Allowances = maps.Clone(free.Allowances)
	paid.Allowances["projects"] = 20
	paid.Allowances["concurrent_work"] = 20
	paid.Features = append(slices.Clone(free.Features), "hosted_artifacts")
	transition := hostedplans.PilotPlans()
	transition.Plans = append(transition.Plans, free, paid)
	cfg := *service.config.Hosted
	cfg.Plans = &transition
	if err := hubapi.FromFeatureError(service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	if _, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id=$1,base_version=$2 WHERE organization_id=$3", free.ID, free.Version, service.database.hostedOrganization); err != nil {
		t.Fatal(err)
	}
	config.Plans = []hostedplans.HostedPlan{free, paid}
	config.Base = free.PlanReference
	cfg.Plans = &config
	if err := hubapi.FromFeatureError(service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	return config
}

func observeHostedArtifactUsage(t *testing.T, service *Service) {
	t.Helper()
	if _, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), `INSERT INTO hosted_artifact_usage(organization_id,service_id,observed_at,retained_bytes,reserved_bytes) VALUES($1,'service_example',$2,0,0)`, service.database.hostedOrganization, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
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
			observeHostedArtifactUsage(t, f.service)
			transition := config
			transition.Plans = slices.Clone(config.Plans)
			for index := range config.Plans {
				config.Plans[index].Allowances = maps.Clone(config.Plans[index].Allowances)
				delete(config.Plans[index].Allowances, "members")
				config.Plans[index].Version++
			}
			transition.Plans = append(transition.Plans, config.Plans...)
			config.Base = config.Plans[0].PlanReference
			cfg := *f.service.config.Hosted
			cfg.Plans = &transition
			if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
				t.Fatal(err)
			}
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_version=$1 WHERE organization_id=$2", config.Base.Version, f.service.database.hostedOrganization); err != nil {
				t.Fatal(err)
			}
			cfg.Plans = &config
			if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
				t.Fatal(err)
			}
			f.user(t, "owner", "owner", "owner@example.test", "", "")
			f.service.database.now = func() time.Time { return now }
			revision := int64(1)
			apply := func(command hostedplans.PlanCommand) {
				t.Helper()
				command.ExpectedRevision = revision
				command.Reason = "pilot testing"
				if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, command)); err != nil {
					t.Fatal(err)
				}
				revision++
			}
			if test.subscription {
				until := now.Add(2 * time.Minute)
				apply(hostedplans.PlanCommand{ID: "paid", Action: "subscription", Plan: config.Plans[1].PlanReference, ExpiresAt: &until})
			}
			if test.grant {
				until := now.Add(time.Minute)
				plan, scope := config.Plans[1].PlanReference, []string{"projects"}
				if test.modelChoice {
					plan, scope = config.Base, []string{"model_choice"}
				}
				apply(hostedplans.PlanCommand{ID: "grant", Action: "grant", GrantID: "pilot_grant", Plan: plan, Scope: scope, ExpiresAt: &until})
			}
			if test.revoke {
				apply(hostedplans.PlanCommand{ID: "revoke", Action: "revoke", GrantID: "pilot_grant"})
			}
			got, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), now.Add(test.at)))
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
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/platform/usage", nil).WithContext(t.Context())
			if err := f.service.platformTenantUsage(echo.New().NewContext(request, response)); err != nil {
				t.Fatal(err)
			}
			var tenant platformTenantUsage
			if err := json.Unmarshal(response.Body.Bytes(), &tenant); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || tenant.WindowEndsAt != fleet.WindowEndsAt || tenant.Allowances["projects"].Limit == nil || *tenant.Allowances["projects"].Limit != test.projects || tenant.Allowances["projects"].Used != 1 {
				t.Fatalf("tenant usage = %d %+v", response.Code, tenant)
			}
			if members, ok := tenant.Allowances["members"]; !ok || members.Limit != nil || members.Used != 1 {
				t.Fatalf("unlimited member usage = %+v, present = %t", members, ok)
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
	command := hostedplans.PlanCommand{ID: "grant", Action: "grant", ExpectedRevision: 1, GrantID: "comp", Plan: config.Plans[1].PlanReference, Scope: []string{"projects"}, Reason: "pilot approval"}
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
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_audit WHERE organization_id=$1 AND action='grant'", f.service.database.hostedOrganization).Scan(&audits); err != nil {
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
	bootstrapToken := hubapi.NewID("token")
	now := f.service.config.now().UTC()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: bootstrapTokenID, Name: "Bootstrap administrator", Hash: apikey.HashToken(bootstrapToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/cloud/metadata", testHubAdminToken, nil), http.StatusForbidden)
	report := performHubAPIRequest(t, f.service, http.MethodGet, "/api/cloud/metadata", bootstrapToken, nil)
	requireNativeStatus(t, report, http.StatusOK)
	for _, forbidden := range []string{"pilot approval", "private-project-sentinel", "owner@example.test", "record_json", "request_hash"} {
		if strings.Contains(report.Body.String(), forbidden) {
			t.Fatalf("metadata leaked %q", forbidden)
		}
	}
}

func TestHostedConcurrentClaimsDowngradeRelease(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := &leaseTestClock{value: now}
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.now = clock.Now
		*cfg = hostedPostgresConfig(t, *cfg)
		cfg.Postgres.LeaseClock = clock.Now
	})
	config := hostedTestPlans(t, f.service, map[string]int64{"concurrent_work": 1})
	work := newNativeFixture(t, f.service, f.service.database.hostedOrganization, "plan-concurrency")
	const contenders = 8
	requests := make([]tracker.NativeClaim, contenders)
	tokens := make([]string, contenders)
	for i := range contenders {
		issue := work.create(t, fmt.Sprintf("Plan work %d", i))
		tokens[i] = work.worker(t, fmt.Sprintf("plan-runner-%d", i))
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), work.project.OrganizationID, apikey.HashToken(tokens[i]), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("runner authority status=%d error=%v", status, err)
		}
		heartbeatHostedPlanWorker(t, work, tokens[i], credential.Runner.MachineID, 1)
		requests[i] = tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: credential.Runner.MachineID, SessionID: fmt.Sprintf("session-%d", i), TTLSeconds: 60, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	}
	type outcome struct {
		lease tracker.NativeLease
		err   error
		index int
	}
	start := make(chan struct{})
	results := make(chan outcome, contenders)
	var group sync.WaitGroup
	for i, request := range requests {
		group.Go(func() {
			<-start
			lease, err := claimHostedPlanWork(t, work, tokens[i], request)
			results <- outcome{lease, err, i}
		})
	}
	close(start)
	group.Wait()
	close(results)
	var winner outcome
	var leases []tracker.NativeLease
	winners := 0
	for result := range results {
		if result.err == nil {
			winner = result
			leases = append(leases, result.lease)
			winners++
			continue
		}
		var refusal *hubapi.NativeError
		if !errors.As(result.err, &refusal) || refusal.Code != "plan_capacity" {
			t.Errorf("claim error: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winners", winners)
	}
	again, err := claimHostedPlanWork(t, work, tokens[winner.index], requests[winner.index])
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
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "downgrade", Action: "base", ExpectedRevision: 1, Plan: zero.PlanReference, Reason: "pilot downgrade"})); err != nil {
		t.Fatal(err)
	}
	renewed := performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(winner.lease.ID)+"/renew", tokens[winner.index], tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, TTLSeconds: 60})
	if renewed.Code != http.StatusOK {
		t.Fatalf("downgrade prevented safe renewal: %d %s", renewed.Code, renewed.Body.String())
	}
	for _, lease := range leases {
		if lease.ID != winner.lease.ID {
			for index := range tokens {
				if lease.MachineID == requests[index].MachineID {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(lease.ID)+"/release", tokens[index], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "work_item_hydration_failed"}), http.StatusNoContent)
				}
			}
		}
	}
	for i := range 2 {
		response := performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(winner.lease.ID)+"/release", tokens[winner.index], tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, Reason: "work_item_hydration_failed"})
		if i == 0 && response.Code != http.StatusNoContent || i == 1 && (response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"stale_fencing_token"`)) {
			t.Fatalf("release %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	usage, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), now))
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["concurrent_work"] != 0 {
		t.Fatal("failed start retained capacity")
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "restore", Action: "base", ExpectedRevision: 2, Plan: config.Base, Reason: "pilot restore"})); err != nil {
		t.Fatal(err)
	}
	next := requests[(winner.index+1)%contenders]
	next.SessionID = "restored-session"
	lease, err := claimHostedPlanWork(t, work, tokens[(winner.index+1)%contenders], next)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	next = requests[(winner.index+2)%contenders]
	next.SessionID = "expired-session"
	if _, err := claimHostedPlanWork(t, work, tokens[(winner.index+2)%contenders], next); err != nil {
		t.Fatalf("exact expiry retained capacity: %v", err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(lease.ID)+"/release", tokens[(winner.index+1)%contenders], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"stale_fencing_token"`) {
		t.Fatalf("stale release=%d %s", response.Code, response.Body.String())
	}
	t.Run("configured plan without a concurrency allowance", func(t *testing.T) {
		unlimited := config.Plans[0]
		unlimited.Version = 3
		unlimited.Allowances = maps.Clone(unlimited.Allowances)
		delete(unlimited.Allowances, "concurrent_work")
		config.Plans = append(config.Plans, unlimited)
		cfg.Plans = &config
		if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
			t.Fatal(err)
		}
		if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "unlimited", Action: "base", ExpectedRevision: 3, Plan: unlimited.PlanReference, Reason: "unrestricted concurrency"})); err != nil {
			t.Fatal(err)
		}
		next := requests[(winner.index+3)%contenders]
		next.SessionID = "unrestricted-session"
		if _, err := claimHostedPlanWork(t, work, tokens[(winner.index+3)%contenders], next); err != nil {
			t.Fatalf("absent concurrency allowance refused an independent claim while another lease was active: %v", err)
		}
	})
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
		{"internal create failure", nil, http.StatusInternalServerError},
		{"allowed", nil, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
			hostedTestPlans(t, f.service, test.limits)
			if test.want == http.StatusInternalServerError {
				admin := usagePostgresAdmin(t, f.service)
				organization := strings.ReplaceAll(string(f.service.database.hostedOrganization), "'", "''")
				project := strings.ReplaceAll(string(f.project), "'", "''")
				for _, statement := range []string{
					`CREATE FUNCTION public.refuse_plan_issue() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN RAISE EXCEPTION 'private-title private-body credential-secret' USING ERRCODE='P0001'; END $$`,
					"GRANT EXECUTE ON FUNCTION public.refuse_plan_issue() TO " + pgx.Identifier{f.service.config.Postgres.Connection.User}.Sanitize(),
					fmt.Sprintf("CREATE TRIGGER refuse_plan_issue BEFORE INSERT ON public.issues FOR EACH ROW WHEN (NEW.organization_id='%s' AND NEW.project_id='%s') EXECUTE FUNCTION public.refuse_plan_issue()", organization, project),
				} {
					if _, err := admin.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			request := map[string]any{"idempotency_key": "create", "title": "private-title", "body": "private-body" + issueContractTestSections, "state": "Todo"}
			response := f.request(t, f.user, http.MethodPost, f.base+"/work-items", request)
			requireNativeStatus(t, response, test.want)
			if test.want != http.StatusOK {
				var rest hubapi.APIErrorResponse
				decodeHubResponse(t, response, &rest)
				if test.want == http.StatusTooManyRequests && (rest.Code != "allowance_exhausted" || !strings.Contains(rest.Message, "Hosted ") || !strings.Contains(rest.Message, "allowance reached (")) {
					t.Fatalf("REST allowance refusal=%s", response.Body)
				}
				authority, err := operatorCatalog(ctx)
				if err != nil {
					t.Fatal(err)
				}
				ctx = f.service.withOperatorCatalog(ctx, authority.credential, authority.identity.OrganizationID)
				for _, transport := range []string{"stdio", "http"} {
					t.Run(transport, func(t *testing.T) {
						send := hostedContextProtocol(t, f.service, ctx, transport)
						reply := send("tools/call", operatortool.FileIssue, map[string]any{"project_id": f.project, "request_id": "mcp-create", "title": request["title"], "description": request["body"], "state": "Todo"})
						var result struct {
							IsError bool                        `json:"isError"`
							Refusal operatortool.AllowanceError `json:"structuredContent"`
						}
						if err := json.Unmarshal(reply.Result, &result); err != nil || len(reply.Error) != 0 || !result.IsError {
							t.Fatalf("MCP refusal=%s %s err=%v", reply.Result, reply.Error, err)
						}
						if test.want == http.StatusTooManyRequests {
							if result.Refusal.Code != "allowance_exhausted" || result.Refusal.Message != rest.Message || result.Refusal.HTTPStatus != http.StatusTooManyRequests {
								t.Fatalf("MCP allowance=%s REST=%s", reply.Result, response.Body)
							}
						} else if rest.Code != "native_operation_failed" || result.Refusal.Code != "" || !strings.Contains(string(reply.Result), "reason_code service_unavailable") {
							t.Fatalf("internal failure was not generic: MCP=%s REST=%s", reply.Result, response.Body)
						}
						for _, secret := range []string{"private-title", "private-body", "credential-secret"} {
							if strings.Contains(string(reply.Result), secret) || strings.Contains(response.Body.String(), secret) {
								t.Fatalf("create refusal exposed %q", secret)
							}
						}
					})
				}
			}
			var events, commands int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_events WHERE organization_id=$1),(SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key IN ('create','mcp-create'))", f.service.database.hostedOrganization).Scan(&events, &commands); err != nil {
				t.Fatal(err)
			}
			if test.want != http.StatusOK {
				if events != 0 || commands != 0 {
					t.Fatal("rejected mutation persisted data or idempotency receipt")
				}
				return
			}
			before, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, f.request(t, f.user, http.MethodPost, f.base+"/work-items", request), http.StatusOK)
			after, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
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
		change func(*hostedplans.HostedPlansConfig)
	}{
		{"missing base", func(c *hostedplans.HostedPlansConfig) { c.Base.ID = "absent" }},
		{"negative allowance", func(c *hostedplans.HostedPlansConfig) { c.Plans[0].Allowances["projects"] = -1 }},
		{"unknown allowance", func(c *hostedplans.HostedPlansConfig) { c.Plans[0].Allowances["unlimited"] = 1 }},
		{"unbounded retention", func(c *hostedplans.HostedPlansConfig) { c.RetentionWindows = 721 }},
		{"unknown feature", func(c *hostedplans.HostedPlansConfig) { c.Plans[0].Features = []string{"admin_bypass"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := hostedplans.PilotPlans()
			test.change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	f := newHostedSecurityFixture(t)
	config := hostedTestPlans(t, f.service, nil)
	config.Plans[0].Allowances["projects"]++
	cfg := *f.service.config.Hosted
	cfg.Plans = &config
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatalf("changed seed stopped startup: %v", err)
	}
	stored, err := hubapi.FeatureResult(f.service.database.plansService().ReadPlan(t.Context(), nil, config.Base))
	if err != nil || stored.Allowances["projects"] == config.Plans[0].Allowances["projects"] {
		t.Fatalf("changed seed overwrote stored plan: %+v %v", stored, err)
	}
	now := time.Now().UTC()
	metrics := &tenantRequestMetrics{}
	e := echo.New()
	for i := range 30 {
		at := now.Add(time.Duration(i) * time.Hour)
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		c := e.NewContext(request, httptest.NewRecorder())
		key := requestMetricKey{Minute: at.Unix() / 60 * 60, Route: "GET /health", Kind: "anonymous", Status: 2}
		pending := map[requestMetricKey]requestLatency{key: {HTTP: hostedRequestUsageFor(c, time.Microsecond)}}
		if err := metrics.WriteUsage(t.Context(), f.service.database.metricsBillingOwner(), at, pending); err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 || metrics.BillingPending != nil {
			t.Fatalf("recorded telemetry stayed pending: samples=%d billing=%v", len(pending), metrics.BillingPending != nil)
		}
	}
	var count int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_usage_windows WHERE organization_id=$1 AND metric='http_requests'", f.service.database.hostedOrganization).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 24 {
		t.Fatalf("retained %d windows", count)
	}
	t.Run("heartbeat retention through periodic usage flush", func(t *testing.T) {
		clock := &leaseTestClock{value: now.Truncate(time.Microsecond)}
		f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.now = clock.Now })
		hostedTestPlans(t, f.service, nil)
		work := newNativeFixture(t, f.service, f.service.database.hostedOrganization, "heartbeat-retention")
		token := work.worker(t, "retained-heartbeats")
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), work.project.OrganizationID, apikey.HashToken(token), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("runner authority status=%d error=%v", status, err)
		}
		if f.service.requestMetrics == nil {
			t.Fatal("periodic request metrics owner is unavailable")
		}
		for i := range 30 {
			if i > 0 {
				clock.Advance(time.Hour)
			}
			if i == 23 {
				path := "/api/v2/organizations/" + string(work.project.OrganizationID) + "/runners/" + credential.Runner.RunnerID + "/renew"
				response := performHubAPIRequest(t, f.service, http.MethodPost, path, token, struct{}{})
				requireNativeStatus(t, response, http.StatusOK)
				var renewed runnerauth.Identity
				decodeHubResponse(t, response, &renewed)
				if renewed.Binding != credential.Runner.Binding || !renewed.ExpiresAt.Equal(clock.Now().Add(runnerauth.CredentialTTL)) {
					t.Fatalf("credential renewal changed identity or expiry: %+v", renewed)
				}
			}
			heartbeatHostedPlanWorker(t, work, token, credential.Runner.MachineID, 1)
			if err := f.service.requestMetrics.Flush(t.Context(), clock.Now()); err != nil {
				t.Fatal(err)
			}
		}
		for _, metric := range []string{"heartbeats", "http_requests"} {
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_usage_windows WHERE organization_id=$1 AND metric=$2", work.project.OrganizationID, metric).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 24 {
				t.Fatalf("retained %d %s windows, want24", count, metric)
			}
		}
	})
}

func TestHostedPlansDisabledWithoutNetwork(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	if service.database.plansService().Plans() != nil {
		t.Fatal("self-hosted plan restrictions enabled")
	}
	exerciseHostedPlanUnrestrictedWork(t, service)
	var count int
	if err := usagePostgresAdmin(t, service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_assignments").Scan(&count); err != nil || count != 0 {
		t.Fatalf("local assignments %d: %v", count, err)
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
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM runner_identities WHERE organization_id=$1", f.service.database.hostedOrganization).Scan(&count); err != nil {
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

func TestHostedPlanConfigurationDefaults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		plans *hostedplans.HostedPlansConfig
		zero  bool
	}{
		{name: "absent", zero: true},
		{name: "empty mapping", plans: &hostedplans.HostedPlansConfig{}, zero: true},
		{name: "empty plan list", plans: &hostedplans.HostedPlansConfig{Plans: []hostedplans.HostedPlan{}}, zero: true},
		{name: "windows only", plans: &hostedplans.HostedPlansConfig{WindowSeconds: 3600}},
		{name: "base only", plans: &hostedplans.HostedPlansConfig{Base: hostedplans.PlanReference{ID: "pilot_free", Version: 1}}},
		{name: "pilot catalog", plans: func() *hostedplans.HostedPlansConfig { c := hostedplans.PilotPlans(); return &c }()},
		{name: "Cloud catalog", plans: func() *hostedplans.HostedPlansConfig { c := hostedplans.CapacityPlans(); return &c }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.plans.IsZero(); got != test.zero {
				t.Fatalf("IsZero() = %v, want %v", got, test.zero)
			}
			valid := test.zero || test.name == "pilot catalog" || test.name == "Cloud catalog"
			f := newHostedSecurityFixture(t)
			cfg := *f.service.config.Hosted
			cfg.Plans = test.plans
			err := hostedplans.ValidateHostedConfig(&cfg)
			if !valid {
				if err == nil {
					t.Fatal("partial entitlements accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("hostedplans.ValidateHostedConfig() = %v", err)
			}
			configured := newHostedSecurityFixture(t, func(config *Config) { config.Hosted.Plans = cfg.Plans })
			d := configured.service.database
			if test.zero {
				if cfg.Plans != nil || d.plansService().Plans() != nil {
					t.Fatal("hosted configuration without a catalog enabled plan allowances")
				}
				var count int
				if err := usagePostgresAdmin(t, configured.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_plan_assignments WHERE organization_id=$1", d.hostedOrganization).Scan(&count); err != nil || count != 0 {
					t.Fatalf("unconfigured catalog assignments = %d: %v", count, err)
				}
				exerciseHostedPlanUnrestrictedWork(t, configured.service)
				return
			}
			entitlement, err := hubapi.FeatureResult(d.plansService().PlanUsage(t.Context(), time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			want := hostedplans.PlanReference{ID: "pilot_free", Version: 1}
			if test.name == "Cloud catalog" {
				want = hostedplans.PlanReference{ID: "free", Version: 1}
				if entitlement.Allowances["concurrent_work"] != 2 {
					t.Fatalf("Free concurrency = %d, want 2", entitlement.Allowances["concurrent_work"])
				}
			}
			if entitlement.Base != want || entitlement.EffectiveBase != want {
				t.Fatalf("new organization entitlement = %+v, want %v", entitlement, want)
			}
		})
	}
}

func TestCapacityCatalog(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		plans := hostedplans.CapacityPlans()
		cfg.Hosted.Plans = &plans
	})
	for _, test := range []struct {
		ref        hostedplans.PlanReference
		cents      int64
		priced     bool
		concurrent int64
		storage    int64
	}{
		{hostedplans.PlanReference{ID: "free", Version: 1}, 0, true, 2, 1 << 30},
		{hostedplans.PlanReference{ID: "starter", Version: 1}, 4900, true, 8, 25 << 30},
		{hostedplans.PlanReference{ID: "team", Version: 2}, 9900, true, 20, 100 << 30},
		{hostedplans.PlanReference{ID: "growth", Version: 2}, 24900, true, 50, 250 << 30},
		{hostedplans.PlanReference{ID: "scale", Version: 2}, 0, false, 100, 1 << 30},
		{hostedplans.PlanReference{ID: "growth", Version: 1}, 14900, true, 50, 1 << 30},
		{hostedplans.PlanReference{ID: "scale", Version: 1}, 39900, true, 100, 1 << 30},
	} {
		t.Run(fmt.Sprintf("%s v%d", test.ref.ID, test.ref.Version), func(t *testing.T) {
			plan, err := hubapi.FeatureResult(f.service.database.plansService().ReadPlan(t.Context(), nil, test.ref))
			if err != nil {
				t.Fatal(err)
			}
			if (plan.MonthlyUSDCents != nil) != test.priced || test.priced && *plan.MonthlyUSDCents != test.cents {
				t.Fatalf("price = %v", plan.MonthlyUSDCents)
			}
			if plan.Allowances["collaboration_bytes"] != test.storage || plan.Allowances["history_records"] != 2000000 {
				t.Fatalf("storage/history = %#v", plan.Allowances)
			}
			if plan.Allowances["concurrent_work"] != test.concurrent || plan.Allowances["unarchived_issues"] != 10000 || !slices.Contains(plan.Features, "native_execution") || slices.Contains(plan.Features, "model_choice") {
				t.Fatalf("catalog = %#v", plan)
			}
			for _, name := range []string{"projects", "members", "repositories"} {
				if _, limited := plan.Allowances[name]; limited {
					t.Fatalf("tier limits %s", name)
				}
			}
		})
	}
	var customers int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_billing_accounts WHERE organization_id=$1", f.service.database.hostedOrganization).Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if customers != 0 || f.service.billing != nil {
		t.Fatal("Free signup created billing state")
	}
	entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
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
	for _, name := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners"} {
		if _, present := entitlement.Usage[name]; !present {
			t.Fatalf("full plan usage omitted unrestricted metric %s", name)
		}
		if _, limited := fleetUsage.Allowances[name]; limited {
			t.Fatalf("unrestricted fleet allowance %s reported a limit", name)
		}
	}
	if fleetUsage.Allowances["concurrent_work"].Limit != 2 || fleetUsage.Allowances["unarchived_issues"].Limit != 10000 {
		t.Fatalf("fleet capacity = %#v", fleetUsage.Allowances)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	response := f.request(t, owner, http.MethodGet, "/organization/billing", nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), "Free access requires no card") {
		t.Fatal("Free billing page omitted the cardless policy")
	}
	work := newNativeFixture(t, f.service, f.service.database.hostedOrganization, "free-claim")
	issue := work.create(t, "Free claim")
	token, request := hostedPlanWorkRequest(t, work, issue, "free-claim")
	if _, err := claimHostedPlanWork(t, work, token, request); err != nil {
		t.Fatalf("Free claim = %v", err)
	}
}

func TestCapacityCostDriversUnknownInputs(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.now = func() time.Time { return now }
	})
	f.seedIssue(t, 1)
	archived := f.seedIssue(t, 2)
	organization := f.service.accessOrganization()
	repository := f.service.config.TenantRepository
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), f.project, archived)
		if err != nil {
			return err
		}
		row.Archived = true
		_, err = w.UpdateWorkItem(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.readHostedMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drivers := report.CostDrivers
	if drivers.ActiveIssues == nil || *drivers.ActiveIssues != 1 || drivers.ArchivedIssues == nil || *drivers.ArchivedIssues != 1 {
		t.Fatalf("drivers=%#v", drivers)
	}
	if report.DatabaseBytes != nil || drivers.DatabaseBytes != nil || drivers.WALBytes != nil || !slices.Contains(report.UnavailableUsage, "database_bytes") || !slices.Contains(report.UnavailableUsage, "wal_bytes") {
		t.Fatalf("tenant physical storage must be unavailable: %#v", report)
	}
	if drivers.RequestBytes != nil || drivers.ResponseBytes != nil || drivers.ArtifactRetainedBytes != nil || drivers.ArtifactReservedBytes != nil || drivers.RelayBytes != nil {
		t.Fatalf("unknown inputs reported as measured: %#v", drivers)
	}
	raw, err := json.Marshal(drivers)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"private-title", "private-body", "private-issue-sentinel", testHubAdminToken, "prj_security"} {
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("cost drivers disclosed %s", sentinel)
		}
	}
	publisherID := "credential_capacity_artifact"
	binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "hosted", HostedOptIn: true, PublisherTokenID: publisherID}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: publisherID, Name: "Example publisher", Hash: apikey.HashToken(publisherID), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: publisherID, ProjectID: f.project}); err != nil {
			return err
		}
		return w.BindArtifactService(t.Context(), f.project, binding)
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name               string
		observed           time.Time
		known              bool
		retained, reserved int64
	}{
		{"unobserved", time.Time{}, false, 0, 0},
		{"measured zero", now.Add(-time.Second), true, 0, 0},
		{"measured usage", now, true, 7, 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RecordHostedArtifactUsage(t.Context(), binding.ServiceID, publisherID, artifact.Usage{ObservedAt: test.observed, RetainedBytes: test.retained, ReservedBytes: test.reserved}, now, f.service.database.hostedRuntime.Load().Plans.WindowSeconds)
			}); err != nil {
				t.Fatal(err)
			}
			report, err := f.service.readHostedMetadata(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			drivers := report.CostDrivers
			if (drivers.ArtifactRetainedBytes != nil) != test.known || (drivers.ArtifactReservedBytes != nil) != test.known {
				t.Fatalf("artifact measurement = %#v", drivers)
			}
			for _, metric := range []string{"artifact_retained_bytes", "artifact_reserved_bytes"} {
				if slices.Contains(report.UnavailableUsage, metric) == test.known {
					t.Fatalf("artifact availability = %#v", report.UnavailableUsage)
				}
			}
			if test.known && (*drivers.ArtifactRetainedBytes != test.retained || *drivers.ArtifactReservedBytes != test.reserved) {
				t.Fatalf("measured artifact usage = %#v, want retained=%d reserved=%d", drivers, test.retained, test.reserved)
			}
			fleet, err := f.service.hostedFleetUsageTenant(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range []string{"artifact_retained_bytes", "artifact_reserved_bytes"} {
				if _, limited := fleet.Allowances[metric]; limited || slices.Contains(fleet.Unavailable, metric) {
					t.Fatalf("unlimited artifact metric gained a fleet allowance: %s %+v", metric, fleet)
				}
			}
			if allowance, limited := fleet.Allowances["api_mutations"]; !limited || allowance.Limit != 1000000 {
				t.Fatalf("configured fleet allowance changed: %+v", fleet)
			}
			if slices.Contains(fleet.Unavailable, "history_records") || fleet.Allowances["history_records"].Used != 2 {
				t.Fatalf("initialized history population disappeared from fleet: %+v", fleet)
			}
		})
	}
}

func TestCapacityFreeConcurrentWorkQueues(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	d := f.service.database
	d.hostedOrganization = f.project.OrganizationID
	plans := hostedplans.CapacityPlans()
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	requests := map[string]tracker.NativeClaim{}
	claim := func(issue tracker.NativeIssue, machine string) *httptest.ResponseRecorder {
		if tokens[machine] == "" {
			tokens[machine], requests[machine] = hostedPlanWorkRequest(t, f, issue, machine)
		}
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", tokens[machine], requests[machine])
	}
	var leases [2]tracker.NativeLease
	for i := range leases {
		response := claim(f.create(t, fmt.Sprintf("Free work %d", i)), fmt.Sprintf("free-machine-%d", i))
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &leases[i])
	}
	queued := f.create(t, "Free queued work")
	for _, test := range []struct {
		name    string
		release bool
		status  int
	}{
		{"refused at the concurrent limit", false, http.StatusConflict},
		{"claimable after a slot frees", true, http.StatusOK},
	} {
		if test.release {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(leases[0].ID)+"/release", tokens["free-machine-0"], map[string]any{"fencing_token": fmt.Sprint(leases[0].FencingToken), "reason": "completed"}), http.StatusNoContent)
		}
		response := claim(queued, "free-machine-queued")
		requireNativeStatus(t, response, test.status)
		if test.status == http.StatusConflict && !strings.Contains(response.Body.String(), `"code":"plan_capacity"`) {
			t.Fatalf("%s: refusal=%s", test.name, response.Body.String())
		}
		item := readWorkItem(t, f, queued.WorkItemID, "")
		var leased int
		if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE i.organization_id=$1 AND i.native_id=$2 AND l.released_at IS NULL", f.project.OrganizationID, item.WorkItemID).Scan(&leased); err != nil {
			t.Fatal(err)
		}
		if item.State != "Todo" || item.Archived || (leased == 1) != (test.status == http.StatusOK) {
			t.Fatalf("%s: state=%s archived=%v leases=%d", test.name, item.State, item.Archived, leased)
		}
	}
}

type hostedConsumptionContextKey struct{}

type hostedConsumptionStatement struct {
	sql  string
	args []any
}

type hostedConsumptionQueries struct {
	mu         sync.Mutex
	statements []hostedConsumptionStatement
	resultRows int64
}

func (q *hostedConsumptionQueries) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.statements, q.resultRows = nil, 0
}

func (q *hostedConsumptionQueries) snapshot() ([]hostedConsumptionStatement, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.statements), q.resultRows
}

func (q *hostedConsumptionQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(hostedConsumptionContextKey{}) == q {
		q.mu.Lock()
		q.statements = append(q.statements, hostedConsumptionStatement{sql: data.SQL, args: slices.Clone(data.Args)})
		q.mu.Unlock()
	}
	return ctx
}

func (q *hostedConsumptionQueries) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(hostedConsumptionContextKey{}) == q {
		q.mu.Lock()
		q.resultRows += data.CommandTag.RowsAffected()
		q.mu.Unlock()
	}
}

func (*hostedConsumptionQueries) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (q *hostedConsumptionQueries) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(hostedConsumptionContextKey{}) == q {
		q.mu.Lock()
		q.statements = append(q.statements, hostedConsumptionStatement{sql: data.SQL, args: slices.Clone(data.Args)})
		q.resultRows += data.CommandTag.RowsAffected()
		q.mu.Unlock()
	}
}

func (*hostedConsumptionQueries) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func TestHostedNativeMutationConsumption(t *testing.T) {
	t.Parallel()
	var pinnedClock atomic.Pointer[time.Time]
	clock := func() time.Time {
		if pinned := pinnedClock.Load(); pinned != nil {
			return *pinned
		}
		return time.Now()
	}
	f := newNativeFixture(t, newPostgresNativeService(t, Config{now: clock}), "", "accounting")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.worker(t, "worker")
	d := f.service.database
	admin := nativeAttemptTestDatabase(t, f)
	queries := &hostedConsumptionQueries{}
	config := postgresStartupTemplate.NativeConfig(t, admin, f.service.config.Postgres.Connection.User, f.service.config.Postgres.Connection.Password)
	config.MaxConns = int32(f.service.config.Postgres.Pool.MaxConnections)
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, queries)
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, f.service.config.Postgres.Pool)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	originalTenant, originalAccess, originalBilling := f.service.config.TenantRepository, f.service.config.AccessRepository, d.billingRepository
	f.service.config.TenantRepository, f.service.config.AccessRepository = repository, repository
	d.billingRepository = repository
	t.Cleanup(func() {
		f.service.config.TenantRepository, f.service.config.AccessRepository, d.billingRepository = originalTenant, originalAccess, originalBilling
	})
	ctx := context.WithValue(t.Context(), hostedConsumptionContextKey{}, queries)
	budgets := map[string][2]int64{}
	expectedBudgets := map[string][2]int64{
		"full consumption":                       {35, 13},
		"Fleet limited retained data":            {33, 13},
		"Fleet unlimited retained data":          {31, 10},
		"Fleet next usage window":                {31, 7},
		"Fleet window overrides gauge":           {32, 10},
		"Fleet owner limited retained data":      {34, 13},
		"Fleet owner unlimited retained data":    {32, 10},
		"Fleet owner next usage window":          {32, 7},
		"Fleet owner window overrides gauge":     {33, 10},
		"metrics ordinary mutation":              {32, 11},
		"metrics ordinary completion":            {29, 8},
		"metrics run observation":                {27, 6},
		"metrics run completion":                 {25, 4},
		"metrics lease validation":               {25, 4},
		"metrics provider preview":               {25, 4},
		"metrics heartbeat":                      {27, 6},
		"metrics enrollment redemption":          {26, 5},
		"metrics runner mutation":                {24, 3},
		"metrics attachment preflight":           {4, 2},
		"metrics attachment growth":              {25, 4},
		"local new attempt":                      {98, 105},
		"local published replacement":            {96, 110},
		"local heartbeat replacement":            {94, 108},
		"local sequence replay":                  {53, 45},
		"local legacy event":                     {31, 23},
		"local accounting new attempt":           {27, 6},
		"local accounting published replacement": {27, 6},
		"local accounting heartbeat replacement": {27, 6},
		"local accounting sequence replay":       {27, 6},
		"local accounting legacy event":          {27, 6},
	}
	checkBudget := func(t *testing.T, key string) []hostedConsumptionStatement {
		t.Helper()
		statements, rows := queries.snapshot()
		actual := [2]int64{int64(len(statements)), rows}
		if actual[0] == 0 {
			t.Fatalf("%s PostgreSQL tracer observed no statements", key)
		}
		if expected, pinned := expectedBudgets[key]; !pinned || expected[0] < 0 || expected[1] < 0 {
			t.Errorf("unverified PostgreSQL budget %q: statements=%d rows=%d; pin after diagnostic execution", key, actual[0], rows)
		} else if actual != expected {
			t.Errorf("%s PostgreSQL budget=%v want=%v", key, actual, expected)
		}
		t.Logf("PostgreSQL budget %q: statements=%d rows=%d", key, actual[0], rows)
		if previous, exists := budgets[key]; exists && actual != previous {
			t.Fatalf("%s PostgreSQL work grew with retained history: before=%v after=%v", key, previous, actual)
		}
		budgets[key] = actual
		for _, captured := range statements {
			statement := captured.sql
			if strings.Contains(statement, "data_json") || strings.Contains(statement, "record_json") || strings.Contains(statement, "rowid") || strings.Contains(statement, "CAST(") {
				t.Fatalf("%s aggregated retained JSON: %s", key, statement)
			}
		}
		return statements
	}
	readConsumption := func(t *testing.T, at time.Time, metrics ...string) hostedplans.ConsumptionRecord {
		t.Helper()
		var result hostedplans.ConsumptionRecord
		if err := repository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			result, err = hubapi.FeatureResult(d.plansService().ReadConsumptionRecord(ctx, r, at, metrics...))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}

	recountBytes := func(t *testing.T) int64 {
		t.Helper()
		var total int64
		content := eventstore.NewContent(f.service.runDetailStore())
		rows, err := admin.QueryContext(t.Context(), `SELECT v.project_id,v.work_item_id,v.record_id,v.revision,v.payload_object_key,v.payload_sha256,v.payload_bytes,v.payload_content_bytes,
    EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=v.organization_id AND i.project_id=v.project_id AND i.native_id=v.record_id AND i.revision=v.revision),
    EXISTS(SELECT 1 FROM native_comments c WHERE c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.record_id AND c.revision=v.revision)
    FROM collaboration_versions v WHERE v.organization_id=$1`, f.project.OrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			version := eventstore.CollaborationVersion{OrganizationID: f.project.OrganizationID}
			var currentIssue, currentComment bool
			if err := rows.Scan(&version.ProjectID, &version.WorkItemID, &version.RecordID, &version.Revision, &version.PayloadObjectKey, &version.PayloadSHA256, &version.PayloadBytes, &version.PayloadContentBytes, &currentIssue, &currentComment); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			raw, err := content.CollaborationVersion(t.Context(), version)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			total += int64(len(raw))
			if currentIssue {
				var issue tracker.NativeIssue
				if err := json.Unmarshal(raw, &issue); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				labels, err := json.Marshal(issue.Labels)
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				assignees, err := json.Marshal(issue.Assignees)
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				total += int64(len(issue.Title) + len(issue.Body) + len(labels) + len(assignees))
			}
			if currentComment {
				var comment tracker.NativeComment
				if err := json.Unmarshal(raw, &comment); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				total += int64(len(comment.Body))
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		rows, err = admin.QueryContext(t.Context(), `SELECT project_id,work_item_id,id,sequence,type,schema_version,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,actor_kind,actor_principal_id,event_revision,from_state,to_state,run_id,attempt_id FROM collaboration_events WHERE organization_id=$1 AND type NOT LIKE 'run.%' AND type<>'scheduler.decision'`, f.project.OrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			event := eventstore.Event{OrganizationID: f.project.OrganizationID}
			if err := rows.Scan(&event.ProjectID, &event.WorkItemID, &event.ID, &event.Sequence, &event.Type, &event.SchemaVersion, &event.RecordedAt, &event.PayloadObjectKey, &event.PayloadSHA256, &event.PayloadBytes, &event.PayloadContentBytes, &event.ActorKind, &event.ActorPrincipalID, &event.EventRevision, &event.FromState, &event.ToState, &event.RunID, &event.AttemptID); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			value, err := content.Event(t.Context(), event)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			data, err := json.Marshal(value.Data)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			actor, err := json.Marshal(value.Actor)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			total += int64(len(data) + len(actor))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		var attachments int64
		if err := admin.QueryRowContext(t.Context(), `SELECT coalesce(sum(size),0) FROM attachments WHERE organization_id=$1 AND object_deleted_at IS NULL AND NOT customer_owned`, f.project.OrganizationID).Scan(&attachments); err != nil {
			t.Fatal(err)
		}
		var archives int64
		if err := admin.QueryRowContext(t.Context(), `SELECT coalesce(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1`, f.project.OrganizationID).Scan(&archives); err != nil {
			t.Fatal(err)
		}
		return total + attachments + archives
	}
	assertSelective := func(t *testing.T, statements []hostedConsumptionStatement, selected []string) {
		t.Helper()
		populationSources := map[string][]string{
			"members":  {"FROM hosted_members", "FROM hosted_member_reservations"},
			"projects": {"FROM projects"}, "repositories": {"FROM project_settings"},
			"registered_runners": {"FROM runner_identities"}, "connected_runners": {"FROM runner_identities"},
			"concurrent_work": {"FROM leases"}, "unarchived_issues": {"FROM issues"},
			"collaboration_bytes":     {"name='collaboration_bytes'"},
			"artifact_retained_bytes": {"FROM hosted_artifact_usage"}, "artifact_reserved_bytes": {"FROM hosted_artifact_usage"},
		}
		for _, captured := range statements {
			for metric, fragments := range populationSources {
				if slices.Contains(selected, metric) {
					continue
				}
				if metric == "registered_runners" && slices.Contains(selected, "connected_runners") || metric == "connected_runners" && slices.Contains(selected, "registered_runners") || metric == "artifact_retained_bytes" && slices.Contains(selected, "artifact_reserved_bytes") || metric == "artifact_reserved_bytes" && slices.Contains(selected, "artifact_retained_bytes") {
					continue
				}
				for _, fragment := range fragments {
					if strings.Contains(captured.sql, fragment) {
						t.Errorf("selected accounting probed unused %s: %s", metric, captured.sql)
					}
				}
			}
			if strings.Contains(captured.sql, "FROM hosted_usage_counters") {
				for _, arg := range captured.args {
					name, ok := arg.(string)
					if !ok {
						continue
					}
					metric := name
					if name == "retained_collaboration_events" {
						metric = "events_total"
					}
					if slices.Contains([]string{"retained_collaboration_events", "history_records"}, name) && !slices.Contains(selected, metric) {
						t.Errorf("selected accounting probed unused counter %s", name)
					}
				}
			}
		}
	}
	plans := hostedplans.PilotPlans()
	plans.Plans[0].Allowances = maps.Clone(plans.Plans[0].Allowances)
	plans.Plans[0].Allowances["concurrent_work"] = 10000
	plans.Plans[0].Allowances["collaboration_bytes"] = 1 << 30
	plans.Plans[0].Allowances["api_mutations"] = 1000000
	plans.Plans[0].Allowances["ingested_events"] = 1000000
	plans.Base = plans.Plans[0].PlanReference
	previous := 0
	for phase, retained := range []int{1, 32} {
		d.hostedRuntime.Store(nil)
		for i := previous; i < retained; i++ {
			issue := f.create(t, fmt.Sprintf("retained-%d", i))
			body := strings.Repeat("x", 32768) + issueContractTestSections
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("body-%d", i)}, ExpectedRevision: issue.Revision, Body: &body}), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", i)}, Body: body}), http.StatusOK)
			retainedWorker := f.worker(t, fmt.Sprintf("retained-worker-%d", i))
			lease := claimNativeAttempt(t, f, retainedWorker, fmt.Sprintf("machine-%d", i), fmt.Sprintf("session-%d", i), issue.WorkItemID)
			event := nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", retainedWorker, event), http.StatusOK)
		}
		previous = retained
		if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			assignment, err := w.BillingAssignment(t.Context())
			if err != nil {
				return err
			}
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: f.project.OrganizationID, Base: billingstore.PlanReference{ID: plans.Base.ID, Version: plans.Base.Version}}, assignment.Revision)
		}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Second).Add(time.Duration((phase+2)*2*int(plans.WindowSeconds)) * time.Second)
		window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
		if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			for _, usage := range []struct {
				metric string
				amount int64
			}{{"api_mutations", 7}, {"ingested_events", 3}} {
				result, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: f.project.OrganizationID, ID: fmt.Sprintf("seed-%d-%s", phase, usage.metric), WindowStart: window, Metric: usage.metric, Amount: usage.amount}, 10000)
				if err != nil {
					return err
				}
				if !result.Applied || !result.Allowed {
					t.Fatalf("window fixture was not applied: %+v", result)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		queries.reset()
		fullRecord := readConsumption(t, now)
		full := fullRecord.Usage
		if want := recountBytes(t); full["collaboration_bytes"] != want || want == 0 {
			t.Errorf("collaboration bytes counter=%d independent raw-source recount=%d", full["collaboration_bytes"], want)
		}
		fullStatements := checkBudget(t, "full consumption")
		if full["collaboration_bytes"] < int64(retained*3*32768) {
			t.Errorf("full queries=%d bytes=%d", len(fullStatements), full["collaboration_bytes"])
		}
		if !slices.Contains(fullRecord.Unavailable, "artifact_retained_bytes") || !slices.Contains(fullRecord.Unavailable, "artifact_reserved_bytes") {
			t.Fatalf("unobserved artifacts reported as measured: %+v", fullRecord)
		}
		var events, history int64
		if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM collaboration_events WHERE organization_id=$1), (SELECT count(*) FROM collaboration_events WHERE organization_id=$1)+(SELECT count(*) FROM collaboration_versions WHERE organization_id=$1)+(SELECT count(*) FROM native_attempt_events WHERE organization_id=$1)`, f.project.OrganizationID).Scan(&events, &history); err != nil {
			t.Fatal(err)
		}
		if events == 0 || history == 0 || full["events_total"] != events || full["history_records"] != history {
			t.Fatalf("typed retained populations=%v independent events=%d history=%d", full, events, history)
		}
		for index, test := range []struct {
			name       string
			allowances map[string]int64
			at         time.Duration
		}{
			{"limited retained data", map[string]int64{"collaboration_bytes": 1 << 30, "history_records": 10000, "api_mutations": 10000, "ingested_events": 10000}, 0},
			{"unlimited retained data", map[string]int64{"api_mutations": 0, "ingested_events": 0, "members": 10}, 0},
			{"next usage window", map[string]int64{"api_mutations": 0, "ingested_events": 0}, time.Duration(plans.WindowSeconds) * time.Second},
			{"window overrides gauge", map[string]int64{"projects": 10}, 0},
		} {
			t.Run(fmt.Sprintf("%d retained/Fleet %s", retained, test.name), func(t *testing.T) {
				plan := billingstore.Plan{OrganizationID: f.project.OrganizationID, PlanReference: billingstore.PlanReference{ID: fmt.Sprintf("fleet_projection_%d", phase), Version: int64(index + 1)}, Name: "Example projection", Allowances: test.allowances}
				var assignment billingstore.Assignment
				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					assignment, err = w.BillingAssignment(t.Context())
					if err != nil {
						return err
					}
					if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
						return err
					}
					return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: f.project.OrganizationID, Base: plan.PlanReference}, assignment.Revision)
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
					defer cancel()
					if err := repository.WriteTenant(cleanup, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						current, err := w.BillingAssignment(cleanup)
						if err != nil {
							return err
						}
						return w.SetBillingAssignment(cleanup, assignment, current.Revision)
					}); err != nil {
						t.Error(err)
					}
				})
				at := now.Add(test.at)
				if test.name == "window overrides gauge" {
					at = window.Add(16 * time.Duration(plans.WindowSeconds) * time.Second)
					if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						for metric, amount := range map[string]int64{"projects": 99, "api_mutations": 7, "ingested_events": 3} {
							result, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: f.project.OrganizationID, ID: fmt.Sprintf("gauge-%d-%s", phase, metric), WindowStart: at, Metric: metric, Amount: amount}, 1000)
							if err != nil {
								return err
							}
							if !result.Applied || !result.Allowed {
								t.Fatalf("gauge fixture was not applied: %+v", result)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				queries.reset()
				selected, err := hubapi.FeatureResult(d.plansService().ReadPlanUsage(ctx, nil, at, hostedplans.AllowanceNames()...))
				if err != nil {
					t.Fatal(err)
				}
				statements := checkBudget(t, "Fleet "+test.name)
				limited := []string{"usage_windows"}
				for metric := range selected.Allowances {
					limited = append(limited, metric)
				}
				assertSelective(t, statements, limited)
				queries.reset()
				pinnedClock.Store(&at)
				fleet, err := f.service.hostedPlanUsageTenant(ctx)
				pinnedClock.Store(nil)
				if err != nil {
					t.Fatal(err)
				}
				fleetStatements := checkBudget(t, "Fleet owner "+test.name)
				fleetMetrics := append(slices.Clone(limited), "artifact_retained_bytes", "artifact_reserved_bytes")
				assertSelective(t, fleetStatements, fleetMetrics)
				for metric := range fleet.Usage {
					if !slices.Contains(fleetMetrics, metric) && !slices.Contains([]string{"api_mutations", "ingested_events", "http_requests", "http_duration_microseconds", "http_request_bytes", "http_response_bytes", "http_request_bytes_known"}, metric) {
						t.Errorf("Fleet read unlimited metric %s", metric)
					}
				}
				authoritative, err := hubapi.FeatureResult(d.plansService().ReadPlanUsage(t.Context(), nil, at))
				if err != nil {
					t.Fatal(err)
				}
				if !maps.Equal(selected.Allowances, authoritative.Allowances) || !selected.WindowEndsAt.Equal(authoritative.WindowEndsAt) {
					t.Fatalf("projection changed entitlement: %#v", selected)
				}
				for name := range authoritative.Allowances {
					if slices.Contains(authoritative.Unavailable, name) {
						if !slices.Contains(selected.Unavailable, name) {
							t.Fatalf("unknown %s became measured", name)
						}
						continue
					}
					if selected.Usage[name] != authoritative.Usage[name] {
						t.Fatalf("%s=%d full=%d", name, selected.Usage[name], authoritative.Usage[name])
					}
				}
				for _, captured := range statements {
					statement := captured.sql
					if (!slices.Contains(slices.Collect(maps.Keys(selected.Allowances)), "members") && (strings.Contains(statement, "hosted_members") || strings.Contains(statement, "hosted_member_reservations"))) || strings.Contains(statement, "FROM leases") || strings.Contains(statement, "runner_identities") || strings.Contains(statement, "repositories") || strings.Contains(statement, "hosted_artifact_usage") || strings.Contains(statement, "count(*) FROM collaboration_events") {
						t.Fatalf("Fleet read unused metric: %s", statement)
					}
					if _, limited := selected.Allowances["history_records"]; !limited && strings.Contains(statement, "FROM hosted_usage_counters") && slices.ContainsFunc(captured.args, func(arg any) bool { name, ok := arg.(string); return ok && name == "history_records" }) {
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
			})
		}
		for _, test := range []struct {
			name    string
			metrics []string
		}{
			{"ordinary mutation", hostedNativeMutationMetrics(struct{}{}, false)},
			{"ordinary completion", hostedNativeMutationMetrics(struct{}{}, true)},
			{"run observation", hostedNativeMutationMetrics(tracker.NativeRunEvent{Type: "run.observed"}, false)},
			{"run completion", hostedNativeMutationMetrics(tracker.NativeRunEvent{Type: "run.finished"}, true)},
			{"lease validation", hostedRunnerTransactionMetrics(hubapi.NativeBase + "/leases/:lease/validate")},
			{"provider preview", hostedRunnerTransactionMetrics(hubapi.NativeBase + "/claims/preview")},
			{"heartbeat", hostedRunnerTransactionMetrics(hubapi.NativeBase + "/machines/:machine/heartbeat")},
			{"enrollment redemption", hostedRunnerTransactionMetrics(enrollmentBase + "/redeem")},
			{"runner mutation", hostedRunnerTransactionMetrics(runnerBase + "/:runner/rotate")},
			{"attachment preflight", []string{"collaboration_bytes"}},
			{"attachment growth", []string{"collaboration_bytes", "usage_windows"}},
		} {
			t.Run(fmt.Sprintf("%d retained/%s", retained, test.name), func(t *testing.T) {
				queries.reset()
				actualRecord := readConsumption(t, now, test.metrics...)
				actual := actualRecord.Usage
				statements := checkBudget(t, "metrics "+test.name)
				assertSelective(t, statements, test.metrics)
				for _, captured := range statements {
					if strings.Contains(captured.sql, "FROM hosted_usage_counters") {
						for _, arg := range captured.args {
							name, ok := arg.(string)
							if ok && slices.Contains([]string{"history_records", "retained_collaboration_events"}, name) {
								requested := name
								if name == "retained_collaboration_events" {
									requested = "events_total"
								}
								if !slices.Contains(test.metrics, requested) {
									t.Fatalf("selected read probed unused counter %s", name)
								}
							}
						}
						if strings.Contains(captured.sql, "name='collaboration_bytes'") && !slices.Contains(test.metrics, "collaboration_bytes") {
							t.Fatal("selected read probed unused collaboration bytes")
						}
					}
				}
				for name, amount := range actual {
					if full[name] != amount {
						t.Fatalf("%s=%d full=%d", name, amount, full[name])
					}
				}
				for _, name := range test.metrics {
					if name != "usage_windows" && !slices.Contains(actualRecord.Unavailable, name) && actual[name] != full[name] {
						t.Fatalf("omitted %s", name)
					}
				}
				if test.name == "lease validation" || test.name == "provider preview" || test.name == "heartbeat" {
					wantMetrics := 3
					if test.name == "heartbeat" {
						wantMetrics += 2
					}
					if actual["api_mutations"] != 7 || actual["ingested_events"] != 3 || len(actual) != wantMetrics {
						t.Fatalf("%s accounting=%v", test.name, actual)
					}
				}
				if test.name == "attachment preflight" && len(actual) != 1 {
					t.Fatalf("attachment accounting=%v", actual)
				}
				if test.name == "attachment growth" && (actual["api_mutations"] != 7 || len(actual) != 3) {
					t.Fatalf("attachment growth=%v", actual)
				}
				if test.name != "runner mutation" && test.name != "enrollment redemption" {
					for _, captured := range statements {
						statement := captured.sql
						if strings.Contains(statement, "hosted_artifact_usage") || strings.Contains(statement, "hosted_members") || strings.Contains(statement, "hosted_member_reservations") || strings.Contains(statement, "FROM leases") {
							t.Fatalf("unrelated report query: %s", statement)
						}
					}
				}
			})
		}
		for _, test := range []struct {
			name                             string
			unchanged, replay, legacy, start bool
		}{
			{name: "new attempt", start: true}, {name: "published replacement"}, {name: "heartbeat replacement", unchanged: true}, {name: "sequence replay", replay: true}, {name: "legacy event", legacy: true},
		} {
			t.Run(fmt.Sprintf("%d retained/local %s", retained, test.name), func(t *testing.T) {
				localWorker := f.worker(t, fmt.Sprintf("local-%d-%s", retained, test.name))
				issue := f.create(t, fmt.Sprintf("local accounting %d %s", retained, test.name))
				lease := claimNativeAttempt(t, f, localWorker, hubapi.NewID("machine"), hubapi.NewID("session"), issue.WorkItemID)
				event := nativeStartedEvent(lease)
				event.IdempotencyKey = "local-accounting"
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
				var expectedEvidence []tracker.NativeEvidence
				if test.unchanged {
					event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC().Add(-time.Second)}
				}
				if !test.start && !test.legacy {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, localWorker, event), http.StatusOK)
					if !test.replay {
						credential, status, err := f.service.authenticateAPIToken(t.Context(), localWorker, "", "")
						if err != nil || status != http.StatusOK {
							t.Fatalf("evidence credential status=%d error=%v", status, err)
						}
						scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
						evidence := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("upload"), LeaseID: lease.ID, FencingToken: lease.FencingToken}, WorkItemID: issue.WorkItemID, AttemptID: event.Data.AttemptID, Caption: "保留された証拠"}
						input := attachment.UploadRequest{Metadata: attachment.Metadata{Name: "retained.txt", ContentType: "text/plain", Size: 8, SHA256: artifact.Digest([]byte("retained"))}, RequestID: evidence.IdempotencyKey, Evidence: &evidence}
						prepared := httptest.NewRecorder()
						c := echo.New().NewContext(newHubAPIRequest(t, http.MethodPost, path, localWorker, nil).WithContext(t.Context()), prepared)
						c.Set("native_scope", scope)
						c.SetParamNames("attempt")
						c.SetParamValues(event.Data.AttemptID)
						if err := f.service.prepareCloudAttachmentTenant(c, input); err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, prepared, http.StatusOK)
						decodeHubResponse(t, prepared, &input.Metadata)
						completed := httptest.NewRecorder()
						c = echo.New().NewContext(newHubAPIRequest(t, http.MethodPost, path, localWorker, nil).WithContext(t.Context()), completed)
						c.Set("native_scope", scope)
						c.SetParamNames("attempt")
						c.SetParamValues(event.Data.AttemptID)
						if err := f.service.recordCloudAttachmentTenant(c, input); err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, completed, http.StatusCreated)
						expectedEvidence = []tracker.NativeEvidence{{AttachmentID: input.ID, Caption: "保留された証拠", Reference: input.Metadata.Markdown(string(f.project.OrganizationID))}}
						event.Type, event.Data.Sequence, event.IdempotencyKey = "run.observed", 2, "local-observation"
						event.Data.Runtime = &tracker.NativeRuntimeObservation{HeartbeatAt: time.Now().UTC(), Phase: "implementation"}
					}
				}
				if test.legacy {
					event.Data.AttemptID, event.Data.Sequence = hubapi.NewID("attempt"), 0
					event.IdempotencyKey = fmt.Sprintf("local-accounting-%d", retained)
				}
				metrics := hostedNativeMutationMetrics(event, false)
				before := readConsumption(t, now, metrics...).Usage
				var visibleBefore []eventstore.Event
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					visibleBefore, err = r.HistoryEvents(t.Context(), f.project.ID, issue.WorkItemID, 0, 200)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				queries.reset()
				if test.legacy {
					credential, status, err := f.service.authenticateAPIToken(t.Context(), localWorker, "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("historical credential status=%d error=%v", status, err)
					}
					scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
					actor, err := json.Marshal(scope.Actor())
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(tracker.CollaborationData{Run: &event.Data})
					if err != nil {
						t.Fatal(err)
					}
					fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "collaboration_events")
					fixture.add("collaboration_events", map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": issue.WorkItemID, "id": hubapi.NewID("event"), "sequence": 0, "type": event.Type, "schema_version": 1, "actor_json": string(actor), "data_json": string(data), "recorded_at": hubapi.FormatHubTime(time.Now().UTC())})
					source := CoreWorkItemImportSource{BundlePath: fixture.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
					if err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
						var row corestore.WorkItem
						if err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							row, err = r.WorkItem(ctx, scope.Project, issue.WorkItemID)
							return err
						}); err != nil {
							return err
						}
						record := postgres.CoreWorkItemImport{Item: row}
						if err := readCoreImportEvents(ctx, newCoreNeutralImport(bundle), eventstore.NewContent(source.Objects), source, row, &record, make(map[string][]byte)); err != nil {
							return err
						}
						if len(record.Events) != 1 {
							t.Fatal("verified historical fixture lost its event")
						}
						value, err := eventstore.NewContent(source.Objects).Event(ctx, record.Events[0])
						if err != nil {
							return err
						}
						if value.Data.Run == nil || value.Data.Run.Sequence != 0 || value.Data.Run.AttemptID != event.Data.AttemptID {
							t.Fatal("historical event acquired runtime sequence authority")
						}
						receipt, err := prepareCoreExecutionReceipt(ctx, eventstore.NewContent(source.Objects), scope.Organization, bundle.Identity().SourceSHA256, "legacy-accounting", event.IdempotencyKey, record.Events[0])
						if err != nil {
							return err
						}
						return repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							if err := f.service.authorizeAccessScope(ctx, w, scope, true); err != nil {
								return err
							}
							if _, err := w.AppendEvent(ctx, record.Events[0]); err != nil {
								return err
							}
							_, err := w.PutReceipt(ctx, receipt)
							return err
						})
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					request := newHubAPIRequest(t, http.MethodPost, path, localWorker, event).WithContext(ctx)
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
				}
				mutationStatements := checkBudget(t, "local "+test.name)
				for _, captured := range mutationStatements {
					statement := strings.ToLower(captured.sql)
					if (strings.Contains(statement, "count(") || strings.Contains(statement, "sum(")) && (strings.Contains(statement, "from collaboration_events") || strings.Contains(statement, "from native_attempts") || strings.Contains(statement, "from native_attempt_events")) {
						t.Fatalf("mutation recounted retained history: %s", captured.sql)
					}
				}
				queries.reset()
				after := readConsumption(t, now, metrics...).Usage
				accountingStatements := checkBudget(t, "local accounting "+test.name)
				for _, captured := range accountingStatements {
					statement := strings.ToLower(captured.sql)
					if strings.Contains(statement, "from collaboration_events") || strings.Contains(statement, "from native_attempts") || strings.Contains(statement, "from native_attempt_events") {
						t.Fatalf("unbounded local accounting read: %s", captured.sql)
					}
				}
				wantEvents, wantHistory := before["events_total"]+1, before["history_records"]+2
				if test.legacy {
					wantHistory = before["history_records"] + 1
				}
				if test.replay {
					wantEvents, wantHistory = before["events_total"], before["history_records"]
				}
				if after["events_total"] != wantEvents || after["history_records"] != wantHistory {
					t.Fatalf("local=%v want events=%d history=%d", after, wantEvents, wantHistory)
				}
				var events, history int64
				if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM collaboration_events WHERE organization_id=$1), (SELECT count(*) FROM collaboration_events WHERE organization_id=$1)+(SELECT count(*) FROM collaboration_versions WHERE organization_id=$1)+(SELECT count(*) FROM native_attempt_events WHERE organization_id=$1)`, f.project.OrganizationID).Scan(&events, &history); err != nil {
					t.Fatal(err)
				}
				if after["events_total"] != events || after["history_records"] != history {
					t.Fatalf("local=%v full events=%d history=%d", after, events, history)
				}
				all := readConsumption(t, now).Usage
				if want := recountBytes(t); all["collaboration_bytes"] != want || want == 0 {
					t.Errorf("local collaboration bytes counter=%d independent raw-source recount=%d", all["collaboration_bytes"], want)
				}
				full := readConsumption(t, now, metrics...).Usage
				if !maps.Equal(after, full) {
					t.Fatalf("local=%v full=%v", after, full)
				}
				var visibleAfter []eventstore.Event
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					visibleAfter, err = r.HistoryEvents(t.Context(), f.project.ID, issue.WorkItemID, 0, 200)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				wantVisible := len(visibleBefore) + 1
				if test.unchanged || test.replay {
					wantVisible = len(visibleBefore)
				}
				if len(visibleAfter) != wantVisible {
					t.Fatalf("local visible history=%d want=%d", len(visibleAfter), wantVisible)
				}
				if !test.legacy {
					attempt, err := f.service.readNativeAttemptTenant(t.Context(), nativeAttemptListingScope(t, f), string(issue.WorkItemID), event.Data.AttemptID)
					if err != nil || attempt.Sequence != event.Data.Sequence {
						t.Fatalf("persisted attempt=%+v error=%v", attempt, err)
					}
					if len(expectedEvidence) > 0 && !reflect.DeepEqual(attempt.Evidence, expectedEvidence) {
						t.Fatalf("replacement lost retained evidence: got=%+v want=%+v", attempt.Evidence, expectedEvidence)
					}
					credential, status, err := f.service.authenticateAPIToken(t.Context(), localWorker, "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("receipt credential status=%d error=%v", status, err)
					}
					if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						receipt, err := r.ReplayReceipt(t.Context(), credential.ID, "POST "+path, event.IdempotencyKey)
						if err == nil && (receipt.CommandKey != event.IdempotencyKey || receipt.OrganizationID != f.project.OrganizationID) {
							t.Fatal("mutation receipt lost its owner")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
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
		{name: "heartbeat API exhaustion uses the runner request budget", capacity: 2, limits: map[string]int64{"api_mutations": 0}, status: http.StatusOK},
		{name: "heartbeat active completion while exhausted", stale: true, active: true, capacity: 2, limits: map[string]int64{"connected_runners": 0, "api_mutations": 0}, status: http.StatusOK},
		{name: "preview API exhaustion uses the runner request budget", preview: true, capacity: 2, limits: map[string]int64{"api_mutations": 0}, status: http.StatusOK},
		{name: "preview unrelated exhaustion", preview: true, capacity: 2, limits: map[string]int64{"connected_runners": 0, "registered_runners": 0, "projects": 0, "repositories": 0, "unarchived_issues": 0, "collaboration_bytes": 1, "history_records": 0, "ingested_events": 0}, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "runner-quota")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			heartbeatHostedPlanWorker(t, f, r.redemption.Credential, r.binding.MachineID, r.redemption.Capacity)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "running", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}), http.StatusOK)
			}
			d := f.service.database
			if test.stale {
				stale := time.Now().Add(-time.Hour)
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND runner_id=$3", stale, f.project.OrganizationID, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET last_heartbeat_at=$1 WHERE organization_id=$2 AND id=$3", stale, f.project.OrganizationID, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			f.service.config.Hosted = &hostedplans.HostedConfig{}
			d.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, test.limits)
			observeHostedArtifactUsage(t, f.service)
			f.service.config.Hosted = nil
			before, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
			var body any = map[string]any{"display_name": "Example runner", "capacity": test.capacity, "version": "updated", "backend_isolation": r.redemption.BackendIsolation}
			if test.preview {
				path = f.base + "/claims/preview"
				body = tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID}}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, body), test.status)
			after, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				if !maps.Equal(before, after) {
					t.Fatalf("rejected operation changed usage: %v -> %v", before, after)
				}
				var version string
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT version FROM machines WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, r.binding.MachineID).Scan(&version); err != nil || version != "test" {
					t.Fatalf("rejected heartbeat version=%s error=%v", version, err)
				}
				return
			}
			if after["api_mutations"] != before["api_mutations"] || after["ingested_events"] != before["ingested_events"] || after["history_records"] != before["history_records"] || after["registered_runners"] != before["registered_runners"] {
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
		{"window exhaustion uses the runner request budget", map[string]int64{"api_mutations": 0}, "run.observed", http.StatusOK},
		{"failed completion while exhausted", map[string]int64{"collaboration_bytes": 1, "history_records": 0, "ingested_events": 0, "api_mutations": 0}, "run.finished", http.StatusOK},
		{"checkpoint while exhausted", map[string]int64{"collaboration_bytes": 1, "history_records": 0, "ingested_events": 0, "api_mutations": 0}, "run.checkpointed", http.StatusOK},
		{"start allowed", nil, "run.started", http.StatusOK},
		{"start storage exhaustion", map[string]int64{"collaboration_bytes": 1}, "run.started", http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "runtime-quota")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("runner authority status=%d error=%v", status, err)
			}
			heartbeatHostedPlanWorker(t, f, worker, credential.Runner.MachineID, 1)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: credential.Runner.MachineID, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			event := nativeStartedEvent(lease)
			if test.kind != "run.started" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			}
			f.service.config.Hosted = &hostedplans.HostedConfig{}
			f.service.database.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, test.limits)
			observeHostedArtifactUsage(t, f.service)
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
			before, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			var preparedBefore int64
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT coalesce(sum(compressed_size),0) FROM archive_objects WHERE organization_id=$1 AND NOT customer_owned", f.project.OrganizationID).Scan(&preparedBefore); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), test.status)
			after, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				var preparedAfter int64
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT coalesce(sum(compressed_size),0) FROM archive_objects WHERE organization_id=$1 AND NOT customer_owned", f.project.OrganizationID).Scan(&preparedAfter); err != nil {
					t.Fatal(err)
				}
				want := maps.Clone(before)
				want["collaboration_bytes"] += preparedAfter - preparedBefore
				if !maps.Equal(want, after) {
					t.Fatalf("rejected observation usage = %v, want %v including staged content", after, want)
				}
				return
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			replay, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, time.Now()))
			if err != nil || !maps.Equal(after, replay) {
				t.Fatalf("replay usage changed: %v -> %v error=%v", after, replay, err)
			}
		})
	}
}

func hostedPlanWorkRequest(t *testing.T, f nativeFixture, issue tracker.NativeIssue, session string) (string, tracker.NativeClaim) {
	t.Helper()
	token := f.worker(t, session)
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("runner authority status=%d error=%v", status, err)
	}
	heartbeatHostedPlanWorker(t, f, token, credential.Runner.MachineID, 1)
	return token, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: credential.Runner.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
}

func claimHostedPlanWork(t *testing.T, f nativeFixture, token string, request tracker.NativeClaim) (tracker.NativeLease, error) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, request)
	if response.Code != http.StatusOK {
		var refusal hubapi.APIErrorResponse
		decodeHubResponse(t, response, &refusal)
		return tracker.NativeLease{}, &hubapi.NativeError{Code: refusal.Code, Message: refusal.Message, Status: response.Code}
	}
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	return lease, nil
}

func exerciseHostedPlanUnrestrictedWork(t *testing.T, service *Service) {
	t.Helper()
	f := newNativeFixture(t, service, "", "unrestricted")
	issue := f.create(t, "Unrestricted work")
	token, request := hostedPlanWorkRequest(t, f, issue, "unrestricted")
	if _, err := claimHostedPlanWork(t, f, token, request); err != nil {
		t.Fatalf("unlimited claim = %v", err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := service.database.checkConversationCollaborationGrowthPostgres(t.Context(), w, "unrestricted-growth", 0, 1, service.config.now(), false); err != nil {
			return err
		}
		return hubapi.FromFeatureError(service.database.plansService().RequireFeatureRecord(t.Context(), w, "hosted_artifacts", service.config.now()))
	}); err != nil {
		t.Fatalf("unlimited growth or feature = %v", err)
	}
}

func heartbeatHostedPlanWorker(t *testing.T, f nativeFixture, token string, machine tracker.MachineID, capacity int) {
	t.Helper()
	now := f.service.config.now().UTC()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(machine)+"/heartbeat", token, map[string]any{
		"display_name": "Runner", "version": "test", "capacity": capacity,
		"backend_isolation":      isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}, "test": {isolation.Sandbox, isolation.NativeTrusted}},
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: now},
	})
	requireNativeStatus(t, response, http.StatusOK)
}
