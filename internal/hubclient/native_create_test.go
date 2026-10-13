package hubclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func createIndependentNativeIssue(t *testing.T, native *NativeClient, draft tracker.CreateIssue) tracker.NativeIssue {
	t.Helper()
	encoded, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := *native.client.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + native.base() + "/work-items"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	token, err := native.client.bearerToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := native.client.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusConflict {
		var challenge struct {
			Code    string `json:"code"`
			Token   string `json:"challenge_token"`
			Matches []struct {
				Number int `json:"number"`
			} `json:"possible_duplicates"`
		}
		if err := json.Unmarshal(payload, &challenge); err != nil {
			t.Fatal(err)
		}
		if challenge.Code != "possible_duplicates" || challenge.Token == "" || len(challenge.Matches) == 0 {
			t.Fatalf("independent fixture challenge: %s", payload)
		}
		draft.ChallengeToken = challenge.Token
		for _, match := range challenge.Matches {
			draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
		}
		issue, err := native.CreateIssue(t.Context(), draft)
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("independent fixture create: %d %s", response.StatusCode, payload)
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(payload, &issue); err != nil {
		t.Fatal(err)
	}
	if issue.WorkItemID == "" {
		t.Fatal("independent fixture omitted issue identity")
	}
	return issue
}

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
