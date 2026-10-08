package cloudentry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedHubReadFixture struct {
	browser *browser
	base    string
	csrf    string
}

func newHostedHubReadFixture(t *testing.T) hostedHubReadFixture {
	t.Helper()
	entry := newEntryFixture(t)
	owner := newBrowser(t, entry.service.Handler())
	owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	response, body := owner.get("/organizations/org_alpha/organization")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("organization status = %d: %s", response.StatusCode, body)
	}
	return hostedHubReadFixture{
		browser: owner,
		base:    "/api/v2/organizations/org_alpha/projects/" + attachmentProject(t, owner, "org_alpha"),
		csrf:    csrfFrom(t, body),
	}
}

func (f hostedHubReadFixture) request(t *testing.T, method, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	return attachmentRequest(t, f.browser, method, target, bytes.NewReader(body), map[string]string{
		"Content-Type": "application/json", "X-CSRF-Token": f.csrf,
	})
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
		response := f.request(t, http.MethodPost, f.base+"/work-items", map[string]any{
			"idempotency_key": "seed-" + state, "title": state + " hosted issue", "state": initialState,
			"body": "## Acceptance criteria\nRead through the shared entry.\n## Must not break\nTenant isolation.\n## How we know it worked\nRun the HTTP regression.",
		})
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
	for _, test := range []struct {
		name, query string
		states      []string
		work        bool
	}{
		{"work-items list", "limit=100", []string{"Todo", "Blocked", "Done"}, false},
		{"board lanes", "state=Todo&state=Blocked&state=In+Progress&state=Human+Review&state=Merging&state=Rework&open=true&limit=200&completed_window=48h", []string{"Todo", "Blocked"}, false},
		{"board totals", "limit=1&completed_window=48h&include=work", []string{"Todo", "Blocked", "Done"}, true},
		{"repeated filter dimensions", "state=Todo&state=Blocked&label=bug&label=feature&assignee=nobody&assignee=other&priority=1&priority=2&open=true&limit=200&completed_window=48h", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, f.base+"/work-items?"+test.query, nil)
			var page tracker.NativeIssuePage
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
				t.Fatalf("read status = %d: %s", response.Code, response.Body)
			}
			if page.Total != len(test.states) {
				t.Fatalf("total = %d, want %d: %s", page.Total, len(test.states), response.Body)
			}
			if test.work {
				if page.Work == nil || len(page.Work.Items) != 1 || !page.Work.Truncated || page.Work.Completed != 1 {
					t.Fatalf("board totals lost open or completed work: %s", response.Body)
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
