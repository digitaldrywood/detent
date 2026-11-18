package workreads_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entryfixture"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedHubReadFixture struct {
	entry   entryfixture.Fixture
	browser *harness.Browser
	base    string
	csrf    string
}

func newHostedHubReadFixture(t *testing.T) hostedHubReadFixture {
	t.Helper()
	entry := entryfixture.NewWithLogger(t, slog.New(slog.DiscardHandler), func(cfg *hubserver.Config) {
		harness.ConfigurePostgresTenant(t, cfg)
		cfg.RunDetails = hubmaintenance.LimitDetails(cfg.RunDetails)
	})
	owner := harness.NewBrowser(t, entry.Service.Handler())
	owner.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	response, body := owner.Get("/organizations/org_alpha/app/bootstrap")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("organization status = %d: %s", response.StatusCode, body)
	}
	project := harness.Project(t, owner, "org_alpha")
	harness.ApprovePolicy(t, owner, "org_alpha", project)
	return hostedHubReadFixture{
		entry:   entry,
		browser: owner,
		base:    "/api/v2/organizations/org_alpha/projects/" + project,
		csrf:    harness.BootstrapCSRF(t, body),
	}
}

func (f hostedHubReadFixture) request(t *testing.T, method, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return harness.JSONRequest(t, f.browser, f.csrf, method, target, payload)
}

func TestHostedHubWorkReads(t *testing.T) {
	t.Parallel()
	f := newHostedHubReadFixture(t)
	issues := make(map[string]tracker.NativeIssue)
	for _, state := range []string{"Todo", "Blocked", "Done"} {
		initialState := state
		if state == "Done" {
			initialState = "Todo"
		}
		draft := map[string]any{
			"idempotency_key": "seed-" + state, "title": state + " hosted issue", "state": initialState,
			"body": "## Acceptance criteria\nRead through the shared entry.\n## Must not break\nTenant isolation.\n## How we know it worked\nRun the HTTP regression.",
		}
		response := f.request(t, http.MethodPost, f.base+"/work-items", draft)
		var challenge harness.DuplicateChallenge
		if json.Unmarshal(response.Body.Bytes(), &challenge) != nil {
			t.Fatalf("file fixture=%d %s", response.Code, response.Body)
		}
		if challenge.ChallengeToken != "" {
			if response.Code != http.StatusConflict {
				t.Fatalf("challenge status=%d: %s", response.Code, response.Body)
			}
			var numbers []int
			for _, match := range challenge.PossibleDuplicates {
				numbers = append(numbers, match.Number)
			}
			draft["challenge_token"], draft["not_duplicate_of"] = challenge.ChallengeToken, numbers
			response = f.request(t, http.MethodPost, f.base+"/work-items", draft)
		}
		var issue tracker.NativeIssue
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &issue) != nil || issue.WorkItemID == "" {
			t.Fatalf("create %s status = %d: %s", state, response.Code, response.Body)
		}
		if state == "Done" {
			response = f.request(t, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{
				Mutation: tracker.Mutation{IdempotencyKey: "complete"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested",
			})
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &issue) != nil || issue.State != "Done" {
				t.Fatalf("complete status = %d: %s", response.Code, response.Body)
			}
		}
		issues[state] = issue
	}
	tenant := hubmaintenance.Tenant{Organization: "org_alpha", Generation: 1}
	var after tracker.NativeWorkItemID
	for range len(issues) {
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubserver.BoardMaintenanceBytes}).Context(t.Context(), tenant)
		next, processed, err := f.entry.Fixtures[tenant.Organization].Service.MaintainBoardRecord(ctx, tracker.OrganizationID(tenant.Organization), after)
		if err != nil || !processed {
			t.Fatalf("project fixture board after %s: processed=%t error=%v", after, processed, err)
		}
		after = next
	}
	t.Run("board totals", func(t *testing.T) {
		response := f.request(t, http.MethodGet, f.base+"/board/counts", nil)
		var page viewstore.BoardSummaryPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.ProjectionPending {
			t.Fatalf("board counts=%d %s", response.Code, response.Body)
		}
		for _, row := range page.Rows {
			if row.Kind == "total" {
				if row.Total != 3 || row.Shipped24h != 1 {
					t.Fatalf("board totals lost open or completed work: %s", response.Body)
				}
				return
			}
		}
		t.Fatalf("board counts missing totals: %s", response.Body)
	})
	for _, test := range []struct {
		name, query string
		states      []string
		work        bool
	}{
		{"work-items list", "limit=100", []string{"Todo", "Blocked", "Done"}, false},
		{"board lanes", "state=Todo&state=Blocked&state=In+Progress&state=Human+Review&state=Merging&state=Rework&open=true&limit=200&completed_window=48h", []string{"Todo", "Blocked"}, false},
		{"open work page", "limit=1&completed_window=48h&include=work", []string{"Todo", "Blocked"}, true},
		{"repeated filter dimensions", "state=Todo&state=Blocked&label=bug&label=feature&assignee=nobody&assignee=other&priority=1&priority=2&open=true&limit=200&completed_window=48h", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, f.base+"/work-items?"+test.query, nil)
			var page tracker.NativeIssuePage
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
				t.Fatalf("read status = %d: %s", response.Code, response.Body)
			}
			if page.Total != -1 || page.ProjectionPending {
				t.Fatalf("list total or projection status: %s", response.Body)
			}
			if test.work {
				if page.Work == nil || len(page.Work.Items) != 1 || !page.Work.Truncated || page.NextCursor == "" || len(page.Items) != 1 {
					t.Fatalf("open work page lost bounded pagination: %s", response.Body)
				}
				item := page.Work.Items[0]
				if issue, ok := issues[item.State]; !ok || item.State == "Done" || item.WorkItemID != issue.WorkItemID || item.ProjectID != issue.ProjectID {
					t.Fatalf("open work page returned an unexpected issue: %s", response.Body)
				}
				return
			}
			if len(page.Items) != len(test.states) {
				t.Fatalf("items = %d, want %d", len(page.Items), len(test.states))
			}
			for _, state := range test.states {
				found := false
				for _, item := range page.Items {
					if item.WorkItemID == issues[state].WorkItemID && item.ProjectID == issues[state].ProjectID && item.State == state {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing %s issue: %s", state, response.Body)
				}
			}
		})
	}
	for _, test := range []struct{ name, target string }{
		{"issue page lookup", "/api/v2/organizations/org_alpha/work-items/" + string(issues["Todo"].WorkItemID)},
		{"project issue read", f.base + "/work-items/" + string(issues["Todo"].WorkItemID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, test.target, nil)
			var issue tracker.NativeIssue
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &issue) != nil || issue.WorkItemID != issues["Todo"].WorkItemID || issue.Body != issues["Todo"].Body {
				t.Fatalf("issue read status = %d: %s", response.Code, response.Body)
			}
		})
	}
	for _, test := range []struct {
		name, target string
		status       int
	}{
		{"other tenant", "/api/v2/organizations/org_beta/work-items/" + string(issues["Todo"].WorkItemID), http.StatusUnauthorized},
		{"unregistered tenant", "/api/v2/organizations/org_missing/work-items/" + string(issues["Todo"].WorkItemID), http.StatusNotFound},
		{"repeated singleton", f.base + "/work-items?limit=1&limit=200", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, test.target, nil)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body)
			}
		})
	}
}
