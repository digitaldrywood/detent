package hubclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCreateIssueResponse(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"created", http.StatusOK, `{"work_item_id":"wi_created"}`, false},
		{"challenge", http.StatusConflict, `{"code":"possible_duplicates","message":"Possible duplicate issues found","challenge_token":"dup_test","possible_duplicates":[{"number":1}]}`, true},
		{"legacy success challenge", http.StatusOK, `{"challenge_token":"dup_test","possible_duplicates":[{"number":1}]}`, true},
		{"missing issue identity", http.StatusOK, `{}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example/work-items" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if _, err := w.Write([]byte(test.body)); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Example issue", State: "Todo"})
			if (err != nil) != test.wantError {
				t.Fatalf("issue=%#v, error=%v", issue, err)
			}
			if err == nil && issue.WorkItemID != "wi_created" || err != nil && issue.WorkItemID != "" {
				t.Fatalf("unexpected issue identity: %#v", issue)
			}
			if test.status == http.StatusConflict {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "possible_duplicates" {
					t.Fatalf("challenge was not an API error: %v", err)
				}
			}
		})
	}
}
