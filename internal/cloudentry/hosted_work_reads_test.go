package cloudentry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeDuplicateFixture struct {
	ChallengeToken     string `json:"challenge_token"`
	PossibleDuplicates []struct {
		Number int `json:"number"`
	} `json:"possible_duplicates"`
}

type hostedHubReadFixture struct {
	entry   entryFixture
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
	project := attachmentProject(t, owner, "org_alpha")
	approveEntryFixturePolicy(t, owner, "org_alpha", project)
	return hostedHubReadFixture{
		entry:   entry,
		browser: owner,
		base:    "/api/v2/organizations/org_alpha/projects/" + project,
		csrf:    csrfFrom(t, body),
	}
}

func (f hostedHubReadFixture) waitForCompleted(t *testing.T) {
	t.Helper()
	identity, err := f.entry.provider.Exchange(t.Context(), "user_alice:porg_alpha", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(f.base, "/api/v2") + "/events?board=true"
	claims, err := f.entry.service.claims(Organization{ID: "org_alpha", Generation: 1}, cloudassert.KindBrowser, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims.Subject, claims.Email, claims.ProviderOrganization, claims.ProviderSession = identity.Subject, identity.Email, identity.Hosted.OrganizationID, identity.Hosted.SessionID
	claims.SessionCreatedAt, claims.SessionExpiresAt = identity.Hosted.CreatedAt, identity.Hosted.ExpiresAt
	claims.Role, claims.AccessExpiresAt = "owner", identity.Hosted.ExpiresAt
	claims.Binding = cloudassert.AuthorizationBinding("board-totals", "org_alpha", identity.Hosted.SessionID)
	claims.CSRF = cloudassert.CSRFToken("board-totals", "org_alpha")
	assertion, err := cloudassert.Sign(f.entry.service.config.SigningKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.entry.fixtures["org_alpha"].service.Handler())
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(cloudassert.Header, assertion)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("board stream status = %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var frame tracker.NativeBoardFrame
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Work != nil && frame.Work.Completed == 1 {
			return
		}
	}
	t.Fatalf("board stream ended before completed work: %v", scanner.Err())
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
		draft := map[string]any{
			"idempotency_key": "seed-" + state, "title": state + " hosted issue", "state": initialState,
			"body": "## Acceptance criteria\nRead through the shared entry.\n## Must not break\nTenant isolation.\n## How we know it worked\nRun the HTTP regression.",
		}
		response := f.request(t, http.MethodPost, f.base+"/work-items", draft)
		var challenge nativeDuplicateFixture
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
			if test.work {
				f.waitForCompleted(t)
			}
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

func approveEntryFixturePolicy(t *testing.T, owner *browser, organization, project string) {
	t.Helper()
	selected := newBrowser(t, owner.entry)
	selected.login("/organizations/"+organization+"/organization", "user_alice:p"+organization)
	response, page := selected.get("/organizations/" + organization + "/organization")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("organization=%d %s", response.StatusCode, page)
	}
	fixture := hostedHubReadFixture{browser: selected, base: "/api/v2/organizations/" + organization + "/projects/" + project, csrf: csrfFrom(t, page)}
	current := fixture.request(t, http.MethodGet, fixture.base+"/policy", nil)
	var approval policy.Approval
	if current.Code == http.StatusOK {
		if err := json.Unmarshal(current.Body.Bytes(), &approval); err != nil {
			t.Fatal(err)
		}
	}
	applied := fixture.request(t, http.MethodPut, fixture.base+"/policy", policy.Change{ExpectedID: approval.Policy.ID, Policy: pilotPolicy()})
	if applied.Code != http.StatusOK {
		t.Fatalf("approve fixture policy=%d %s", applied.Code, applied.Body)
	}
}
