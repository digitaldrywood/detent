package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestMachineIssueDuplicate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, existing, state string
		reused                bool
	}{
		{"same fingerprint", issueorigin.Stamp("Original", issueorigin.Origin{Kind: "routine", Instance: "one", Source: "run-1", Fingerprint: "disk"}), "open", true},
		{"legacy audit", "<!-- detent-audit-fp:disk -->", "open", true},
		{"different fingerprint", issueorigin.Stamp("Other", issueorigin.Origin{Kind: "audit", Fingerprint: "network"}), "open", false},
		{"closed issue", issueorigin.Stamp("Closed", issueorigin.Origin{Kind: "worker", Fingerprint: "disk"}), "closed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := issueorigin.Stamp("New occurrence", issueorigin.Origin{Kind: "worker", Instance: "two", Source: "attempt-2", Fingerprint: "disk"})
			path := "/repos/example/repo/issues"
			response := fmt.Sprintf(`{"node_id":"I_43","number":43,"body":%q,"state":"open"}`, body)
			if tt.reused {
				path = "/repos/example/repo/issues/42/comments"
				response = `{"node_id":"IC_1"}`
			}
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, body: fmt.Sprintf(`[{"node_id":"I_42","number":42,"body":%q,"state":%q}]`, tt.existing, tt.state)},
				{method: http.MethodPost, path: path, body: response},
			})
			c := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			c.machineIssueStore = &machineTestStore{records: map[string]coordination.Record{}}
			issue, err := c.CreateIssue(t.Context(), connector.IssueDraft{Title: "Disk space", Body: body})
			if err != nil || issue.PublicationReused != tt.reused {
				t.Fatalf("CreateIssue() = %+v, %v", issue, err)
			}
			requests := server.requests()
			posted := requests[1]["body"].(map[string]any)["body"].(string)
			if tt.reused && (!strings.Contains(posted, "New machine occurrence") || issue.Description != tt.existing) {
				t.Fatalf("duplicate replaced body or lost occurrence: %+v %s", issue, posted)
			}
		})
	}
}

func TestSeptemberEightDiskOccurrences(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		number    int
		title, at string
	}{
		{2364, "Restore disk capacity for Detent worker validation", "2026-09-08T22:45:23Z"},
		{2365, "operator: restore validation host disk capacity", "2026-09-08T22:46:49Z"},
		{2366, "Restore free disk capacity for Detent validation", "2026-09-08T22:52:31Z"},
		{2367, "Free host disk space for Detent validation", "2026-09-08T22:53:18Z"},
	}
	var responses []graphqlTestResponse
	original := issueorigin.Stamp(fixtures[0].title, issueorigin.Origin{Kind: "worker", Instance: "validation-host", Source: fixtures[0].at, Fingerprint: "validation-host-disk-capacity"})
	for index := range fixtures {
		if index == 0 {
			responses = append(responses,
				graphqlTestResponse{method: http.MethodGet, body: `[]`},
				graphqlTestResponse{method: http.MethodPost, path: "/repos/example/repo/issues", body: fmt.Sprintf(`{"node_id":"I_2364","number":2364,"state":"open","body":%q}`, original)},
			)
		} else {
			responses = append(responses,
				graphqlTestResponse{method: http.MethodGet, body: fmt.Sprintf(`[{"node_id":"I_2364","number":2364,"state":"open","body":%q}]`, original)},
				graphqlTestResponse{method: http.MethodPost, path: "/repos/example/repo/issues/2364/comments", body: `{"node_id":"IC_occurrence"}`},
			)
		}
	}
	server := newGraphQLTestServer(t, responses)
	c := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	c.machineIssueStore = &machineTestStore{records: map[string]coordination.Record{}}
	for index, fixture := range fixtures {
		body := issueorigin.Stamp(fixture.title, issueorigin.Origin{Kind: "worker", Instance: "validation-host", Source: fixture.at, Fingerprint: "validation-host-disk-capacity"})
		issue, err := c.CreateIssue(t.Context(), connector.IssueDraft{Title: fixture.title, Body: body})
		if err != nil || issue.ID != "I_2364" || issue.PublicationReused != (index > 0) {
			t.Fatalf("occurrence #%d = %+v, %v", fixture.number, issue, err)
		}
	}
}

func TestMachineOriginSurvivesBodyUpdates(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"body", "intake"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			original := issueorigin.Stamp("Original", issueorigin.Origin{Kind: "routine", Instance: "first-instance", Source: "first-run", Fingerprint: "problem"})
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, body: fmt.Sprintf(`{"node_id":"I_42","number":42,"state":"open","body":%q}`, original)},
				{method: http.MethodPatch, path: "/repos/example/repo/issues/42", body: `{"node_id":"I_42","number":42,"state":"open","body":"updated"}`},
			})
			c := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			c.machineIssueStore = &machineTestStore{records: map[string]coordination.Record{}}
			c.projectCache.SetIssueRef("I_42", issueRef{Owner: "example", Name: "repo", Number: 42})
			var err error
			if mode == "body" {
				err = c.UpdateIssueBody(t.Context(), "I_42", "Updated")
			} else {
				_, err = c.UpdateIntakeIssue(t.Context(), "I_42", intake.IssueDraft{Title: "Updated", Body: "Updated"})
			}
			if err != nil {
				t.Fatal(err)
			}
			body := server.requests()[1]["body"].(map[string]any)["body"].(string)
			origin, ok := issueorigin.Parse(body)
			if !ok || origin.Instance != "first-instance" || origin.Source != "first-run" || origin.Fingerprint != "problem" {
				t.Fatalf("updated origin = %+v", origin)
			}
		})
	}
}

type machineTestStore struct {
	mu      sync.Mutex
	records map[string]coordination.Record
}

func (s *machineTestStore) Get(_ context.Context, key string) (coordination.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	return record, ok, nil
}

func (s *machineTestStore) CompareAndSwap(_ context.Context, key, version string, value []byte) (coordination.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.records[key]
	if previous.Version != version {
		return previous, false, nil
	}
	record := coordination.Record{Value: append([]byte(nil), value...), Version: version + "x", ModifiedAt: time.Now()}
	s.records[key] = record
	return record, true, nil
}

func TestMachineIssueSeparateConnectors(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var body string
	creates, comments := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			if body == "" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"node_id":"I_1","number":1,"state":"open","body":%q}]`, body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			comments++
			fmt.Fprint(w, `{"node_id":"IC_1"}`)
		default:
			var draft struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
				t.Error(err)
			}
			body = draft.Body
			creates++
			fmt.Fprintf(w, `{"node_id":"I_1","number":1,"state":"open","body":%q}`, body)
		}
	}))
	defer server.Close()
	store := &machineTestStore{records: map[string]coordination.Record{}}
	results := make(chan error, 2)
	reused := make(chan bool, 2)
	for _, kind := range []string{"worker", "routine"} {
		c, err := NewConnector(Config{Endpoint: server.URL, APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
		if err != nil {
			t.Fatal(err)
		}
		c.machineIssueStore = store
		go func() {
			issue, err := c.CreateIssue(t.Context(), connector.IssueDraft{Title: "Disk", Body: issueorigin.Stamp(kind, issueorigin.Origin{Kind: kind, Fingerprint: "disk"})})
			if err == nil && issue.ID != "I_1" {
				err = fmt.Errorf("issue ID = %q", issue.ID)
			}
			reused <- issue.PublicationReused
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if first, second := <-reused, <-reused; first == second {
		t.Fatalf("reused = %v, %v", first, second)
	}
	if creates != 1 || comments != 1 {
		t.Fatalf("creates=%d comments=%d", creates, comments)
	}
}

func TestMachineIssueCoordinationFailure(t *testing.T) {
	t.Parallel()
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"errors":[{"message":"coordination access denied"}]}`},
	})
	c := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	_, err := c.CreateIssue(t.Context(), connector.IssueDraft{Title: "Disk", Body: issueorigin.Stamp("Disk", issueorigin.Origin{Kind: "worker", Fingerprint: "disk"})})
	if err == nil {
		t.Fatal("expected coordination failure")
	}
	if len(server.requests()) != 1 {
		t.Fatalf("requests=%v", server.requests())
	}
}

func TestMachineIssueListingAbsenceIsNotClosure(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"open", "closed"} {
		t.Run(state, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, body: `[]`},
				{method: http.MethodGet, path: "/repos/example/repo/issues/1", body: fmt.Sprintf(`{"node_id":"I_1","number":1,"state":%q,"body":"original"}`, state)},
			})
			c := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			c.projectCache.SetIssueRef("I_1", issueRef{Owner: "example", Name: "repo", Number: 1})
			backend := &fingerprintIssueBackend{connector: c, fingerprint: "disk"}
			closed, err := backend.IntakeIssueClosed(t.Context(), "I_1")
			if err != nil || closed != (state == "closed") {
				t.Fatalf("closed=%v err=%v", closed, err)
			}
		})
	}
}
