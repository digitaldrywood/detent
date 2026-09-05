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
	tracker := &prerequisiteTracker{issues: map[int]restIssue{}, edges: map[int][]int{}}
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
				var body struct{ Body string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				issue.Body = &body.Body
				tracker.issues[n] = issue
				tracker.bodyWrites++
			}
			write(issue)
			return
		}
		switch parts[1] {
		case "comments", "timeline":
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

func TestEnsureHumanPrerequisiteConcurrentAndRestart(t *testing.T) {
	t.Parallel()
	tracker := newPrerequisiteTracker(t)
	c := tracker.connector(t)
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			result, err := c.EnsureHumanPrerequisite(t.Context(), fmt.Sprintf("owner/repo#%d", 1+i%4), prerequisiteRequest())
			if err != nil || result.Issue.Identifier != "owner/repo#100" {
				t.Errorf("ensure = %s, %v", result.Issue.Identifier, err)
			}
		})
	}
	workers.Wait()
	if _, err := tracker.connector(t).EnsureHumanPrerequisite(t.Context(), "owner/repo#1", prerequisiteRequest()); err != nil {
		t.Fatal(err)
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.creates != 1 || tracker.edgeWrites != 4 || tracker.bodyWrites != 4 {
		t.Fatalf("writes: creates=%d edges=%d bodies=%d", tracker.creates, tracker.edgeWrites, tracker.bodyWrites)
	}
	for n := 1; n <= 4; n++ {
		body := *tracker.issues[n].Body
		if !strings.HasPrefix(body, "Independent acceptance criteria\n\nDepends on: owner/repo#9\n") || strings.Count(body, "Depends on: owner/repo#100") != 1 {
			t.Fatalf("body was not preserved: %q", body)
		}
		if tracker.issues[n].Labels[0].Name != "detent:todo" {
			t.Fatal("dependent left Todo")
		}
	}
	if !slices.ContainsFunc(tracker.issues[100].Labels, func(label label) bool { return label.Name == "detent:backlog" }) {
		t.Fatal("human prerequisite is not Backlog")
	}
}

func TestEnsureHumanPrerequisitePartialWriteRetry(t *testing.T) {
	t.Parallel()
	tracker := newPrerequisiteTracker(t)
	tracker.failBody = true
	c := tracker.connector(t)
	if _, err := c.EnsureHumanPrerequisite(t.Context(), "owner/repo#1", prerequisiteRequest()); err == nil {
		t.Fatal("expected body failure")
	}
	if _, err := c.EnsureHumanPrerequisite(t.Context(), "owner/repo#1", prerequisiteRequest()); err != nil {
		t.Fatal(err)
	}
	if tracker.creates != 1 || tracker.edgeWrites != 1 || tracker.bodyWrites != 1 {
		t.Fatalf("retry duplicated writes: %+v", tracker)
	}
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

func TestEnsureHumanPrerequisiteRejectsInvalidEdges(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, dependent, existing, body string
		closed                          bool
	}{
		{name: "self", dependent: "owner/repo#1", existing: "#1"},
		{name: "cross repository", dependent: "owner/repo#1", existing: "private/repo#1"},
		{name: "foreign dependent", dependent: "private/repo#1"},
		{name: "malformed", dependent: "owner/repo#1bad"},
		{name: "closed software", dependent: "owner/repo#1", closed: true},
		{name: "cycle", dependent: "owner/repo#1", existing: "#8", body: "Depends on: #1"},
		{name: "malformed graph", dependent: "owner/repo#1", existing: "#8", body: "Depends on: #bad"},
		{name: "private graph", dependent: "owner/repo#1", existing: "#8", body: "Depends on: private/repo#2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newPrerequisiteTracker(t)
			body := prerequisiteBody(t) + "\n" + tt.body
			tracker.issues[8] = restIssue{ID: 8, NodeID: "I_8", Number: 8, State: "open", Body: &body}
			if tt.closed {
				issue := tracker.issues[1]
				issue.State = "closed"
				tracker.issues[1] = issue
			}
			request := prerequisiteRequest()
			request.ExistingIdentifier = tt.existing
			if _, err := tracker.connector(t).EnsureHumanPrerequisite(t.Context(), tt.dependent, request); err == nil {
				t.Fatal("invalid edge accepted")
			}
			if tracker.creates != 0 || tracker.edgeWrites != 0 || tracker.bodyWrites != 0 {
				t.Fatal("invalid request mutated tracker")
			}
		})
	}
}

func TestEnsureHumanPrerequisiteMigratesExistingIssue(t *testing.T) {
	t.Parallel()
	tracker := newPrerequisiteTracker(t)
	body := "Human task: enable test authentication. Publishing still needs approval.\n\nDepends on: #9\n"
	tracker.issues[8] = restIssue{ID: 8, NodeID: "I_8", Number: 8, State: "open", Body: &body}
	request := prerequisiteRequest()
	request.ExistingIdentifier = "#8"
	c := tracker.connector(t)
	for range 2 {
		result, err := c.EnsureHumanPrerequisite(t.Context(), "owner/repo#1", request)
		if err != nil || result.Created || result.Issue.Identifier != "owner/repo#8" {
			t.Fatalf("migration=%+v %v", result, err)
		}
	}
	if tracker.creates != 0 || tracker.edgeWrites != 1 || tracker.bodyWrites != 2 {
		t.Fatalf("migration writes=%d %d %d", tracker.creates, tracker.edgeWrites, tracker.bodyWrites)
	}
	if !strings.HasPrefix(*tracker.issues[8].Body, body) {
		t.Fatal("migration replaced human instructions")
	}
}

func TestEnsureHumanPrerequisiteContractFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*connector.HumanPrerequisiteRequest, *prerequisiteTracker)
	}{
		{name: "invalid schema", change: func(r *connector.HumanPrerequisiteRequest, _ *prerequisiteTracker) { r.Task.Schema = 2 }},
		{name: "empty title", change: func(r *connector.HumanPrerequisiteRequest, _ *prerequisiteTracker) { r.Title = "" }},
		{name: "worker completion", change: func(r *connector.HumanPrerequisiteRequest, _ *prerequisiteTracker) {
			r.Task.CompletionEvidence = "done"
		}},
		{name: "missing existing", change: func(r *connector.HumanPrerequisiteRequest, _ *prerequisiteTracker) {
			r.ExistingIdentifier = "#700"
			r.Task.Key = "another-key"
		}},
		{name: "changed approval", change: func(r *connector.HumanPrerequisiteRequest, _ *prerequisiteTracker) {
			r.ExistingIdentifier = "#8"
			r.Task.ApprovalConstraint = "Approved"
		}},
		{name: "duplicate registry", change: func(_ *connector.HumanPrerequisiteRequest, tr *prerequisiteTracker) {
			issue := tr.issues[8]
			issue.ID = 7
			issue.Number = 7
			issue.NodeID = "I_7"
			tr.issues[7] = issue
		}},
		{name: "invalid existing marker", change: func(r *connector.HumanPrerequisiteRequest, tr *prerequisiteTracker) {
			r.ExistingIdentifier = "#8"
			issue := tr.issues[8]
			body := "```detent-human\nschema: 2\nkey: test-account\n```"
			issue.Body = &body
			tr.issues[8] = issue
		}},
		{name: "closed prose only", change: func(r *connector.HumanPrerequisiteRequest, tr *prerequisiteTracker) {
			r.ExistingIdentifier = "#8"
			issue := tr.issues[8]
			issue.State = "closed"
			body := "old task"
			issue.Body = &body
			tr.issues[8] = issue
		}},
		{name: "malformed dependent edge", change: func(_ *connector.HumanPrerequisiteRequest, tr *prerequisiteTracker) {
			issue := tr.issues[1]
			body := "Depends on: #bad"
			issue.Body = &body
			tr.issues[1] = issue
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newPrerequisiteTracker(t)
			body := prerequisiteBody(t)
			tr.issues[8] = restIssue{ID: 8, NodeID: "I_8", Number: 8, State: "open", Body: &body}
			request := prerequisiteRequest()
			tt.change(&request, tr)
			if _, err := tr.connector(t).EnsureHumanPrerequisite(t.Context(), "owner/repo#1", request); err == nil {
				t.Fatal("invalid contract accepted")
			}
			if tr.creates != 0 || tr.edgeWrites != 0 || tr.bodyWrites != 0 {
				t.Fatal("contract failure mutated tracker")
			}
		})
	}
}
