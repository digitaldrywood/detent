package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
)

type prerequisiteTracker struct {
	mu                              sync.Mutex
	issues                          map[int]restIssue
	comments                        map[int][]restComment
	edges                           map[int][]int
	creates, edgeWrites, bodyWrites int
	failBody                        bool
	server                          *httptest.Server
}

func prerequisiteRequest() connector.HumanPrerequisiteRequest {
	return connector.HumanPrerequisiteRequest{Title: "Enable test account", Task: connector.HumanTask{Schema: 1, Key: "test-account", Action: "Enable test account authentication", Owner: "Account administrator", CompletionCriteria: "Authentication verified in test tenant", ApprovalConstraint: "Publishing requires separate approval"}}
}

func prerequisiteBody(t *testing.T) string {
	t.Helper()
	data, err := yaml.Marshal(prerequisiteRequest().Task)
	if err != nil {
		t.Fatal(err)
	}
	return "```detent-human\n" + string(data) + "```\n"
}

func newPrerequisiteTracker(t *testing.T) *prerequisiteTracker {
	t.Helper()
	tracker := &prerequisiteTracker{issues: map[int]restIssue{}, edges: map[int][]int{}, comments: map[int][]restComment{}}
	for n := 1; n <= 4; n++ {
		body := "Independent acceptance criteria\n\nDepends on: owner/repo#9\n"
		tracker.issues[n] = restIssue{ID: n, NodeID: fmt.Sprintf("I_%d", n), Number: n, State: "open", Title: "Implement feature", Body: &body, Labels: []label{{Name: "detent:todo"}}}
	}
	body := "Technical prerequisite complete"
	tracker.issues[9] = restIssue{ID: 9, NodeID: "I_9", Number: 9, State: "closed", Body: &body}
	tracker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/issues")
		write := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		if path == "" {
			if r.Method == http.MethodGet {
				var rows []restIssue
				for _, issue := range tracker.issues {
					rows = append(rows, issue)
				}
				slices.SortFunc(rows, func(a, b restIssue) int { return a.Number - b.Number })
				write(rows)
				return
			}
			if r.Method == http.MethodPost {
				var draft struct {
					Title, Body string
					Labels      []string
				}
				if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
					t.Error(err)
					return
				}
				n := 100 + tracker.creates
				tracker.creates++
				issue := restIssue{ID: n, NodeID: fmt.Sprintf("I_%d", n), Number: n, State: "open", Title: draft.Title, Body: &draft.Body}
				for _, name := range draft.Labels {
					issue.Labels = append(issue.Labels, label{Name: name})
				}
				tracker.issues[n] = issue
				write(issue)
				return
			}
		}
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		n, _ := strconv.Atoi(parts[0])
		issue, ok := tracker.issues[n]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		if len(parts) == 1 {
			if r.Method == http.MethodPatch {
				if tracker.failBody {
					tracker.failBody = false
					http.Error(w, `{"message":"body temporarily forbidden"}`, http.StatusForbidden)
					return
				}
				var body struct{ Body, State string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				issue.Body = &body.Body
				if body.State != "" {
					issue.State = body.State
				}
				tracker.issues[n] = issue
				tracker.bodyWrites++
			}
			write(issue)
			return
		}
		switch parts[1] {
		case "comments":
			write(tracker.comments[n])
		case "timeline":
			write([]any{})
		case "labels":
			var input struct{ Labels []string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			issue.Labels = nil
			for _, name := range input.Labels {
				issue.Labels = append(issue.Labels, label{Name: name})
			}
			tracker.issues[n] = issue
			write(issue.Labels)
		case "dependencies":
			if len(parts) >= 3 && parts[2] == "blocking" {
				rows := []restIssue{}
				for dependent, refs := range tracker.edges {
					if slices.Contains(refs, n) {
						rows = append(rows, tracker.issues[dependent])
					}
				}
				write(rows)
				return
			}
			if r.Method == http.MethodDelete {
				id, _ := strconv.Atoi(parts[3])
				tracker.edges[n] = slices.DeleteFunc(tracker.edges[n], func(value int) bool { return value == id })
				tracker.edgeWrites++
				write(tracker.issues[id])
				return
			}
			if r.Method == http.MethodPost {
				var input struct {
					ID int `json:"issue_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					return
				}
				tracker.edges[n] = append(tracker.edges[n], input.ID)
				tracker.edgeWrites++
				write(tracker.issues[input.ID])
			} else {
				rows := []restIssue{}
				for _, id := range tracker.edges[n] {
					rows = append(rows, tracker.issues[id])
				}
				write(rows)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(tracker.server.Close)
	return tracker
}

func (tracker *prerequisiteTracker) connector(t *testing.T) *Connector {
	t.Helper()
	c, err := NewConnector(Config{Endpoint: tracker.server.URL, APIKey: "test-token", HTTPClient: tracker.server.Client(), Repository: "owner/repo", GitHubStatusSource: GitHubStatusSourceLabel, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, ObservedStates: []string{"Backlog"}, TerminalStates: []string{"Done"}})
	if err != nil {
		t.Fatal(err)
	}
	c.client.restBackoffs = newRESTBackoffRegistry()
	return c
}

func TestHumanPrerequisiteHydrationUsesCurrentEvidence(t *testing.T) {
	t.Parallel()
	tracker := newPrerequisiteTracker(t)
	c := tracker.connector(t)
	tracker.edges[1] = []int{8}
	for _, tt := range []struct {
		name, state, evidence string
		ready                 bool
	}{
		{name: "open", state: "open"},
		{name: "closed without evidence", state: "closed"},
		{name: "completion recorded", state: "closed", evidence: "Verified test tenant sign-in", ready: true},
		{name: "reopened", state: "open", evidence: "Previously verified sign-in"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(prerequisiteBody(t), "```\n", "completion_evidence: "+strconv.Quote(tt.evidence)+"\n```\n", 1)
			tracker.mu.Lock()
			tracker.issues[8] = restIssue{ID: 8, NodeID: "I_8", Number: 8, State: tt.state, Body: &body}
			tracker.mu.Unlock()
			native, err := c.restNativeBlockedByRefs(t.Context(), issueRef{Owner: "owner", Name: "repo", Number: 1})
			if err != nil || len(native) != 1 || !native[0].HumanOwned || native[0].HumanCompletionReady != tt.ready {
				t.Fatalf("native evidence = %+v, %v", native, err)
			}
			issues := []connector.Issue{{BlockedBy: []connector.BlockedRef{{Identifier: "owner/repo#8"}}}}
			if err := c.resolveBlockedByProjectState(t.Context(), issues); err != nil {
				t.Fatal(err)
			}
			prose := issues[0].BlockedBy[0]
			if !prose.HumanOwned || prose.HumanCompletionReady != tt.ready {
				t.Fatalf("prose evidence = %+v", prose)
			}
		})
	}
}

func TestHumanPrerequisiteCreationDisabled(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"", "owner/repo#10"} {
		t.Run(existing, func(t *testing.T) {
			tracker := newPrerequisiteTracker(t)
			request := prerequisiteRequest()
			request.ExistingIdentifier = existing
			if _, err := tracker.connector(t).EnsureHumanPrerequisite(t.Context(), "owner/repo#1", request); err == nil {
				t.Fatal("creation was not rejected")
			}
			if tracker.creates != 0 || tracker.edgeWrites != 0 || tracker.bodyWrites != 0 {
				t.Fatal("disabled tool mutated tracker")
			}
		})
	}
}
