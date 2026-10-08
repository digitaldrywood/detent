package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func capacityReport(now time.Time) providercapacity.Report {
	return providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team", Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
}

func publishCapacity(t *testing.T, f nativeFixture, r runnerFixture, reports ...providercapacity.Report) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": reports})
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
	}{
		{"available", func(*providercapacity.Report, *tracker.NativeClaim) {}, http.StatusOK},
		{"unknown", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "unknown" }, http.StatusOK},
		{"stale exhaustion", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(-providercapacity.MaxAge)
		}, http.StatusOK},
		{"future observation", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(time.Second)
		}, http.StatusOK},
		{"exhausted", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "exhausted" }, http.StatusConflict},
		{"reset", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ResetAt = r.ObservedAt
			r.ObservedAt = r.ObservedAt.Add(-time.Minute)
		}, http.StatusOK},
		{"explicit model unavailable", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = "expensive"
		}, http.StatusConflict},
		{"wrong backend", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Backend = "other"
		}, http.StatusConflict},
		{"changed issue", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates[0].Revision++ }, http.StatusConflict},
		{"missing requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates = nil }, http.StatusConflict},
		{"invalid requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = ""
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "provider")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			report := capacityReport(now)
			claim := providerClaim(r, issue, "work")
			test.change(&report, &claim)
			publishCapacity(t, f, r, report)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, test.want)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM provider_reservations").Scan(&count); err != nil {
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
			if count != 1 || lease.ProviderReservation == nil || lease.ProviderReservation.Model != "test-model" {
				t.Fatalf("lease = %+v", lease)
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

func TestProviderSharedCapacityConcurrentRecovery(t *testing.T) {
	t.Parallel()
	for _, sharing := range []string{"declared", "unknown", "mixed"} {
		t.Run(sharing, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
			f := newNativeFixture(t, openTestService(t, cfg), "", "shared-provider")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			runners := make([]runnerFixture, 2)
			for i := range runners {
				runners[i] = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				runners[i].enroll(t)
				report := capacityReport(now)
				report.AccountAlias = fmt.Sprintf("local-%d", i)
				if sharing == "unknown" || sharing == "mixed" && i == 1 {
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
			if count != 1 {
				t.Fatalf("concurrent reservations = %d, want 1", count)
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, cfg)
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

func TestProviderStartRevalidation(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"exhausted", "model", "account", "active"} {
		t.Run(change, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			report := capacityReport(f.service.config.now())
			publishCapacity(t, f, r, report)
			issue := f.create(t, "work")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "session"))
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			if change == "active" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
			}
			switch change {
			case "exhausted", "active":
				report.Availability = "exhausted"
			case "model":
				event.Data.Identity.Model = "expensive"
			case "account":
				report.AccountAlias = "changed"
			}
			publishCapacity(t, f, r, report)
			if change == "active" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
				event.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusConflict)
			}
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
	}{
		{name: "legacy numeric order", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{0, 1, 2}, unavailable: "provider", want: []int{0, 1, 2}, winner: 1},
		{name: "issue priority beats merging lane", states: []string{"Todo", "Merging", "Todo"}, priorities: []int{0, 3, 2}, order: []string{"Merging", "Rework", "In Progress", "Todo"}, want: []int{0, 2, 1}},
		{name: "configured lane preference yields to age", states: []string{"Merging", "Todo", "Rework"}, priorities: []int{1, 1, 1}, order: []string{"Todo", "Rework", "Merging"}, want: []int{0, 1, 2}},
		{name: "source lanes yield to age", states: []string{"Todo", "In Progress", "Rework"}, priorities: []int{1, 1, 1}, order: []string{"Rework", "In Progress", "Todo"}, want: []int{0, 1, 2}},
		{name: "numeric priority precedes source lane", states: []string{"Todo", "In Progress", "Rework"}, priorities: []int{0, 1, 2}, order: []string{"Rework", "In Progress", "Todo"}, want: []int{0, 1, 2}},
		{name: "configured labels and lanes yield to age", states: []string{"Todo", "Rework", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Rework", "Todo"}, labels: []string{"hotfix", "bug"}, want: []int{0, 1, 2}},
		{name: "queue rank yields to age", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, ranks: []string{"c", "a", "b"}, want: []int{0, 1, 2}},
		{name: "precise creation order breaks unranked ties", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, ranks: []string{" ", "  ", "   "}, created: []string{"2026-10-02T12:00:00.000000002Z", "2026-10-02T12:00:00Z", "2026-10-02T12:00:00.000000001Z"}, want: []int{1, 2, 0}, winner: 1},
		{name: "unblocker count yields to age", states: []string{"Todo", "Todo", "Todo"}, priorities: []int{1, 1, 1}, order: []string{"Todo"}, unavailable: "unblocker", want: []int{0, 1}},
		{name: "required review is retained", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "required review", want: []int{1, 2}, winner: 1},
		{name: "unavailable merging provider falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{0, 1, 2}, order: []string{"Merging", "Todo"}, unavailable: "provider", want: []int{0, 1, 2}, winner: 1},
		{name: "changed merging revision falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{0, 1, 2}, order: []string{"Merging", "Todo"}, unavailable: "revision", want: []int{0, 1, 2}, winner: 1},
		{name: "unreviewed merging falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "review", want: []int{1, 2}, winner: 1},
		{name: "dependency held merging falls through", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "dependency", want: []int{1, 2}, winner: 1},
		{name: "leased merging leaves next slot for todo", states: []string{"Merging", "Todo", "Todo"}, priorities: []int{3, 0, 2}, order: []string{"Merging", "Todo"}, unavailable: "lease", want: []int{1, 2}, winner: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			for _, state := range []string{"Merging", "Rework"} {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)", f.project.ID, state, state, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: test.unavailable == "required review"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
			report := capacityReport(f.service.config.now())
			report.MaxConcurrent = 2
			publishCapacity(t, f, r, report)
			issues := []tracker.NativeIssue{f.create(t, "head"), f.create(t, "next"), f.create(t, "tail")}
			claim := providerClaim(r, issues[0], "fair")
			claim.WorkItemID, claim.ProviderCandidates = "", nil
			claim.DispatchPriorityByState, claim.DispatchPriorityByLabel = test.order, test.labels
			claim.WorkflowStates = []string{"Merging", "Rework", "In Progress", "Todo"}
			claim.PrioritizeUnblockers = test.unavailable == "unblocker"
			for i, issue := range issues {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?) WHERE native_id = ?", f.project.ID, test.states[i], issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				rank := strconv.Itoa(i)
				if test.ranks != nil {
					rank = test.ranks[i]
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = ?, rank = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", test.priorities[i], rank, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if test.created != nil {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at = ? WHERE native_id = ?", test.created[i], issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if test.labels != nil && i < len(test.labels) {
					raw, err := marshalNative([]string{test.labels[i]})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json = ? WHERE native_id = ?", raw, issue.WorkItemID); err != nil {
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
			if test.unavailable == "dependency" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (dependent_issue_id, blocker_issue_id, provenance, created_at, updated_at) SELECT a.id, b.id, 'native', ?, ? FROM issues a, issues b WHERE a.native_id = ? AND b.native_id = ?", testTimestamp, testTimestamp, issues[0].WorkItemID, issues[2].WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.unavailable == "unblocker" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (dependent_issue_id, blocker_issue_id, provenance, created_at, updated_at) SELECT a.id, b.id, 'native', ?, ? FROM issues a, issues b WHERE a.native_id = ? AND b.native_id = ?", testTimestamp, testTimestamp, issues[2].WorkItemID, issues[1].WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.unavailable == "lease" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issues[0], "running")), http.StatusOK)
			}
			var writesBefore, writesAfter int64
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&writesBefore); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: claim}), http.StatusOK)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: claim})
			requireNativeStatus(t, response, http.StatusOK)
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&writesAfter); err != nil || writesAfter-writesBefore != 9 {
				t.Fatalf("candidate previews wrote %d rows beyond authentication metadata, err=%v", writesAfter-writesBefore-9, err)
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
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != issues[test.winner].WorkItemID {
				t.Fatalf("claim = %s, want %s", lease.WorkItemID, issues[test.winner].WorkItemID)
			}
			var leases, reservations int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE released_at IS NULL").Scan(&leases); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM provider_reservations").Scan(&reservations); err != nil {
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
				var failure nativeError
				decodeHubResponse(t, response, &failure)
				if failure.Code != "selector_no_match" {
					t.Fatalf("fixed host lost authority: %s", failure.Code)
				}
			}
		})
	}
}

func TestProviderOlderReportsCannotRestoreQuota(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	report := capacityReport(f.service.config.now().Add(-time.Second))
	report.Availability = "exhausted"
	publishCapacity(t, f, r, report)
	for _, age := range []time.Duration{0, -time.Second, time.Hour} {
		stale := report
		stale.ObservedAt = stale.ObservedAt.Add(age)
		stale.Availability = "available"
		publishCapacity(t, f, r, stale)
		stored, err := readProviderReports(t.Context(), f.service.database.db, r.binding.RunnerID)
		if err != nil || len(stored) != 1 || stored[0].Availability != "exhausted" || !stored[0].ObservedAt.Equal(report.ObservedAt) {
			t.Fatalf("stale observation replaced exhaustion: %+v, %v", stored, err)
		}
	}
}

func TestProviderPoolIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, provider, shared, authority, availability, leaseEnd string
		otherOrganization, stale, offline, reserve                bool
		used, bound, finalBound                                   int
		state                                                     string
	}{
		{name: "same pool", reserve: true, used: 1, bound: 2, state: "available"},
		{name: "explicit independent account", shared: "independent", reserve: true, bound: 4, state: "available"},
		{name: "unknown sharing", shared: "unknown", reserve: true, used: 1, bound: 2, state: "available"},
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
		{name: "missing expiry", authority: "missing", bound: 4, state: "available"},
		{name: "expired pinned lease until expiry", authority: "expired", reserve: true, leaseEnd: "expiry", used: 1, bound: 2, finalBound: 4, state: "available"},
		{name: "revoked pinned lease until expiry", authority: "revoked", reserve: true, leaseEnd: "expiry", used: 1, bound: 2, finalBound: 4, state: "available"},
		{name: "fresh report supersedes pinned lease bound before release", authority: "expired", reserve: true, leaseEnd: "release", used: 1, bound: 4, finalBound: 4, state: "available"},
		{name: "draining authority and lease", authority: "draining", reserve: true, leaseEnd: "release", used: 1, bound: 2, finalBound: 2, state: "available"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "provider-authority")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			current := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			current.enroll(t)
			otherFixture := f
			if test.otherOrganization {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations (id, name, created_at) VALUES ('org_other', 'Other', ?)", formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				otherFixture = newNativeFixture(t, f.service, "org_other", "other-provider")
				approveHubTestPolicy(t, f.service, otherFixture.base+"/policy", hubTestPolicy())
			}
			otherRunner := prepareRunner(t, otherFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			otherRunner.enroll(t)
			report := capacityReport(now)
			report.MaxConcurrent = 4
			if test.leaseEnd == "release" {
				report.MaxConcurrent = 2
			}
			publishCapacity(t, f, current, report)
			other := capacityReport(now)
			other.AccountAlias, other.MaxConcurrent = "older-runner", 2
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
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(now.Add(-48*time.Hour)), otherRunner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			var statement string
			switch test.authority {
			case "expired":
				statement = "UPDATE api_tokens SET expires_at = ? WHERE id = ?"
			case "future":
				statement = "UPDATE api_tokens SET created_at = ? WHERE id = ?"
			case "invalid":
				statement = "UPDATE api_tokens SET expires_at = 'invalid' WHERE id = ?"
			case "missing":
				statement = "UPDATE api_tokens SET expires_at = NULL WHERE id = ?"
			case "revoked":
				statement = "UPDATE api_tokens SET revoked_at = ? WHERE id = ?"
			case "draining":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET state = 'draining' WHERE id = ?", otherRunner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if statement != "" {
				args := []any{otherRunner.binding.RunnerID}
				switch test.authority {
				case "expired", "revoked":
					args = []any{formatHubTime(now), otherRunner.binding.RunnerID}
				case "future":
					args = []any{formatHubTime(now.Add(time.Second)), otherRunner.binding.RunnerID}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			view, err := providerView(t.Context(), f.service.database.db, f.project.OrganizationID, report, now)
			if err != nil || view.Used != test.used || view.MaxConcurrent != test.bound || view.State != test.state {
				t.Fatalf("view = %+v, %v; want used=%d bound=%d state=%s", view, err, test.used, test.bound, test.state)
			}
			if test.leaseEnd != "" {
				if test.leaseEnd == "release" {
					path := f.base + "/leases/" + string(lease.ID) + "/release"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, current.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				} else {
					now = lease.ExpiresAt
				}
				view, err = providerView(t.Context(), f.service.database.db, f.project.OrganizationID, report, now)
				if err != nil || view.Used != 0 || view.MaxConcurrent != test.finalBound {
					t.Fatalf("ended lease view = %+v, %v; want used=0 bound=%d", view, err, test.finalBound)
				}
			}
			stored, err := readProviderReports(t.Context(), f.service.database.db, otherRunner.binding.RunnerID)
			if err != nil || len(stored) != 1 || !reflect.DeepEqual(stored[0], other) {
				t.Fatalf("historical report changed: %+v, %v", stored, err)
			}
		})
	}
}
