package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func capacityReport(now time.Time) providercapacity.Report {
	return providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team", Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
}

func publishCapacity(t *testing.T, f nativeFixture, r runnerFixture, reports ...providercapacity.Report) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": reports,
			"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()}})
	requireNativeStatus(t, response, http.StatusOK)
}

func providerClaim(r runnerFixture, issue tracker.NativeIssue, session string) tracker.NativeClaim {
	return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "implement", Backend: "codex", Model: "test-model"}}}}
}

func TestProviderCapacityClaimObservations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*providercapacity.Report, *tracker.NativeClaim)
		want   int
		code   string
	}{
		{"available", func(*providercapacity.Report, *tracker.NativeClaim) {}, http.StatusOK, ""},
		{"multiple candidates retain selection", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.WorkItemID = ""
			missing := c.ProviderCandidates[0]
			missing.WorkItemID = "wi_missing"
			c.ProviderCandidates = append([]tracker.NativeCapacityCandidate{missing}, c.ProviderCandidates...)
		}, http.StatusOK, ""},
		{"unknown", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "unknown" }, http.StatusOK, ""},
		{"default report without account limit", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.MaxConcurrent, r.Availability = 0, "unknown"
		}, http.StatusOK, ""},
		{"stale exhaustion", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(-providercapacity.MaxAge)
		}, http.StatusOK, ""},
		{"future observation", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(time.Second)
		}, http.StatusOK, ""},
		{"exhausted", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "exhausted" }, http.StatusOK, ""},
		{"reset", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ResetAt = r.ObservedAt
			r.ObservedAt = r.ObservedAt.Add(-time.Minute)
		}, http.StatusOK, ""},
		{"explicit model unavailable", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = "expensive"
		}, http.StatusConflict, "provider_incompatible"},
		{"wrong backend", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Backend = "other"
		}, http.StatusConflict, "provider_incompatible"},
		{"changed issue", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates[0].Revision++ }, http.StatusConflict, "provider_candidate_changed"},
		{"missing requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates = nil }, http.StatusConflict, "provider_incompatible"},
		{"invalid requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = ""
		}, http.StatusUnprocessableEntity, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			clock := &leaseTestClock{value: now}
			f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: clock.Now}), "", "provider")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			r.reportCapacity(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			report := capacityReport(now)
			claim := providerClaim(r, issue, "work")
			test.change(&report, &claim)
			publishCapacity(t, f, r, report)
			if report.MaxConcurrent == 0 {
				for range 6 {
					clock.Advance(30 * time.Second)
					now = clock.Now()
					report.ObservedAt = now
					config := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now}
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{
						"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 2, "version": "test", "provider_reports": []providercapacity.Report{report}, "capacity_configuration": config,
					})
					requireNativeStatus(t, response, http.StatusOK)
					var view runnerauth.CapacityView
					response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity?backend=codex", testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &view)
					if view.Effective == nil || *view.Effective != 2 || view.Status != "applied" {
						t.Fatalf("refreshed default capacity=%+v", view)
					}
				}
			}
			assertRunnerAdmissionFacts(t, f, r)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, test.want)
			if test.code != "" {
				var refusal hubapi.NativeError
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != test.code {
					t.Fatalf("refusal = %s, want %s", refusal.Code, test.code)
				}
			}
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM provider_reservations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.want != http.StatusOK {
				if count != 0 {
					t.Fatal("failed claim retained reservation")
				}
				return
			}
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != issue.WorkItemID || count != 1 || lease.ProviderReservation == nil || lease.ProviderReservation.Model != "test-model" {
				t.Fatalf("lease = %+v", lease)
			}
			assertRunnerAdmissionFacts(t, f, r)
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var full runnerauth.Runner
			decodeHubResponse(t, response, &full)
			if len(full.Leases) != 1 || full.Leases[0].ID != lease.ID || full.Leases[0].Title != issue.Title || full.Leases[0].Policy.ID != hubTestPolicy().ID || full.Leases[0].ProviderReservation == nil || !reflect.DeepEqual(full.Leases[0].ProviderReservation, lease.ProviderReservation) {
				t.Fatalf("runner response lost lease enrichment: %+v", full.Leases)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var repeated tracker.NativeLease
			decodeHubResponse(t, response, &repeated)
			if repeated.ID != lease.ID || repeated.ProviderReservation.Model != lease.ProviderReservation.Model {
				t.Fatal("idempotent claim changed reservation")
			}
		})
	}
}

func TestProviderProjectBackendReservations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
	code := newNativeFixture(t, service, "", "code")
	otherCode := newNativeFixture(t, service, code.project.OrganizationID, "other-code")
	design := newNativeFixture(t, service, code.project.OrganizationID, "design")
	r := prepareRunnerForScope(t, code, "organization", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.redemption.Capacity = 3
	r.enroll(t)
	r.reportCapacity(t)
	reports := []providercapacity.Report{
		{Provider: "openai", Backend: "codex-code", AccountAlias: "runner", Models: []string{"sol", "code-model", "astra", "other-code-model", "provider_default"}, Availability: "unknown", ObservedAt: now},
		{Provider: "anthropic", Backend: "claude-design", AccountAlias: "runner", Models: []string{"opus", "design-model", "provider_default"}, Availability: "unknown", ObservedAt: now},
	}
	publishCapacity(t, code, r, reports...)
	for _, test := range []struct {
		fixture        nativeFixture
		backend, model string
	}{{code, "codex-code", "sol"}, {otherCode, "codex-code", "astra"}, {design, "claude-design", "opus"}} {
		t.Run(test.model, func(t *testing.T) {
			f := test.fixture
			approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			claim := providerClaim(r, issue, test.model)
			claim.ProviderCandidates[0].Requirement.Backend = test.backend
			claim.ProviderCandidates[0].Requirement.Model = test.model
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != issue.WorkItemID || lease.ProviderReservation == nil || lease.ProviderReservation.Backend != test.backend || lease.ProviderReservation.Model != test.model || lease.ProviderReservation.Report.Backend != test.backend || lease.ProviderReservation.Report.AccountAlias != "runner" || lease.ProviderReservation.Report.Availability != "unknown" {
				t.Fatalf("project reservation=%+v", lease)
			}
			assertRunnerAdmissionFacts(t, f, r)
		})
	}
	response := performHubAPIRequest(t, service, http.MethodGet, r.identityPath()+"/capacity", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var capacity runnerCapacityRead
	decodeHubResponse(t, response, &capacity)
	if len(capacity.ProviderCapacity) != 2 {
		t.Fatalf("capacity=%+v", capacity)
	}
	for _, view := range capacity.ProviderCapacity {
		wantUsed := 1
		if view.Backend == "codex-code" {
			wantUsed = 2
		}
		if view.Used != wantUsed || view.State != "unknown" || view.Reason == "" {
			t.Fatalf("project capacity observation=%+v", view)
		}
	}
}

func TestProviderSharedCapacityConcurrentRecovery(t *testing.T) {
	t.Parallel()
	for _, sharing := range []string{"declared", "subscription weekly exhausted", "subscription stale quota", "subscription unknown quota", "unknown", "same local alias", "mixed", "mixed reversed"} {
		t.Run(sharing, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			cfg := Config{Clock: func() time.Time { return now }}
			f := newNativeFixture(t, newClockedPostgresNativeService(t, cfg), "", "shared-provider")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			runners := make([]runnerFixture, 2)
			for i := range runners {
				runners[i] = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				runners[i].enroll(t)
				runners[i].reportCapacity(t)
				report := capacityReport(now)
				report.AccountAlias = fmt.Sprintf("local-%d", i)
				if strings.HasPrefix(sharing, "subscription") {
					used := 100.0
					report.Billing = "subscription"
					report.Availability = "exhausted"
					report.UsageWindows = []providercapacity.UsageWindow{{Kind: "weekly", PercentUsed: &used, ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour)}}
					if sharing == "subscription stale quota" {
						report.ObservedAt = now.Add(-providercapacity.MaxAge)
					}
					if sharing == "subscription unknown quota" {
						report.Availability = "unknown"
						report.UsageWindows = nil
					}
				}
				if sharing == "same local alias" {
					report.AccountAlias = "local"
				}
				if sharing == "unknown" || sharing == "same local alias" || sharing == "mixed" && i == 1 || sharing == "mixed reversed" && i == 0 {
					report.SharedAccountAlias = ""
				}
				publishCapacity(t, f, runners[i], report)
			}
			type result struct {
				status, runner int
				lease          tracker.NativeLease
			}
			outcomes := make(chan result, 8)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 8 {
				issue := f.create(t, fmt.Sprintf("work-%d", i))
				wg.Go(func() {
					<-start
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[i%2].redemption.Credential, providerClaim(runners[i%2], issue, fmt.Sprintf("session-%d", i)))
					outcome := result{status: response.Code, runner: i % 2}
					if response.Code == http.StatusOK {
						decodeHubResponse(t, response, &outcome.lease)
					}
					outcomes <- outcome
				})
			}
			close(start)
			wg.Wait()
			close(outcomes)
			var winner result
			count := 0
			for outcome := range outcomes {
				if outcome.status == http.StatusOK {
					count++
					winner = outcome
				} else if outcome.status != http.StatusConflict {
					t.Fatalf("status = %d", outcome.status)
				}
			}
			want := 2
			if sharing == "declared" || strings.HasPrefix(sharing, "subscription") {
				want = 1
			}
			if count != want {
				t.Fatalf("concurrent reservations = %d, want %d", count, want)
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = reopenPostgresNativeService(t, f.service)
			response := performHubAPIRequest(t, f.service, http.MethodGet, runners[0].base+"/runners", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var fleet []runnerauth.Runner
			decodeHubResponse(t, response, &fleet)
			for _, r := range fleet {
				if len(r.ProviderCapacity) != 1 || r.ProviderCapacity[0].Used != 1 {
					t.Fatalf("restart capacity = %+v", r.ProviderCapacity)
				}
			}
			blockedIssue := f.create(t, "blocked")
			claim := providerClaim(runners[1-winner.runner], blockedIssue, "blocked")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim), http.StatusConflict)
			now = now.Add(90 * time.Second)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var successor tracker.NativeLease
			decodeHubResponse(t, response, &successor)
			if successor.FencingToken <= winner.lease.FencingToken {
				t.Fatal("recovery reused fencing token")
			}
			path := f.base + "/leases/" + string(winner.lease.ID) + "/release"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runners[winner.runner].redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, Reason: "released"}), http.StatusConflict)
			path = f.base + "/leases/" + string(successor.ID) + "/release"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runners[1-winner.runner].redemption.Credential, tracker.NativeLeaseMutation{FencingToken: successor.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
			claim.SessionID = "after-cancel"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim), http.StatusOK)
		})
	}
}

func TestProviderQueueOrderAndSelectors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		states      []string
		priorities  []int
		order       []string
		labels      []string
		ranks       []string
		created     []string
		unavailable string
		want        []int
		winner      int
		targeted    bool
		byID        bool
	}{
		{name: "single provider candidate honors project rank and selectors", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{0, 1, 2}, want: []int{0, 1, 2}, winner: 1, targeted: true},
		{name: "legacy numeric order", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{0, 1, 2}, unavailable: "provider", want: []int{0, 1, 2}, winner: 1},
		{name: "reviewed merging beats higher priority implementation", states: []string{"Todo", "Merging", "Todo"}, priorities: []int{0, 3, 2}, order: []string{"Merging", "Rework", "In Progress", "Todo"}, want: []int{1, 0, 2}, winner: 1},
		{name: "merging lane lands before newer work of the same priority", states: []string{"Todo", "Merging", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo", "Merging"}, want: []int{1, 0, 2}, winner: 1},
		{name: "merging then rework precede configured lane preference", states: []string{"Merging", "Todo", "Rework"}, priorities: []int{1, 1, 1}, order: []string{"Todo", "Rework", "Merging"}, want: []int{0, 2, 1}},
		{name: "rework then in progress precede older todo", states: []string{"Todo", "In Progress", "Rework"}, priorities: []int{1, 1, 1}, order: []string{"Rework", "In Progress", "Todo"}, want: []int{2, 1, 0}, winner: 2},
		{name: "rework then in progress precede higher priority todo", states: []string{"Todo", "In Progress", "Rework"}, priorities: []int{0, 1, 2}, order: []string{"Rework", "In Progress", "Todo"}, want: []int{2, 1, 0}, winner: 2},
		{name: "rework precedes configured labels and older todo", states: []string{"Todo", "Rework", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Rework", "Todo"}, labels: []string{"hotfix", "bug"}, want: []int{1, 0, 2}, winner: 1},
		{name: "queue rank yields to age", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, ranks: []string{"c", "a", "b"}, want: []int{0, 1, 2}},
		{name: "precise creation order breaks unranked ties", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, ranks: []string{" ", "  ", "   "}, created: []string{"2026-10-02T12:00:00.000002Z", "2026-10-02T12:00:00Z", "2026-10-02T12:00:00.000001Z"}, want: []int{1, 2, 0}, winner: 1},
		{name: "equal creation times break ties by work item ID", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, ranks: []string{" ", " ", " "}, created: []string{"2026-10-02T12:00:00.000001Z", "2026-10-02T12:00:00.000001Z", "2026-10-02T12:00:00.000001Z"}, byID: true},
		{name: "unblocker count yields to age", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, unavailable: "unblocker", want: []int{0, 1}},
		{name: "retired red barrier does not hold merging", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{0, 1, 2}, order: []string{"Merging", "Todo"}, unavailable: "retired barrier", want: []int{0, 1, 2}},
		{name: "missing required review keeps merging unclaimable", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "required review", want: []int{1, 2}, winner: 1},
		{name: "unavailable merging provider falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{0, 1, 2}, order: []string{"Merging", "Todo"}, unavailable: "provider", want: []int{0, 1, 2}, winner: 1},
		{name: "changed merging revision falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{0, 1, 2}, order: []string{"Merging", "Todo"}, unavailable: "revision", want: []int{0, 1, 2}, winner: 1},
		{name: "unreviewed merging falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "review", want: []int{1, 2}, winner: 1},
		{name: "dependency held merging falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "dependency", want: []int{1, 2}, winner: 1},
		{name: "leased merging leaves next slot for todo", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "lease", want: []int{1, 2}, winner: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			r.reportCapacity(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			if test.targeted {
				higher := newNativeFixture(t, f.service, f.project.OrganizationID, "higher-ranked")
				approveHubTestPolicy(t, f.service, higher.base+"/policy", hubTestPolicy())
				patchLabels(t, higher, higher.create(t, "unrequested head"), "head-label", []string{"excluded"})
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=$1 THEN 0 ELSE 1 END WHERE organization_id=$2", higher.project.ID, f.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
				routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Provider runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID, higher.project.ID}}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			}
			states := []tracker.NativeState{
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Rework"}},
				{Name: "Rework", Dispatchable: true},
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				record.States = append(record.States, states...)
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: test.unavailable == "required review"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
			report := capacityReport(f.service.config.Clock())
			report.MaxConcurrent = 2
			if test.targeted {
				report.Models = append(report.Models, "gpt-6.1-sol")
			}
			publishCapacity(t, f, r, report)
			issues := []tracker.NativeIssue{f.create(t, "head"), f.create(t, "next"), f.create(t, "tail")}
			claim := providerClaim(r, issues[0], "fair")
			claim.WorkItemID, claim.ProviderCandidates = "", nil
			claim.DispatchPriorityByState, claim.DispatchPriorityByLabel = test.order, test.labels
			claim.WorkflowStates = []string{"Merging", "Rework", "In Progress", "Todo"}
			claim.PrioritizeUnblockers = test.unavailable == "unblocker"
			for i, issue := range issues {
				rank := strconv.Itoa(i)
				if test.ranks != nil {
					rank = test.ranks[i]
				}
				issue = writeSelectionFixtureRecord(t, f, issue.WorkItemID, func(w tenantstore.Writer, item *corestore.WorkItem, snapshot *tracker.NativeIssue) error {
					item.WorkflowStateName, snapshot.State = &test.states[i], test.states[i]
					if test.created != nil {
						created, err := hubapi.ParseTimeValue(test.created[i])
						if err != nil {
							return err
						}
						item.NativeCreatedAt, snapshot.CreatedAt = created, created
					}
					queue, err := w.QueueEntry(t.Context(), f.project.ID, issue.WorkItemID, string(f.project.ID))
					if err != nil {
						return err
					}
					queue.Priority, queue.Rank, queue.State = new(int64(test.priorities[i])), rank, test.states[i]
					if err := w.PutQueueEntry(t.Context(), queue); err != nil {
						return err
					}
					if test.labels != nil && i < len(test.labels) {
						snapshot.Labels = []string{test.labels[i]}
						return w.SetWorkItemLabels(t.Context(), f.project.ID, issue.WorkItemID, snapshot.Labels)
					}
					return nil
				})
				issues[i] = issue
				if test.created != nil {
					if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE issues SET native_created_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", issue.CreatedAt, f.project.OrganizationID, f.project.ID, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if test.states[i] == "Merging" && test.unavailable != "review" {
					path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Reviewed work"})
					requireNativeStatus(t, response, http.StatusOK)
					var change tracker.ChangeRequest
					decodeHubResponse(t, response, &change)
					cf := changeFixture{nativeFixture: f, issue: issue, change: change, path: path + "/" + change.ID}
					cf.publish(t, "version", "", true)
					wantReviewed := test.unavailable != "required review"
					if detail := cf.detail(t); (detail.Summary.Status == "reviewed") != wantReviewed {
						t.Fatalf("change readiness = %+v", detail.Summary)
					} else if !wantReviewed && detail.Summary.Status != "needs_evidence" {
						t.Fatalf("missing required review status = %s, want needs_evidence", detail.Summary.Status)
					}
				}
				candidate := providerClaim(r, issue, "").ProviderCandidates[0]
				if i == 0 && test.unavailable == "provider" {
					candidate.Requirement.Model = "unsupported"
				}
				if i == 0 && test.unavailable == "revision" {
					candidate.Revision++
				}
				claim.ProviderCandidates = append(claim.ProviderCandidates, candidate)
			}
			if test.byID {
				test.want = []int{0, 1, 2}
				slices.SortFunc(test.want, func(a, b int) int { return strings.Compare(string(issues[a].WorkItemID), string(issues[b].WorkItemID)) })
				test.winner = test.want[0]
			}
			if test.unavailable == "dependency" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: issues[0].WorkItemID, RelatedProjectID: f.project.ID, RelatedWorkItemID: issues[2].WorkItemID, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock())
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.unavailable == "unblocker" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: issues[2].WorkItemID, RelatedProjectID: f.project.ID, RelatedWorkItemID: issues[1].WorkItemID, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock())
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.unavailable == "lease" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issues[0], "running")), http.StatusOK)
			}
			if test.unavailable == "retired barrier" {
				var absent bool
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT to_regclass('public.landing_barriers') IS NULL`).Scan(&absent); err != nil || !absent {
					t.Fatalf("retired landing barrier still has durable authority: absent=%v error=%v", absent, err)
				}
			}
			var writes []string
			var statements int
			var writesMu sync.Mutex
			writeStatement := regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE|TRUNCATE)\b`)
			traced := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
				writesMu.Lock()
				defer writesMu.Unlock()
				statements++
				if writeStatement.MatchString(statement) {
					writes = append(writes, statement)
				}
			})
			previewRequest := func() *httptest.ResponseRecorder {
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: claim}).WithContext(traced)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				return response
			}
			for range 8 {
				requireNativeStatus(t, previewRequest(), http.StatusOK)
			}
			response := previewRequest()
			requireNativeStatus(t, response, http.StatusOK)
			writesMu.Lock()
			previewWrites, observedStatements := append([]string(nil), writes...), statements
			writesMu.Unlock()
			if observedStatements == 0 || len(previewWrites) != 0 {
				t.Fatalf("candidate previews statements=%d data writes=%v", observedStatements, previewWrites)
			}
			var page tracker.NativeCapacityPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != len(test.want) {
				t.Fatalf("preview count = %d, want %d", len(page.Items), len(test.want))
			}
			for i, index := range test.want {
				if page.Items[i].WorkItemID != issues[index].WorkItemID {
					t.Fatalf("preview[%d] = %s, want %s", i, page.Items[i].WorkItemID, issues[index].WorkItemID)
				}
			}
			preview := tracker.NativeCapacityPreview{NativeClaim: claim, Limit: 1}
			var paged []tracker.NativeWorkItemID
			for count := 0; ; count++ {
				if count > 4 {
					t.Fatal("bounded preview did not terminate")
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, preview)
				requireNativeStatus(t, response, http.StatusOK)
				var bounded tracker.NativeCapacityPage
				decodeHubResponse(t, response, &bounded)
				if len(bounded.Items) > 1 {
					t.Fatal("preview exceeded requested hydration budget")
				}
				for _, item := range bounded.Items {
					paged = append(paged, item.WorkItemID)
				}
				if bounded.Next == 0 {
					break
				}
				if bounded.Next == preview.After {
					t.Fatal("preview repeated cursor")
				}
				preview.After = bounded.Next
			}
			if len(paged) != len(test.want) {
				t.Fatalf("paged=%v want=%v", paged, test.want)
			}
			for index, want := range test.want {
				if paged[index] != issues[want].WorkItemID {
					t.Fatalf("paged=%v want=%v", paged, test.want)
				}
			}
			if test.targeted {
				claim.ProviderCandidates = claim.ProviderCandidates[test.winner : test.winner+1]
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusConflict)
				var refusal hubapi.NativeError
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != "no_claimable_work" {
					t.Fatalf("higher-ranked project refusal = %s, want no_claimable_work", refusal.Code)
				}
				claim.LabelExclude = []string{"excluded"}
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != issues[test.winner].WorkItemID {
				t.Fatalf("claim = %s, want %s", lease.WorkItemID, issues[test.winner].WorkItemID)
			}
			var leases, reservations int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE released_at IS NULL").Scan(&leases); err != nil {
				t.Fatal(err)
			}
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM provider_reservations").Scan(&reservations); err != nil {
				t.Fatal(err)
			}
			wantLeases := 1
			if test.unavailable == "lease" {
				wantLeases++
			}
			if leases != wantLeases || reservations != wantLeases {
				t.Fatalf("leases/reservations = %d/%d, want %d", leases, reservations, wantLeases)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			if test.unavailable == "lease" {
				return
			}
			descriptor := hubTestPolicy()
			descriptor.Requirements.MachineID = string(runnerauth.NewBinding().MachineID)
			descriptor = descriptor.WithID()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, map[string]any{"expected_policy_id": hubTestPolicy().ID, "policy": descriptor}), http.StatusOK)
			claim.PolicyID, claim.SessionID = descriptor.ID, "wrong-host"
			for _, path := range []string{"/claims", "/claims/preview"} {
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+path, r.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusConflict)
				var failure hubapi.NativeError
				decodeHubResponse(t, response, &failure)
				if failure.Code != "selector_no_match" {
					t.Fatalf("fixed host lost authority: %s", failure.Code)
				}
			}
		})
	}
}

func TestProviderPoolIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, provider, shared, authority, availability, leaseEnd               string
		otherOrganization, stale, offline, reserve, unbounded, currentUnbounded bool
		currentLocal, sameAlias, sameRunner, missingOwner                       bool
		used, bound, finalBound                                                 int
		state                                                                   string
	}{
		{name: "same pool", reserve: true, used: 1, bound: 2, state: "available"},
		{name: "local report does not affect explicit account", unbounded: true, shared: "unknown", reserve: true, availability: "unknown", bound: 4, state: "available"},
		{name: "explicit unbounded account ignores local cap", currentUnbounded: true, shared: "unknown", reserve: true, bound: 0, state: "unknown"},
		{name: "default reports use runner limits", currentUnbounded: true, unbounded: true, shared: "unknown", reserve: true, bound: 0, state: "unknown"},
		{name: "explicit independent account", shared: "independent", reserve: true, bound: 4, state: "available"},
		{name: "local sharing stays independent", shared: "unknown", reserve: true, bound: 4, state: "available"},
		{name: "same local alias on different runners", currentLocal: true, sameAlias: true, shared: "unknown", reserve: true, bound: 4, state: "available"},
		{name: "local account ignores explicit exhaustion", currentLocal: true, sameAlias: true, availability: "exhausted", reserve: true, bound: 4, state: "available"},
		{name: "explicit account ignores local exhaustion", shared: "unknown", availability: "exhausted", reserve: true, bound: 4, state: "available"},
		{name: "local account retains own reservations", currentLocal: true, sameAlias: true, sameRunner: true, shared: "unknown", reserve: true, used: 1, bound: 2, state: "available"},
		{name: "ownerless local reservation is not global", currentLocal: true, sameAlias: true, shared: "unknown", missingOwner: true, reserve: true, bound: 4, state: "available"},
		{name: "other provider", provider: "anthropic", reserve: true, bound: 4, state: "available"},
		{name: "other organization", otherOrganization: true, reserve: true, bound: 4, state: "available"},
		{name: "valid lower report", bound: 2, state: "available"},
		{name: "valid offline authority", offline: true, bound: 2, state: "available"},
		{name: "valid exhaustion", availability: "exhausted", bound: 2, state: "exhausted"},
		{name: "valid unknown", availability: "unknown", bound: 2, state: "unknown"},
		{name: "valid stale report", stale: true, availability: "exhausted", bound: 2, state: "unknown"},
		{name: "expired historical report", authority: "expired", stale: true, offline: true, bound: 4, state: "available"},
		{name: "expired exhaustion", authority: "expired", availability: "exhausted", bound: 4, state: "available"},
		{name: "revoked exhaustion", authority: "revoked", availability: "exhausted", bound: 4, state: "available"},
		{name: "future authority", authority: "future", bound: 4, state: "available"},
		{name: "invalid expiry", authority: "invalid", bound: 4, state: "available"},
		{name: "missing expiry is non-expiring authority", authority: "missing", bound: 2, state: "available"},
		{name: "same alias on another owner does not supersede pinned shared bound", sameAlias: true, authority: "expired", reserve: true, leaseEnd: "expiry", used: 1, bound: 2, finalBound: 4, state: "available"},
		{name: "expired pinned lease until expiry", authority: "expired", reserve: true, leaseEnd: "expiry", used: 1, bound: 2, finalBound: 4, state: "available"},
		{name: "revoked pinned lease until expiry", authority: "revoked", reserve: true, leaseEnd: "expiry", used: 1, bound: 2, finalBound: 4, state: "available"},
		{name: "fresh report supersedes pinned lease bound before release", authority: "expired", reserve: true, leaseEnd: "release", used: 1, bound: 4, finalBound: 4, state: "available"},
		{name: "draining authority and lease", authority: "draining", reserve: true, leaseEnd: "release", used: 1, bound: 2, finalBound: 2, state: "available"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }}), "", "provider-authority")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			current := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			current.enroll(t)
			current.reportCapacity(t)
			otherFixture := f
			if test.otherOrganization {
				otherFixture = newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "other-provider")
			}
			otherRunner := prepareRunner(t, otherFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			otherRunner.enroll(t)
			otherRunner.reportCapacity(t)
			report := capacityReport(now)
			report.MaxConcurrent = 4
			if test.currentLocal {
				report.SharedAccountAlias = ""
			}
			if test.sameRunner {
				otherRunner = current
			}
			if test.currentUnbounded {
				report.MaxConcurrent, report.Availability = 0, "unknown"
			}
			if test.leaseEnd == "release" {
				report.MaxConcurrent = 2
			}
			publishCapacity(t, f, current, report)
			other := capacityReport(now)
			other.AccountAlias, other.MaxConcurrent = "older-runner", 2
			if test.sameAlias {
				other.AccountAlias = report.AccountAlias
			}
			if test.unbounded {
				other.MaxConcurrent = 0
			}
			if test.stale {
				other.ObservedAt = now.Add(-48 * time.Hour)
			}
			if test.provider != "" {
				other.Provider = test.provider
			}
			if test.shared != "" {
				other.SharedAccountAlias = test.shared
				if test.shared == "unknown" {
					other.SharedAccountAlias = ""
				}
			}
			publishCapacity(t, otherFixture, otherRunner, other)
			var lease tracker.NativeLease
			if test.reserve {
				owner := otherRunner
				if test.leaseEnd == "release" {
					owner = current
				}
				issue := owner.create(t, "reserved")
				response := performHubAPIRequest(t, f.service, http.MethodPost, owner.nativeFixture.base+"/claims", owner.redemption.Credential, providerClaim(owner, issue, "reserved"))
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &lease)
				if test.missingOwner {
					if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM runner_lease_assignments WHERE lease_id=$1", lease.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.leaseEnd == "release" {
				report.MaxConcurrent = 4
				publishCapacity(t, f, current, report)
			}
			if test.availability != "" {
				other.Availability = test.availability
			}
			publishCapacity(t, otherFixture, otherRunner, other)
			if test.offline {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = $1 WHERE runner_id= $2", now.Add(-48*time.Hour), otherRunner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			var statement string
			switch test.authority {
			case "expired":
				statement = "UPDATE api_tokens SET expires_at = $1 WHERE id = $2"
			case "future":
				statement = "UPDATE api_tokens SET created_at = $1 WHERE id = $2"
			case "invalid":
				statement = "UPDATE api_tokens SET expires_at = 'invalid' WHERE id = $1"
			case "missing":
				statement = "UPDATE api_tokens SET expires_at = NULL WHERE id = $1"
			case "revoked":
				statement = "UPDATE api_tokens SET revoked_at = $1 WHERE id = $2"
			case "draining":
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET state = 'draining' WHERE runner_id= $1", otherRunner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if statement != "" {
				args := []any{otherRunner.binding.RunnerID}
				switch test.authority {
				case "expired", "revoked":
					args = []any{now, otherRunner.binding.RunnerID}
				case "future":
					args = []any{now.Add(time.Second), otherRunner.binding.RunnerID}
				}
				_, mutationErr := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), statement, args...)
				if test.authority == "invalid" {
					var pgError *pgconn.PgError
					if !errors.As(mutationErr, &pgError) || pgError.Code != "22007" {
						t.Fatalf("malformed typed expiry error=%v", mutationErr)
					}
					test.bound = other.MaxConcurrent
				} else if mutationErr != nil {
					t.Fatal(mutationErr)
				}
			}
			readerCredential, readerStatus, readerErr := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if readerErr != nil {
				t.Fatalf("provider reader status=%d error=%v", readerStatus, readerErr)
			}
			capacity, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: readerCredential}, current.binding.RunnerID)
			if err != nil || len(capacity.ProviderCapacity) != 1 {
				t.Fatalf("provider routing=%+v error=%v", capacity, err)
			}
			view := capacity.ProviderCapacity[0]
			if view.Used != test.used || view.MaxConcurrent != test.bound || view.State != test.state {
				t.Fatalf("view = %+v, %v; want used=%d bound=%d state=%s", view, err, test.used, test.bound, test.state)
			}
			if test.leaseEnd != "" {
				if test.leaseEnd == "release" {
					path := f.base + "/leases/" + string(lease.ID) + "/release"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, current.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				} else {
					now = lease.ExpiresAt
				}
				capacity, err = f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: readerCredential}, current.binding.RunnerID)
				if err != nil || len(capacity.ProviderCapacity) != 1 {
					t.Fatalf("ended provider routing=%+v error=%v", capacity, err)
				}
				view = capacity.ProviderCapacity[0]
				if view.Used != 0 || view.MaxConcurrent != test.finalBound {
					t.Fatalf("ended lease view = %+v, %v; want used=0 bound=%d", view, err, test.finalBound)
				}
			}
			stored, err := []providercapacity.Report(nil), error(nil)
			err = f.service.config.TenantRepository.ReadTenant(t.Context(), otherFixture.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
				var readErr error
				stored, readErr = reader.RunnerProviderReports(t.Context(), otherRunner.binding.RunnerID)
				return readErr
			})
			expectedReport := other
			if expectedReport.Billing == "" {
				expectedReport.Billing = "unknown"
			}
			if err != nil || len(stored) != 1 || !bytes.Equal(historyTestJSON(t, stored[0]), historyTestJSON(t, expectedReport)) {
				t.Fatalf("historical report changed: %+v, %v", stored, err)
			}
		})
	}
}

func TestProviderPreviewAnswersWhileTheWriterIsHeld(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	now := f.service.config.Clock()
	report := capacityReport(now)
	report.MaxConcurrent = 2
	publishCapacity(t, f, r, report)
	issues := []tracker.NativeIssue{f.create(t, "head"), f.create(t, "next"), f.create(t, "tail")}
	repository := f.service.database.postgres
	for index, issue := range issues {
		if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			entry, err := w.QueueEntry(t.Context(), f.project.ID, issue.WorkItemID, string(f.project.ID))
			if err != nil {
				return err
			}
			entry.Priority = new(int64(index))
			entry.Rank = strconv.Itoa(index)
			return w.PutQueueEntry(t.Context(), entry)
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim := providerClaim(r, issues[0], "fair")
	claim.WorkItemID, claim.ProviderCandidates = "", nil
	claim.WorkflowStates = []string{"Merging", "Rework", "In Progress", "Todo"}
	for _, issue := range issues {
		claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, issue, "").ProviderCandidates[0])
	}
	finding := newHealthFinding("scheduler_loop_behind", "instance", "project", string(f.project.ID), "Scheduler refresh is behind.", "Check scheduler refresh health.", []string{string(f.project.ID)}, healthEvidence{})
	if err := f.service.publishHealthEvaluation(t.Context(), f.project.OrganizationID, now, []healthFinding{finding}); err != nil {
		t.Fatal(err)
	}
	var original postgres.HealthRecord
	if err := repository.WriteHealth(t.Context(), f.project.OrganizationID, corestore.Background, func(w postgres.HealthWriter) error {
		var err error
		original, err = w.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: f.project.OrganizationID, Fingerprint: finding.Fingerprint, ResolvedAt: new(now.Add(-healthReopenWindow))})
		if err != nil {
			return err
		}
		return w.LinkHealthIssue(t.Context(), postgres.LinkHealthIssueInput{OrganizationID: f.project.OrganizationID, FindingID: original.ID, ProjectID: f.project.ID, WorkItemID: string(issues[0].WorkItemID)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.publishHealthEvaluation(t.Context(), f.project.OrganizationID, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	readHealth := func() (postgres.HealthRecord, []postgres.HealthIssueLink) {
		t.Helper()
		var row postgres.HealthRecord
		var links []postgres.HealthIssueLink
		err := repository.ReadHealth(t.Context(), f.project.OrganizationID, corestore.Serving, func(r postgres.HealthReader) error {
			var err error
			row, err = r.HealthRecord(t.Context(), postgres.HealthRecordInput{OrganizationID: f.project.OrganizationID, ID: original.ID})
			if err != nil {
				return err
			}
			links, err = r.HealthRecordIssues(t.Context(), postgres.HealthRecordIssuesInput{OrganizationID: f.project.OrganizationID, FindingID: original.ID, Limit: 2})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return row, links
	}
	beforeHealth, beforeLinks := readHealth()
	if beforeHealth.ResolvedAt == nil || !beforeHealth.ResolvedAt.Equal(now.Add(time.Minute)) || len(beforeLinks) != 1 || beforeLinks[0].WorkItemID != string(issues[0].WorkItemID) {
		t.Fatalf("resolved linked finding=%+v links=%+v", beforeHealth, beforeLinks)
	}
	f.service.tokenUseWork.Wait()
	admin := usagePostgresAdmin(t, f.service)
	writer, err := pgx.ConnectConfig(t.Context(), postgresStartupTemplate.NativeConfig(t, admin, "", "").ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := writer.Begin(t.Context())
	if err != nil {
		closeContext, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
		defer cancel()
		_ = writer.Close(closeContext)
		t.Fatal(err)
	}
	var release sync.Once
	releaseWriter := func() {
		t.Helper()
		release.Do(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
			defer cancel()
			if err := hold.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(err)
			}
			if err := writer.Close(cleanup); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(releaseWriter)
	if _, err := hold.Exec(pgtest.UnboundProbe(t.Context()), "LOCK TABLE health_findings IN SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	writeStatement := regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE|TRUNCATE)\b`)
	var traceMu sync.Mutex
	var statements int
	var writes []string
	traced := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
		traceMu.Lock()
		defer traceMu.Unlock()
		statements++
		if writeStatement.MatchString(statement) {
			writes = append(writes, statement)
		}
	})
	preview := func(input tracker.NativeCapacityPreview) *httptest.ResponseRecorder {
		t.Helper()
		ctx, cancel := context.WithCancel(traced)
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, input).WithContext(ctx)
		response := httptest.NewRecorder()
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			f.service.Handler().ServeHTTP(response, request)
		}()
		t.Cleanup(func() {
			cancel()
			releaseWriter()
			cleanup, stop := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
			defer stop()
			select {
			case <-finished:
			case <-cleanup.Done():
				t.Error("preview did not exit after writer release and request cancellation")
			}
		})
		select {
		case <-finished:
			cancel()
			return response
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("claim preview waited on the held writer")
			return nil
		}
	}
	var response *httptest.ResponseRecorder
	for range 9 {
		response = preview(tracker.NativeCapacityPreview{NativeClaim: claim})
		requireNativeStatus(t, response, http.StatusOK)
	}
	var page tracker.NativeCapacityPage
	decodeHubResponse(t, response, &page)
	if len(page.Items) != len(issues) {
		t.Fatalf("preview count=%d want=%d", len(page.Items), len(issues))
	}
	for index, issue := range issues {
		if page.Items[index].WorkItemID != issue.WorkItemID {
			t.Fatalf("preview[%d]=%s want=%s", index, page.Items[index].WorkItemID, issue.WorkItemID)
		}
	}
	paged := tracker.NativeCapacityPreview{NativeClaim: claim, Limit: 1}
	var ids []tracker.NativeWorkItemID
	for count := 0; ; count++ {
		if count > 4 {
			t.Fatal("bounded preview did not terminate")
		}
		response := preview(paged)
		requireNativeStatus(t, response, http.StatusOK)
		var next tracker.NativeCapacityPage
		decodeHubResponse(t, response, &next)
		if len(next.Items) > 1 {
			t.Fatal("preview exceeded requested hydration budget")
		}
		for _, item := range next.Items {
			ids = append(ids, item.WorkItemID)
		}
		if next.Next == 0 {
			break
		}
		if next.Next == paged.After {
			t.Fatal("preview repeated cursor")
		}
		paged.After = next.Next
	}
	if len(ids) != len(issues) {
		t.Fatalf("paged=%v want=%v", ids, issues)
	}
	for index, issue := range issues {
		if ids[index] != issue.WorkItemID {
			t.Fatalf("paged[%d]=%s want=%s", index, ids[index], issue.WorkItemID)
		}
	}
	f.service.tokenUseWork.Wait()
	traceMu.Lock()
	readStatements, dataWrites := statements, append([]string(nil), writes...)
	traceMu.Unlock()
	if readStatements == 0 || len(dataWrites) != 0 {
		t.Fatalf("preview statements=%d data writes=%v", readStatements, dataWrites)
	}
	afterHealth, afterLinks := readHealth()
	if !reflect.DeepEqual(afterHealth, beforeHealth) || !reflect.DeepEqual(afterLinks, beforeLinks) {
		t.Fatalf("preview mutated resolved finding or links: %+v %+v", afterHealth, afterLinks)
	}
	releaseWriter()
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != issues[0].WorkItemID {
		t.Fatalf("claim=%s want=%s", lease.WorkItemID, issues[0].WorkItemID)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		capacity, err := reader.ClaimCapacityUsage(t.Context(), string(r.binding.MachineID), r.binding.RunnerID, f.project.ID, "", f.service.config.Clock())
		if err != nil {
			return err
		}
		used, err := reader.ProviderReservationUsage(t.Context(), report, r.binding.RunnerID, "", f.service.config.Clock())
		if err != nil {
			return err
		}
		reservation, err := reader.ProviderReservation(t.Context(), lease.ID)
		if err != nil {
			return err
		}
		if capacity.Host != 1 || capacity.Runner != 1 || capacity.Project != 1 || used != 1 || reservation.LeaseID != lease.ID || reservation.OrganizationID != f.project.OrganizationID {
			t.Fatalf("leases/reservations=%+v/%d reservation=%+v", capacity, used, reservation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	descriptor := hubTestPolicy()
	descriptor.Requirements.MachineID = string(runnerauth.NewBinding().MachineID)
	descriptor = descriptor.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, map[string]any{"expected_policy_id": hubTestPolicy().ID, "policy": descriptor}), http.StatusOK)
	claim.PolicyID, claim.SessionID = descriptor.ID, "wrong-host"
	for _, path := range []string{"/claims", "/claims/preview"} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+path, r.redemption.Credential, claim)
		requireNativeStatus(t, response, http.StatusConflict)
		var failure hubapi.NativeError
		decodeHubResponse(t, response, &failure)
		if failure.Code != "selector_no_match" {
			t.Fatalf("fixed host lost authority: %s", failure.Code)
		}
	}
}

func TestProviderUsageHeartbeatRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }}), "", "usage")
	var organizationBase string
	for index, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			organizationBase = r.base
			report := capacityReport(now)
			report.Backend = backend
			report.Billing = []string{"subscription", "api_key"}[index]
			percent := []float64{85, 40}[index]
			report.UsageWindows = []providercapacity.UsageWindow{{Kind: "five_hour", PercentUsed: &percent, ObservedAt: now.Add(-30 * time.Second), ResetAt: now.Add(time.Hour)}, {Kind: "weekly"}}
			publishCapacity(t, f, r, report)
			check := func(want []providercapacity.UsageWindow) {
				t.Helper()
				routingCredential0, routingStatus0, routingError0 := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
				if routingError0 != nil {
					t.Fatalf("routing reader status=%d error=%v", routingStatus0, routingError0)
				}
				stored, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential0}, r.binding.RunnerID)
				if err != nil || len(stored.ProviderCapacity) != 1 || !reflect.DeepEqual(stored.ProviderCapacity[0].UsageWindows, want) || stored.ProviderCapacity[0].Billing != report.BillingBasis() {
					t.Fatalf("runner = %+v, err = %v, want windows = %+v", stored.ProviderCapacity, err, want)
				}
				reports, err := []providercapacity.Report(nil), error(nil)
				err = f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var readErr error
					reports, readErr = reader.RunnerProviderReports(t.Context(), r.binding.RunnerID)
					return readErr
				})
				if err != nil || len(reports) != 1 || !reflect.DeepEqual(reports[0].UsageWindows, want) || reports[0].Billing != stored.ProviderCapacity[0].Billing {
					t.Fatalf("typed provider rows lost usage or billing: %+v, %v", reports, err)
				}
			}
			check(report.UsageWindows)
			response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var capacity runnerCapacityRead
			decodeHubResponse(t, response, &capacity)
			if len(capacity.ProviderCapacity) != 1 || !reflect.DeepEqual(capacity.ProviderCapacity[0].UsageWindows, report.UsageWindows) || capacity.ProviderCapacity[0].Billing != report.Billing {
				t.Fatalf("capacity API = %+v", capacity)
			}
			report.ObservedAt = now.Add(time.Second)
			now = now.Add(time.Second)
			publishCapacity(t, f, r, report)
			check(report.UsageWindows)
			malformed := report
			malformed.Billing = "invalid"
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []providercapacity.Report{malformed}})
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			check(report.UsageWindows)
			malformed = report
			malformed.UsageWindows = []providercapacity.UsageWindow{{Kind: "private-account@example.com"}}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []providercapacity.Report{malformed}})
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			check(report.UsageWindows)
			savedBilling := report.Billing
			report.Billing = ""
			publishCapacity(t, f, r, report)
			check(report.UsageWindows)
			report.Billing = savedBilling
			publishCapacity(t, f, r, report)
			check(report.UsageWindows)
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, organizationBase+"/runners", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var fleet []runnerauth.Runner
	decodeHubResponse(t, response, &fleet)
	if len(fleet) != 2 {
		t.Fatalf("API runners = %d, want 2", len(fleet))
	}
	for _, r := range fleet {
		if len(r.ProviderCapacity) != 1 || len(r.ProviderCapacity[0].UsageWindows) != 2 || r.ProviderCapacity[0].UsageWindows[0].PercentUsed == nil {
			t.Fatalf("API capacity = %+v", r.ProviderCapacity)
		}
		want := 85.0
		if r.ProviderCapacity[0].Backend == "codex" {
			want = 40
		}
		wantBilling := "subscription"
		if r.ProviderCapacity[0].Backend == "codex" {
			wantBilling = "api_key"
		}
		if r.ProviderCapacity[0].Billing != wantBilling {
			t.Fatalf("API billing = %q, want %q", r.ProviderCapacity[0].Billing, wantBilling)
		}
		if *r.ProviderCapacity[0].UsageWindows[0].PercentUsed != want {
			t.Fatalf("API usage = %+v, want %v", r.ProviderCapacity, want)
		}
	}
	runners, err := usagePostgresAdmin(t, f.service).QueryContext(t.Context(), `SELECT runner_id, count(*) FROM runner_provider_usage_windows GROUP BY runner_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer runners.Close()
	count := 0
	for runners.Next() {
		var id string
		var windows int
		if err := runners.Scan(&id, &windows); err != nil {
			t.Fatal(err)
		}
		if windows != 2 {
			t.Fatalf("runner %s has %d windows", id, windows)
		}
		count++
	}
	if err := runners.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stored runners = %d, want 2", count)
	}
}

func TestProviderCapacityReportDisappearance(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"empty", "omitted"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }}), "", "provider")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			publishCapacity(t, f, r, capacityReport(now))
			payload := map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test"}
			if state == "empty" {
				payload["provider_reports"] = []providercapacity.Report{}
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload)
			requireNativeStatus(t, response, http.StatusOK)
			reports, err := []providercapacity.Report(nil), error(nil)
			err = f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
				var readErr error
				reports, readErr = reader.RunnerProviderReports(t.Context(), r.binding.RunnerID)
				return readErr
			})
			if state == "omitted" {
				if err != nil || len(reports) != 1 || !reports[0].ObservedAt.Equal(capacityReport(now).ObservedAt) {
					t.Fatalf("omitted provider reports must stay unchanged without renewing their observation: %+v, err=%v", reports, err)
				}
				now = now.Add(providercapacity.MaxAge)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload), http.StatusOK)
			} else if err != nil || len(reports) != 0 {
				t.Fatalf("reports after disappearance=%+v, err=%v", reports, err)
			}
			claim := providerClaim(r, issue, "empty")
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusConflict)
			claim.ProviderCandidates = nil
			claim.Capabilities = append(claim.Capabilities, tracker.NativeProviderCapacityCapability)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusConflict)
		})
	}
}

func patchLabels(t *testing.T, f nativeFixture, issue tracker.NativeIssue, key string, labels []string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token,
		tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, Labels: &labels})
	requireNativeStatus(t, response, http.StatusOK)
	var updated tracker.NativeIssue
	decodeHubResponse(t, response, &updated)
	return updated
}

func TestProviderPhaseSwapIsTenantScoped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
	first := newNativeFixture(t, service, "", "planning")
	second := newNativeFixture(t, service, first.project.OrganizationID+"_second", "planning")
	pinned := hubTestPolicy()
	pinned.Gates.PlanEnabled = true
	pinned.Gates.PlanReview = "automated"
	pinned = pinned.WithID()
	plan := providercapacity.Requirement{Role: "plan", Backend: "planner", Model: "test-model"}
	code := providercapacity.Requirement{Role: "code", Backend: "codex", Model: "test-model"}
	type owner struct {
		runner runnerFixture
		lease  tracker.NativeLease
	}
	claim := func(f nativeFixture, requirement providercapacity.Requirement, session string) owner {
		t.Helper()
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.enroll(t)
		r.reportCapacity(t)
		report := capacityReport(now)
		planner := report
		planner.Backend, planner.AccountAlias, planner.SharedAccountAlias = "planner", "planning", "planning"
		publishCapacity(t, f, r, report, planner)
		issue := f.create(t, session)
		request := providerClaim(r, issue, session)
		request.PolicyID = pinned.ID
		request.ProviderCandidates[0].Requirement = requirement
		response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		if lease.ProviderReservation == nil || lease.ProviderReservation.Requirement != requirement {
			t.Fatalf("wrong starting permit: %+v", lease.ProviderReservation)
		}
		event := nativeStartedEvent(lease)
		event.Data.Identity = &tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), http.StatusOK)
		return owner{runner: r, lease: lease}
	}
	for _, f := range []nativeFixture{first, second} {
		approveHubTestPolicy(t, service, f.base+"/policy", pinned)
	}
	local := claim(first, plan, "local-plan")
	other := claim(second, code, "other-code")
	swap := func(f nativeFixture, lease tracker.NativeLease, requester runnerFixture, from, to providercapacity.Requirement) *httptest.ResponseRecorder {
		t.Helper()
		return performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", requester.redemption.Credential,
			tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90,
				ProviderPhase: &tracker.NativeProviderPhaseMutation{Expected: from, Requirement: to, Sequence: 1}})
	}
	if refused := swap(second, other.lease, local.runner, code, plan); refused.Code < 400 {
		t.Fatal("runner from another tenant changed the permit")
	}
	response := swap(first, local.lease, local.runner, plan, code)
	requireNativeStatus(t, response, http.StatusOK)
	var renewed tracker.NativeLease
	decodeHubResponse(t, response, &renewed)
	if renewed.ID != local.lease.ID || renewed.FencingToken != local.lease.FencingToken || renewed.ProviderReservation == nil || renewed.ProviderReservation.Requirement != code {
		t.Fatalf("swap changed the fence or selected the wrong account: %+v", renewed)
	}
	response = swap(second, other.lease, other.runner, plan, code)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &renewed)
	if renewed.ID != other.lease.ID || renewed.ProviderReservation == nil || renewed.ProviderReservation.Requirement != code {
		t.Fatalf("another tenant's permit changed: %+v", renewed)
	}
}
