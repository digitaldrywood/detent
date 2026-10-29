package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// TestConversationRunnerExecutesLiveTurns drives a conversation turn end to
// end: a real hub service and database, the production worker client and
// scheduler, and the production runner turn path over a scripted live
// backend. The first attempt asks a question the operator answers and is
// steered mid-turn; the second attempt continues the conversation and is
// interrupted.
func TestConversationRunnerExecutesLiveTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	useFastConversationTimings(t)
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary=%t", ordinary), func(t *testing.T) {
			hub := newConversationRunnerHub(t)

			var id, issueID string
			if ordinary {
				var issue tracker.NativeIssue
				hub.expect(t, http.MethodPost, "/work-items", map[string]any{"idempotency_key": "ordinary-1", "title": "Rewrite the parser", "body": "Split the parser into a lexer and a parser." + issueContractTestSections, "state": "Todo"}, http.StatusOK, &issue)
				issueID = string(issue.WorkItemID)
				hub.expect(t, http.MethodPost, "/work-items/"+issueID+"/comments", map[string]any{"idempotency_key": "comment-1", "body": "Historical issue comment: do not steer this turn."}, http.StatusOK, nil)
			} else {
				var created struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				hub.expect(t, http.MethodPost, "/conversations", map[string]any{"key": "create-1", "title": "Parser"}, http.StatusCreated, &created)
				id = created.Conversation.ID
				var link struct {
					Issue struct {
						ID string `json:"id"`
					} `json:"issue"`
				}
				hub.expect(t, http.MethodPost, "/conversations/"+id+"/link", map[string]any{
					"key": "link-1", "share_history": true,
					"issue": map[string]any{"title": "Rewrite the parser", "description": "Split the parser into a lexer and a parser." + issueContractTestSections},
				}, http.StatusOK, &link)
				issueID = link.Issue.ID
				if receipt := hub.command(t, id, conversation.Command{Key: "follow-up", Kind: conversation.CommandMessage, Text: "Keep the public API stable."}); receipt.Status != conversation.DeliveryQueued {
					t.Fatalf("follow-up receipt = %#v, want queued", receipt)
				}
			}

			backend := &liveConversationBackend{started: make(chan struct{}, 2), waitDone: testenv.WaitContext(t, t.Context()).Done()}
			scheduler := hub.scheduler(t)
			first := hub.startAttempt(t, scheduler, backend, issueID)
			select {
			case <-backend.started:
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("worker did not start a provider turn")
			}
			var canonical runnerSnapshot
			hub.expect(t, http.MethodGet, "/work-items/"+issueID+"/conversation", nil, http.StatusOK, &canonical)
			if ordinary {
				id = canonical.Conversation.ID
			} else if id != canonical.Conversation.ID {
				t.Fatal("conversation-origin worker lost its canonical conversation")
			}

			waiting := hub.await(t, id, "the first question", func(s runnerSnapshot) bool {
				return s.Conversation.Execution.Status == conversation.ExecutionWaitingInput && s.pendingQuestion() != ""
			})
			firstAttempt := waiting.attempt()
			if len(backend.recordedRequests()) != 1 {
				t.Fatal("binding dispatched another provider turn")
			}
			var stale struct {
				Code string `json:"code"`
			}
			hub.expect(t, http.MethodPost, "/conversations/"+id+"/commands", conversation.Command{
				Key: "stale-turn", Kind: conversation.CommandMessage, Text: "Do not deliver",
				Expected: conversation.Expected{AttemptID: firstAttempt, TurnID: "past-turn"},
			}, http.StatusConflict, &stale)
			if stale.Code != "stale_execution" {
				t.Fatalf("stale turn result = %q", stale.Code)
			}
			if capabilities := waiting.Conversation.Execution.Capabilities; !capabilities.Steer || !capabilities.Interrupt || !capabilities.Answer {
				t.Fatalf("capabilities = %#v, want the runner's live control set", capabilities)
			}
			if delivery := waiting.delivery("follow-up"); !ordinary && delivery != conversation.DeliveryDelivered {
				t.Fatalf("queued follow-up delivery = %q, want delivered with the first turn", delivery)
			}
			hub.command(t, id, conversation.Command{
				Key: "answer-1", Kind: conversation.CommandAnswer, QuestionID: waiting.pendingQuestion(),
				Answers: map[string][]string{"approach": {"Incremental"}}, Expected: conversation.Expected{AttemptID: firstAttempt},
			})
			hub.await(t, id, "the answered turn to continue", func(s runnerSnapshot) bool {
				return strings.Contains(s.assistantText(), "Taking the Incremental approach.") && s.Conversation.Execution.Status == conversation.ExecutionRunning
			})
			hub.command(t, id, conversation.Command{Key: "steer-1", Kind: conversation.CommandMessage, Text: "Also update the changelog.", Expected: conversation.Expected{AttemptID: firstAttempt, TurnID: "turn-1"}})
			if outcome := awaitRunnerAttempt(t, first); outcome.err != nil || outcome.result.FinalState != runner.FinalStateCompleted {
				t.Fatalf("first attempt = %#v, error = %v", outcome.result.FinalState, outcome.err)
			}
			if got := scheduler.RunExecution(issueID).(*nativeExecution).conversationContinuation; got == ordinary {
				t.Fatalf("initial continuation owner = %t, ordinary = %t", got, ordinary)
			}
			if err := scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
				t.Fatal(err)
			}
			completed := hub.await(t, id, "the first execution to complete", func(s runnerSnapshot) bool {
				return s.Conversation.Execution.Status == conversation.ExecutionCompleted
			})
			if text := completed.assistantText(); !strings.Contains(text, "Noted: Also update the changelog.") {
				t.Fatalf("assistant text = %q, want the steered reply", text)
			}
			if delivery := completed.delivery("steer-1"); delivery != conversation.DeliveryDelivered {
				t.Fatalf("steer delivery = %q, want delivered", delivery)
			}
			if status := completed.questionStatus(waiting.pendingQuestion()); status != conversation.QuestionAnswered {
				t.Fatalf("question status = %q, want answered", status)
			}

			if receipt := hub.command(t, id, conversation.Command{Key: "continue-1", Kind: conversation.CommandContinue, Text: "Continue with the release notes.", Expected: conversation.Expected{AttemptID: firstAttempt}}); receipt.Status != conversation.DeliverySaved {
				t.Fatalf("continue receipt = %#v, want saved", receipt)
			}
			second := hub.startAttempt(t, scheduler, backend, issueID)
			running := hub.await(t, id, "the continuation to stream", func(s runnerSnapshot) bool {
				return s.Conversation.Execution.Status == conversation.ExecutionRunning && strings.Contains(s.assistantText(), "Drafting the release notes.")
			})
			secondAttempt := running.attempt()
			if secondAttempt == firstAttempt {
				t.Fatalf("second attempt reused the first attempt id %s", firstAttempt)
			}
			hub.command(t, id, conversation.Command{Key: "interrupt-1", Kind: conversation.CommandInterrupt, Expected: conversation.Expected{AttemptID: secondAttempt}})
			if outcome := awaitRunnerAttempt(t, second); outcome.err != nil {
				t.Fatalf("second attempt error = %v", outcome.err)
			}
			if !scheduler.RunExecution(issueID).(*nativeExecution).conversationContinuation {
				t.Fatal("explicit continuation lost its completion owner")
			}
			if err := scheduler.ReleaseClaim(t.Context(), issueID, "interrupted"); err != nil {
				t.Fatal(err)
			}
			interrupted := hub.await(t, id, "the interrupted execution", func(s runnerSnapshot) bool {
				return s.Conversation.Execution.Status == conversation.ExecutionInterrupted && s.attempt() == secondAttempt
			})
			if delivery := interrupted.delivery("interrupt-1"); delivery != conversation.DeliveryDelivered {
				t.Fatalf("interrupt delivery = %q, want delivered", delivery)
			}

			requests := backend.recordedRequests()
			if len(requests) != 2 {
				t.Fatalf("provider turns = %d, want one per attempt", len(requests))
			}
			if !ordinary && !strings.Contains(requests[0].Prompt, "Keep the public API stable.") {
				t.Fatalf("first turn prompt lost the queued follow-up: %q", requests[0].Prompt)
			}
			if !strings.Contains(requests[1].Prompt, "Continue with the release notes.") {
				t.Fatalf("continuation prompt lost the continue text: %q", requests[1].Prompt)
			}
			kinds := backend.consumedKinds()
			if want := []runner.AgentControlKind{runner.AgentControlAnswer, runner.AgentControlMessage, runner.AgentControlInterrupt}; fmt.Sprint(kinds) != fmt.Sprint(want) {
				t.Fatalf("consumed controls = %v, want %v", kinds, want)
			}
			if ordinary {
				for _, message := range completed.Messages {
					if strings.Contains(message.Text, "Historical issue comment") {
						t.Fatal("historical issue comment became a conversation message")
					}
				}
				var comments tracker.Page[tracker.NativeComment]
				hub.expect(t, http.MethodGet, "/work-items/"+issueID+"/comments", nil, http.StatusOK, &comments)
				if len(comments.Items) != 1 || comments.Items[0].Body != "Historical issue comment: do not steer this turn." {
					t.Fatalf("issue comments changed: %#v", comments)
				}
			}
		})
	}

	t.Run("completion before steering handoff", func(t *testing.T) {
		hub := newConversationRunnerHub(t)
		ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
		defer cancel()
		running, completed, acknowledged := make(chan struct{}), make(chan struct{}), make(chan struct{})
		handed := make(chan ConversationControlsPage, 1)
		release := make(chan struct{})
		var runningOnce, completedOnce, acknowledgedOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		transport := hub.httpClient.Transport
		hub.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			var events ConversationTurnEventsRequest
			if strings.HasSuffix(request.URL.Path, "/turn-events") {
				payload, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				request.Body = io.NopCloser(bytes.NewReader(payload))
				if err := json.Unmarshal(payload, &events); err != nil {
					return nil, err
				}
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode < 300 {
				for _, event := range events.Events {
					switch {
					case event.Type == runner.ConversationEventExecutionStatus && event.Status == "running":
						runningOnce.Do(func() { close(running) })
					case event.Type == runner.ConversationEventTurnCompleted:
						completedOnce.Do(func() { close(completed) })
					}
				}
			}
			if strings.HasSuffix(request.URL.Path, "/controls") && response.StatusCode == http.StatusOK {
				payload, err := io.ReadAll(response.Body)
				if err != nil {
					return nil, err
				}
				if err := response.Body.Close(); err != nil {
					return nil, err
				}
				response.Body = io.NopCloser(bytes.NewReader(payload))
				var page ConversationControlsPage
				if err := json.Unmarshal(payload, &page); err != nil {
					return nil, err
				}
				if len(page.Controls) > 0 {
					handed <- page
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}
				if request.URL.Query().Get("after") != "0" {
					acknowledgedOnce.Do(func() { close(acknowledged) })
				}
			}
			return response, nil
		})
		wait := func(event <-chan struct{}) {
			t.Helper()
			select {
			case <-event:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		var issue tracker.NativeIssue
		hub.expect(t, http.MethodPost, "/work-items", map[string]any{"idempotency_key": "handoff-issue", "title": "Preserve the parser API", "body": "Keep the parser interface stable." + issueContractTestSections, "state": "Todo"}, http.StatusOK, &issue)
		scheduler := hub.scheduler(t)
		candidates, err := scheduler.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{Policy: hub.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}})
		if err != nil || len(candidates) != 1 {
			t.Fatalf("candidates = %#v, error = %v", candidates, err)
		}
		if _, err := scheduler.AdoptClaim(ctx, candidates[0], time.Now()); err != nil {
			t.Fatal(err)
		}
		execution := scheduler.RunExecution(string(issue.WorkItemID)).(*nativeExecution)
		if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
			t.Fatal(err)
		}
		session, err := execution.BindConversation(ctx, runner.ConversationCapabilities{Steer: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			unblock()
			if err := session.Close(ctx, runner.ConversationOutcomeSucceeded, nil); err != nil {
				t.Error(err)
			}
		}()
		control := session.Control(runner.ConversationTurnHooks{})
		if err := session.Report(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventExecutionStatus, Status: "running"}}); err != nil {
			t.Fatal(err)
		}
		wait(running)
		id := session.ConversationID()
		var before runnerSnapshot
		hub.expect(t, http.MethodGet, "/conversations/"+id, nil, http.StatusOK, &before)
		if before.Conversation.Execution.Status != conversation.ExecutionRunning || before.Conversation.Execution.TurnID != nil {
			t.Fatalf("initial execution = %#v", before.Conversation.Execution)
		}
		t.Logf("before acceptance: running, turn_id absent, steer=%t", before.Conversation.Execution.Capabilities.Steer)
		operator, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return hub.operator }, HTTPClient: hub.httpClient})
		if err != nil {
			t.Fatal(err)
		}
		command := conversation.Command{Key: "handoff-steer", Kind: conversation.CommandMessage, Text: "Use the attached evidence", Expected: conversation.Expected{AttemptID: before.attempt()}}
		var receipt conversation.Receipt
		commandErr := operator.request(ctx, http.MethodPost, hub.base+"/conversations/"+id+"/commands", command, &receipt)
		if commandErr == nil {
			if receipt.Status != conversation.DeliveryQueued || !before.Conversation.Execution.Capabilities.Steer {
				t.Fatalf("accepted control = %#v, capabilities = %#v", receipt, before.Conversation.Execution.Capabilities)
			}
			select {
			case page := <-handed:
				if len(page.Controls) != 1 || page.Controls[0].Expected.AttemptID != before.attempt() || page.Controls[0].Expected.TurnID != "" {
					t.Fatalf("control handoff = %#v", page)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			t.Log("message accepted as queued; controls response held before reaching the turn")
		} else {
			var failure *APIError
			if !errors.As(commandErr, &failure) || failure.Code != "stale_execution" || before.Conversation.Execution.Capabilities.Steer {
				t.Fatalf("steering refusal = %v, capabilities = %#v", commandErr, before.Conversation.Execution.Capabilities)
			}
		}
		if err := session.Report(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, ThreadID: "thread-handoff", TurnID: "turn-handoff", Status: "completed"}}); err != nil {
			t.Fatal(err)
		}
		session.FinishTurn()
		wait(completed)
		select {
		case command := <-control.Commands:
			t.Fatalf("control reached the provider before completion: %#v", command)
		default:
		}
		t.Log("provider completed; FinishTurn returned with no control available to its transport")
		unblock()
		if commandErr == nil {
			wait(acknowledged)
		}
		if err := session.Close(ctx, runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Fatal(err)
		}
		var after runnerSnapshot
		hub.expect(t, http.MethodGet, "/conversations/"+id, nil, http.StatusOK, &after)
		if after.Conversation.Execution.Status != conversation.ExecutionCompleted {
			t.Fatalf("terminal execution = %#v", after.Conversation.Execution)
		}
		if commandErr == nil {
			if len(after.Messages) != 1 || after.Messages[0].ID != receipt.MessageID || after.Messages[0].Text != command.Text || after.Messages[0].Delivery != conversation.DeliveryUnknown || after.Messages[0].AttemptID == nil || *after.Messages[0].AttemptID != before.attempt() {
				t.Fatalf("retained undelivered message = %#v", after.Messages)
			}
			t.Fatalf("accepted steering before provider completion never reached the transport: queued -> unknown; message retained; late handoff settled by production Close")
		}
		if len(after.Messages) != 0 {
			t.Fatalf("refused steering persisted a message: %#v", after.Messages)
		}
	})

}

const conversationRunnerAdminToken = "conversation-runner-admin"

type conversationRunnerHub struct {
	httpClient   *http.Client
	organization tracker.OrganizationID
	project      tracker.ProjectID
	base         string
	operator     string
	worker       string
	descriptor   policy.Descriptor
}

func newConversationRunnerHub(t *testing.T) *conversationRunnerHub {
	t.Helper()
	service, err := hubserver.Open(t.Context(), hubserver.Config{
		RunDetails:        &archivedRunDetails{},
		DatabasePath:      hubDatabasePath(t),
		InitialAdminToken: []byte(conversationRunnerAdminToken),
		Content:           newHubContentStore(),
		Conversation:      &hubserver.ConversationConfig{Enabled: true, ArchiveContents: &conversationArchiveFixture{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	handler := service.Handler()
	httpClient := &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := recorder.Result()
		response.Request = request
		return response, nil
	})}
	admin, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return conversationRunnerAdminToken }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := admin.request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	h := &conversationRunnerHub{httpClient: httpClient, organization: organizations.Items[0].ID, descriptor: clientTestPolicy()}
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}
	var project tracker.NativeProject
	body := map[string]any{"name": "product", "idempotency_key": "project-product", "states": states, "require_dependencies": false}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", body, &project); err != nil {
		t.Fatal(err)
	}
	h.project = project.ID
	h.base = "/api/v2/organizations/" + string(h.organization) + "/projects/" + string(h.project)
	h.operator = h.createToken(t, admin, "operator", "operator")
	h.worker = h.createToken(t, admin, "worker", "worker")
	native, err := admin.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *conversationRunnerHub) createToken(t *testing.T, admin *Client, name, scope string) string {
	t.Helper()
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": name, "scope": scope}, &token); err != nil {
		t.Fatal(err)
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	return token.Token
}

// expect performs one operator request over real HTTP, requires the status
// and decodes the body.
func (h *conversationRunnerHub) expect(t *testing.T, method, path string, body any, status int, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, "http://hub.test"+h.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.operator)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := h.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, status, payload)
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			t.Fatalf("decode %s %s: %v: %s", method, path, err, payload)
		}
	}
}

func (h *conversationRunnerHub) command(t *testing.T, id string, command conversation.Command) conversation.Receipt {
	t.Helper()
	var receipt conversation.Receipt
	h.expect(t, http.MethodPost, "/conversations/"+id+"/commands", command, http.StatusOK, &receipt)
	return receipt
}

type runnerSnapshot struct {
	Conversation struct {
		ID        string `json:"id"`
		Execution struct {
			Status       conversation.ExecutionStatus `json:"status"`
			AttemptID    *string                      `json:"attempt_id"`
			TurnID       *string                      `json:"turn_id"`
			Capabilities conversation.Capabilities    `json:"capabilities"`
		} `json:"execution"`
	} `json:"conversation"`
	Messages []struct {
		ID         string                   `json:"id"`
		AttemptID  *string                  `json:"attempt_id"`
		Role       conversation.Role        `json:"role"`
		Kind       conversation.MessageKind `json:"kind"`
		Text       string                   `json:"text"`
		Delivery   conversation.Delivery    `json:"delivery"`
		CommandKey *string                  `json:"command_key"`
	} `json:"messages"`
	Questions []struct {
		ID     string                      `json:"id"`
		Status conversation.QuestionStatus `json:"status"`
	} `json:"questions"`
}

func (s runnerSnapshot) attempt() string {
	if s.Conversation.Execution.AttemptID == nil {
		return ""
	}
	return *s.Conversation.Execution.AttemptID
}

func (s runnerSnapshot) pendingQuestion() string {
	for _, question := range s.Questions {
		if question.Status == conversation.QuestionPending {
			return question.ID
		}
	}
	return ""
}

func (s runnerSnapshot) questionStatus(id string) conversation.QuestionStatus {
	for _, question := range s.Questions {
		if question.ID == id {
			return question.Status
		}
	}
	return ""
}

func (s runnerSnapshot) delivery(key string) conversation.Delivery {
	for _, message := range s.Messages {
		if message.CommandKey != nil && *message.CommandKey == key {
			return message.Delivery
		}
	}
	return ""
}

func (s runnerSnapshot) assistantText() string {
	var text strings.Builder
	for _, message := range s.Messages {
		if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageText {
			text.WriteString(message.Text)
		}
	}
	return text.String()
}

// await polls the snapshot until want is satisfied.
func (h *conversationRunnerHub) await(t *testing.T, id, reason string, want func(runnerSnapshot) bool) runnerSnapshot {
	t.Helper()
	var last runnerSnapshot
	testenv.WaitUntil(t, t.Context(), func() error {
		last = runnerSnapshot{}
		h.expect(t, http.MethodGet, "/conversations/"+id, nil, http.StatusOK, &last)
		if want(last) {
			return nil
		}
		return fmt.Errorf("snapshot never satisfied %s: execution %q, %d messages, assistant %q", reason, last.Conversation.Execution.Status, len(last.Messages), last.assistantText())
	})
	return last
}

func (h *conversationRunnerHub) scheduler(t *testing.T) *Scheduler {
	t.Helper()
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return h.worker }, HTTPClient: h.httpClient})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		OrganizationID:    h.organization,
		NativeProjects:    map[string]tracker.ProjectID{"local": h.project},
		Machine:           Machine{ID: "machine-conversation", Hostname: "host", Capacity: 1, Version: "test"},
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

type runnerAttemptOutcome struct {
	result runner.RunResult
	err    error
}

// startAttempt claims the linked issue and runs it through the production
// runner in the background, as the orchestrator's dispatch path does.
func (h *conversationRunnerHub) startAttempt(t *testing.T, scheduler *Scheduler, backend runner.AgentBackend, issueID string) <-chan runnerAttemptOutcome {
	t.Helper()
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want the linked issue %s", candidates, issueID)
	}
	if _, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	execution := scheduler.RunExecution(issueID)
	if execution == nil {
		t.Fatal("claimed native issue has no execution lifecycle")
	}
	agent, err := runner.NewRunner(runner.Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Complete the linked issue"},
		Workspace:    &conversationStubWorkspace{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}},
		AgentBackend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidates[0], Mode: runner.RunModePlan}
	done := make(chan runnerAttemptOutcome, 1)
	go func() {
		result, err := agent.Run(t.Context(), request)
		done <- runnerAttemptOutcome{result: result, err: err}
	}()
	return done
}

func awaitRunnerAttempt(t *testing.T, done <-chan runnerAttemptOutcome) runnerAttemptOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("attempt did not finish")
	}
	return runnerAttemptOutcome{}
}

// conversationStubWorkspace is the smallest workspace backend the runner
// accepts; the conversation turn never touches a repository.
type conversationStubWorkspace struct {
	info workspace.Info
}

func (w *conversationStubWorkspace) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	info := w.info
	info.Branch = issue.BranchName
	return info, nil
}

func (w *conversationStubWorkspace) Cleanup(context.Context, string) error { return nil }

func (w *conversationStubWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (w *conversationStubWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {}

func (w *conversationStubWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}

func (w *conversationStubWorkspace) RecoveryState(context.Context, workspace.Info, workspace.Issue) (workspace.RecoveryState, error) {
	return workspace.RecoveryState{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: strings.Repeat("b", 64), BaseFingerprint: strings.Repeat("c", 64)}, nil
}

// liveConversationBackend is a scripted live-control provider. Its first turn
// asks a question, waits for the answer and then for a steer; its second turn
// streams until it is interrupted.
type liveConversationBackend struct {
	started  chan struct{}
	waitDone <-chan struct{}
	mu       sync.Mutex
	requests []runner.AgentTurnRequest
	consumed []runner.AgentControlKind
}

const (
	liveThreadID = "thread-live-1"
	liveQuestion = `[{"id":"approach","header":"Approach","question":"Which approach should I take?","options":[{"label":"Incremental","description":"Small steps"},{"label":"Rewrite","description":"Start over"}]}]`
)

func (*liveConversationBackend) SupportsLiveControl() bool { return true }

func (b *liveConversationBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	turn := len(b.requests)
	b.mu.Unlock()
	if b.started != nil {
		b.started <- struct{}{}
	}
	if request.ConversationControl == nil {
		return runner.AgentTurnResult{}, errors.New("turn ran without a conversation control")
	}
	if turn == 1 {
		return b.answeredAndSteeredTurn(ctx, request, onUpdate)
	}
	return b.interruptedTurn(ctx, request, onUpdate)
}

func (b *liveConversationBackend) answeredAndSteeredTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	const turnID = "turn-1"
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: liveThreadID, TurnID: turnID}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := streamLiveDeltas(onUpdate, liveThreadID, turnID, "item-1", "Reading the issue. ", "One question first. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := request.ConversationControl.InputRequested(runner.AgentInputRequest{ID: "req-1", ThreadID: liveThreadID, TurnID: turnID, Questions: json.RawMessage(liveQuestion)}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	answer, err := b.consume(ctx, request, runner.AgentControlAnswer)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	if answer.RequestID != "req-1" {
		return runner.AgentTurnResult{}, fmt.Errorf("answer request = %q, want req-1", answer.RequestID)
	}
	choice := strings.Join(answer.Answers["approach"], ",")
	if err := streamLiveDeltas(onUpdate, liveThreadID, turnID, "item-2", "Taking the "+choice+" approach. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	steer, err := b.consume(ctx, request, runner.AgentControlMessage)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := streamLiveDeltas(onUpdate, liveThreadID, turnID, "item-3", "Noted: "+steer.Text); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: liveThreadID, TurnID: turnID, Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: liveThreadID, TurnID: turnID}, nil
}

func (b *liveConversationBackend) interruptedTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	const turnID = "turn-2"
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: liveThreadID, TurnID: turnID}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := streamLiveDeltas(onUpdate, liveThreadID, turnID, "item-4", "Drafting the release notes. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if _, err := b.consume(ctx, request, runner.AgentControlInterrupt); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: liveThreadID, TurnID: turnID, Status: "interrupted"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: liveThreadID, TurnID: turnID}, nil
}

func streamLiveDeltas(onUpdate runner.AgentUpdateHandler, thread, turn, item string, deltas ...string) error {
	for _, delta := range deltas {
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ThreadID: thread, TurnID: turn, ItemID: item, Delta: delta}); err != nil {
			return err
		}
	}
	return nil
}

// consume waits for the next control, runs its ownership check exactly as a
// real transport does immediately before writing to the provider, and settles
// its reply. A control of another kind is consumed and recorded too.
func (b *liveConversationBackend) consume(ctx context.Context, request runner.AgentTurnRequest, kind runner.AgentControlKind) (runner.AgentControl, error) {
	for {
		select {
		case command, ok := <-request.ConversationControl.Commands:
			if !ok {
				return runner.AgentControl{}, errors.New("conversation control queue closed")
			}
			if err := command.Validate(); err != nil {
				command.Reply <- err
				return runner.AgentControl{}, err
			}
			wantTurn := "turn-1"
			if kind == runner.AgentControlInterrupt {
				wantTurn = "turn-2"
			}
			if command.ThreadID != liveThreadID || command.TurnID != wantTurn {
				err := fmt.Errorf("control addressed %s/%s, want %s/%s", command.ThreadID, command.TurnID, liveThreadID, wantTurn)
				command.Reply <- err
				return runner.AgentControl{}, err
			}
			err := command.Check(ctx)
			command.Reply <- err
			if err != nil {
				return runner.AgentControl{}, err
			}
			b.mu.Lock()
			b.consumed = append(b.consumed, command.Kind)
			b.mu.Unlock()
			if command.Kind == kind {
				return command, nil
			}
		case <-ctx.Done():
			return runner.AgentControl{}, ctx.Err()
		case <-b.waitDone:
			return runner.AgentControl{}, fmt.Errorf("no %s control arrived before the test deadline", kind)
		}
	}
}

func (b *liveConversationBackend) recordedRequests() []runner.AgentTurnRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentTurnRequest(nil), b.requests...)
}

func (b *liveConversationBackend) consumedKinds() []runner.AgentControlKind {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentControlKind(nil), b.consumed...)
}

type conversationArchiveFixture struct {
	mu      sync.Mutex
	content map[string][]byte
}

func (f *conversationArchiveFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (f *conversationArchiveFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.content[ref.Key]
	if !ok {
		return nil, errors.New("archive content missing")
	}
	return bytes.Clone(content), nil
}
