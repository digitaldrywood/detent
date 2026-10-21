package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const decisionCompletion = "```detent-status\n" + `schema: 1
status: blocked
human_action: Choose the installer naming rule.
decision_request:
  summary: The installer is ready for the naming decision.
  question: Which naming rule should the installer use?
  options:
    - label: Use a derived name
      consequence: New installs derive their name from the Hub URL.
      recommended: true
      return_state: Rework
    - label: Require an explicit name
      consequence: Operators supply the name for new installs.
      recommended: false
      return_state: Todo
  custom_allowed: true
  custom_return_state: Rework
` + "```"

func TestNativeDecisionRoundTripAndAnswer(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "Todo", "Done"}},
		{Name: "Blocked", Transitions: []string{"Todo", "Rework"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", string(raw), f.project.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range states[2:4] {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'))`, f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable); err != nil {
			t.Fatal(err)
		}
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "worker")
	for index, name := range []string{"option", "custom"} {
		t.Run(name, func(t *testing.T) {
			issue := f.create(t, name)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			started := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "progress-" + name}, ExpectedRevision: issue.Revision, State: "In Progress", Reason: "user_requested"})
			requireNativeStatus(t, started, http.StatusOK)
			decodeHubResponse(t, started, &issue)
			lease := claimNativeAttempt(t, f, worker, "machine-"+name, "session-"+name, issue.WorkItemID)
			start := nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish-"+name, 2, "succeeded"
			finish.Data.CompletionBody = decisionCompletion
			finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			move := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "block-" + name}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "user_requested"}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, move)
			requireNativeStatus(t, response, http.StatusOK)
			read := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, read, http.StatusOK)
			decodeHubResponse(t, read, &issue)
			if issue.DecisionRequest == nil || issue.DecisionRequest.AttemptID != finish.Data.AttemptID || len(issue.DecisionRequest.Options) != 2 || issue.HumanAction != "Choose the installer naming rule." {
				t.Fatalf("issue attention = %#v", issue)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/explanation", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var explanation explain.IssueExplanation
			decodeHubResponse(t, response, &explanation)
			if !reflect.DeepEqual(explanation.DecisionRequest, issue.DecisionRequest) {
				t.Fatalf("explanation request = %#v, item = %#v", explanation.DecisionRequest, issue.DecisionRequest)
			}
			answer := &tracker.DecisionAnswer{AttemptID: finish.Data.AttemptID}
			state, want := "Todo", "Require an explicit name"
			if index == 0 {
				selected := 1
				answer.Option = &selected
			} else {
				answer.Custom = "Use a project-specific name"
				state, want = "Rework", answer.Custom
			}
			move = tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "answer-" + name}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested", DecisionAnswer: answer}
			wrong := move
			wrong.IdempotencyKey, wrong.State = "wrong-"+name, "Todo"
			if state == "Todo" {
				wrong.State = "Rework"
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, wrong), http.StatusUnprocessableEntity)
			stale := move
			staleAnswer := *answer
			staleAnswer.AttemptID = "attempt_old"
			stale.IdempotencyKey, stale.DecisionAnswer = "stale-"+name, &staleAnswer
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, stale), http.StatusUnprocessableEntity)
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, move)
			requireNativeStatus(t, response, http.StatusOK)
			issue = tracker.NativeIssue{}
			decodeHubResponse(t, response, &issue)
			if issue.State != state || issue.DecisionRequest != nil {
				t.Fatalf("answered issue = %#v", issue)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, move), http.StatusOK)
			var count int
			var body string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*), COALESCE(max(body), '') FROM native_comments WHERE work_item_id = ?", issue.WorkItemID).Scan(&count, &body); err != nil {
				t.Fatal(err)
			}
			if count != 1 || !strings.Contains(body, want) || !strings.Contains(body, "Which naming rule") {
				t.Fatalf("answer comments = %d, body = %q", count, body)
			}
			if state == "Rework" {
				move = tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "another-hold"}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "user_requested"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, move), http.StatusOK)
				read = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, read, http.StatusOK)
				issue = tracker.NativeIssue{}
				decodeHubResponse(t, read, &issue)
				if issue.DecisionRequest != nil || issue.HumanAction != "" {
					t.Fatal("a later hold resurrected the answered decision")
				}
			}
		})
	}
	var count int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM decision_requests").Scan(&count); err != nil || count != 2 {
		t.Fatalf("persisted requests = %d, error = %v", count, err)
	}
}

func TestNativeDecisionCompletionRefusal(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "missing question")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	for _, tt := range []struct{ name, body string }{
		{"missing request", "```detent-status\nschema: 1\nstatus: blocked\nhuman_action: Choose a name\n```"},
		{"malformed request", strings.Replace(decisionCompletion, "  question: Which naming rule should the installer use?\n", "", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Outcome = "run.finished", "invalid-"+tt.name, "succeeded"
			finish.Data.CompletionBody, finish.Data.Disposition = tt.body, &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if !strings.Contains(response.Body.String(), `"field":"data.completion_body"`) {
				t.Fatalf("refusal = %s", response.Body.String())
			}
		})
	}
}
