package scenariotest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
	endpoint := *fixtureClient(native.Transport()).baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + native.Scope() + "/work-items"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	token, err := fixtureClient(native.Transport()).bearerToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := fixtureClient(native.Transport()).httpClient.Do(request)
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
