package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestCoordinatorItemStore covers the coordinator item rows and the
// conversation column that records where a provider thread was produced.
func TestCoordinatorItemStore(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	record := f.record(f.owner, "coordinator", now)
	record.ProviderThreadID = "thread-1"
	record.ProviderThreadRunnerID = "rnr_one"
	tx := f.tx(t)
	if err := f.store.createConversation(t.Context(), tx, &record); err != nil {
		t.Fatalf("createConversation() error = %v", err)
	}
	f.commit(t, tx)

	stored, err := f.store.readConversation(t.Context(), f.service.database.db, f.organization, f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if stored.ProviderThreadRunnerID != "rnr_one" {
		t.Fatalf("provider thread runner = %q, want rnr_one", stored.ProviderThreadRunnerID)
	}

	if _, open, err := f.store.openCoordinatorItem(t.Context(), f.service.database.db, record.ID); err != nil || open {
		t.Fatalf("openCoordinatorItem() = %t, %v, want no open item", open, err)
	}

	tx = f.tx(t)
	if err := f.store.createCoordinatorItem(t.Context(), tx, coordinatorItemRecord{
		WorkItemID: "wi_1", ConversationID: record.ID, OrganizationID: f.organization, ProjectID: f.project.ID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("createCoordinatorItem() error = %v", err)
	}
	f.commit(t, tx)

	item, open, err := f.store.openCoordinatorItem(t.Context(), f.service.database.db, record.ID)
	if err != nil || !open || item.WorkItemID != "wi_1" {
		t.Fatalf("openCoordinatorItem() = %#v, %t, %v", item, open, err)
	}
	found, err := f.store.readConversationByCoordinatorItem(t.Context(), f.service.database.db, f.organization, f.project.ID, "wi_1")
	if err != nil || found.ID != record.ID {
		t.Fatalf("readConversationByCoordinatorItem() = %#v, %v", found, err)
	}

	tx = f.tx(t)
	if err := f.store.closeCoordinatorItem(t.Context(), tx, "wi_1", now.Add(time.Minute)); err != nil {
		t.Fatalf("closeCoordinatorItem() error = %v", err)
	}
	f.commit(t, tx)

	if _, open, err := f.store.openCoordinatorItem(t.Context(), f.service.database.db, record.ID); err != nil || open {
		t.Fatalf("openCoordinatorItem() after close = %t, %v", open, err)
	}
	if _, err := f.store.readConversationByCoordinatorItem(t.Context(), f.service.database.db, f.organization, f.project.ID, "wi_1"); err == nil {
		t.Fatal("readConversationByCoordinatorItem() resolved a closed item")
	}
}

// coordinatorIssues lists the coordinator items recorded for a project with
// the issue each one dispatches.
type coordinatorIssueRow struct {
	workItemID string
	title      string
	body       string
	state      string
	labels     string
	closed     bool
}

func coordinatorIssues(t *testing.T, service *Service, project string) []coordinatorIssueRow {
	t.Helper()
	rows, err := service.database.db.QueryContext(t.Context(), `SELECT ci.work_item_id, i.title, i.body, COALESCE(ws.detent_state, ''), i.labels_json, ci.closed_at IS NOT NULL
FROM coordinator_items ci JOIN issues i ON i.native_id = ci.work_item_id AND i.organization_id = ci.organization_id AND i.project_id = ci.project_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE ci.project_id = ? ORDER BY ci.created_at, ci.work_item_id`, project)
	if err != nil {
		t.Fatalf("query coordinator items: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var items []coordinatorIssueRow
	for rows.Next() {
		var item coordinatorIssueRow
		if err := rows.Scan(&item.workItemID, &item.title, &item.body, &item.state, &item.labels, &item.closed); err != nil {
			t.Fatalf("scan coordinator item: %v", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate coordinator items: %v", err)
	}
	return items
}

// TestCoordinatorItemMessageWithoutBackend proves an unlinked message on a
// hub without a coordinator backend opens exactly one coordinator item and
// queues the message for a runner instead of failing.
func TestCoordinatorItemMessageWithoutBackend(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Plan the release"})
	id := created.Conversation.ID

	response := f.command(t, f.token, id, conversation.Command{Key: "m-1", Kind: conversation.CommandMessage, Text: "What is blocked?"})
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryQueued || receipt.Error != nil {
		t.Fatalf("receipt = %#v, want queued without an error", receipt)
	}

	snapshot := f.snapshot(t, f.token, id)
	if snapshot.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
		t.Fatalf("execution = %#v, want waiting_for_runner", snapshot.Conversation.Execution)
	}
	if snapshot.Conversation.Execution.Capabilities.Steer || snapshot.Conversation.Execution.AttemptID != nil {
		t.Fatalf("execution owner/capabilities = %#v, want cleared", snapshot.Conversation.Execution)
	}
	if snapshot.Conversation.WorkItemID != nil {
		t.Fatalf("work item = %v, want the conversation to stay unlinked", snapshot.Conversation.WorkItemID)
	}
	if snapshot.Conversation.Visibility != conversation.VisibilityPrivate {
		t.Fatalf("visibility = %q, want private", snapshot.Conversation.Visibility)
	}

	items := coordinatorIssues(t, f.service, string(f.project.ID))
	if len(items) != 1 {
		t.Fatalf("coordinator items = %#v, want exactly one", items)
	}
	item := items[0]
	// The title and body carry no user text: the issue is readable by every
	// project reader while the chat stays private (decisions section 10.1).
	if item.title != "Coordinator turn for conversation "+id[len(id)-8:] {
		t.Errorf("title = %q", item.title)
	}
	if strings.Contains(item.title, "Plan the release") || strings.Contains(item.body, "Plan the release") || strings.Contains(item.body, "What is blocked?") {
		t.Errorf("title %q or body %q leaked chat content", item.title, item.body)
	}
	if !strings.Contains(item.body, id) || !strings.Contains(item.body, "No implementation") {
		t.Errorf("body = %q, want the conversation id and the no-implementation statement", item.body)
	}
	if item.state != "Todo" {
		t.Errorf("state = %q, want the first dispatchable state", item.state)
	}
	if !strings.Contains(item.labels, coordinatorItemLabel) {
		t.Errorf("labels = %q, want %q", item.labels, coordinatorItemLabel)
	}
	if item.closed {
		t.Error("coordinator item is closed right after creation")
	}

	// A second message reuses the open item.
	second := f.command(t, f.token, id, conversation.Command{Key: "m-2", Kind: conversation.CommandMessage, Text: "And after that?"})
	requireNativeStatus(t, second, http.StatusOK)
	decodeHubResponse(t, second, &receipt)
	if receipt.Status != conversation.DeliveryQueued {
		t.Fatalf("second receipt = %#v, want queued", receipt)
	}
	if again := coordinatorIssues(t, f.service, string(f.project.ID)); len(again) != 1 || again[0].workItemID != item.workItemID {
		t.Fatalf("coordinator items after a second message = %#v, want the same single item", again)
	}
}

// coordinatorItemFixture is a hub without a coordinator backend, one chat
// whose message opened a coordinator item, and enrolled runners that may or
// may not report a live-control backend.
type coordinatorItemFixture struct {
	conversationAPIFixture
	mu             sync.Mutex
	clock          time.Time
	policy         string
	conversationID string
	item           string
	ordinary       tracker.NativeIssue
	runners        int
}

func newCoordinatorItemFixture(t *testing.T) *coordinatorItemFixture {
	t.Helper()
	fixture := &coordinatorItemFixture{clock: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour},
		now: func() time.Time {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.clock
		},
	})
	f := newNativeFixture(t, service, "", "chat")
	var ownerID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = ?", "operator-chat").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	fixture.conversationAPIFixture = conversationAPIFixture{nativeFixture: f, ownerID: ownerID}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	created := fixture.create(t, f.token, map[string]any{"title": "Plan the release"})
	fixture.conversationID = created.Conversation.ID
	requireNativeStatus(t, fixture.command(t, f.token, fixture.conversationID, conversation.Command{Key: "m-1", Kind: conversation.CommandMessage, Text: "What is blocked?"}), http.StatusOK)
	items := coordinatorIssues(t, service, string(f.project.ID))
	if len(items) != 1 {
		t.Fatalf("coordinator items = %#v, want one", items)
	}
	fixture.item = items[0].workItemID
	// The ordinary issue is created after the coordinator item, and the
	// coordinator item is ranked first, so a runner that skips it must still
	// reach the ordinary issue in the same claim.
	fixture.ordinary = f.create(t, "ordinary")
	if _, err := service.database.db.ExecContext(t.Context(), `UPDATE queue_entries SET rank = '000'
WHERE issue_id = (SELECT id FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?)`, f.project.OrganizationID, f.project.ID, fixture.item); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *coordinatorItemFixture) at() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// runner enrolls a runner and optionally publishes a fresh codex report.
// Each runner reports its own provider account so that concurrency is never
// shared between the fixture's runners.
func (f *coordinatorItemFixture) runner(t *testing.T, liveControl bool) runnerFixture {
	t.Helper()
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	if liveControl {
		publishCapacity(t, f.nativeFixture, r, f.report(t, r, "codex"))
	}
	return r
}

// report is the runner's own provider observation at the fixture's clock.
func (f *coordinatorItemFixture) report(t *testing.T, r runnerFixture, backend string) providercapacity.Report {
	t.Helper()
	f.mu.Lock()
	f.runners++
	alias := fmt.Sprintf("runner%d", f.runners)
	f.mu.Unlock()
	return providercapacity.Report{Provider: "openai", Backend: backend, AccountAlias: alias, Models: []string{"test-model"},
		MaxConcurrent: 4, Availability: "available", ObservedAt: f.at()}
}

// scope is an admin read scope for the fixture's project.
func (f *coordinatorItemFixture) scope() nativeScope {
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
}

func (f *coordinatorItemFixture) claim(t *testing.T, r runnerFixture, claim tracker.NativeClaim) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
}

// coordinatorTurnBinding is one runner generation on a work item.
type coordinatorTurnBinding struct {
	runner  runnerFixture
	lease   tracker.NativeLease
	attempt string
	run     string
	item    string
}

// start claims the item for the runner and reports a running attempt on it.
func (f *coordinatorItemFixture) start(t *testing.T, r runnerFixture, item string, session string) coordinatorTurnBinding {
	t.Helper()
	issue, _, err := readNativeIssue(t.Context(), f.service.database.db, f.scope(), item)
	if err != nil {
		t.Fatal(err)
	}
	response := f.claim(t, r, providerClaim(r, issue, session))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	bound := coordinatorTurnBinding{runner: r, lease: lease, attempt: newNativeID("attempt"), run: newNativeID("run"), item: item}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: bound.run, AttemptID: bound.attempt, PolicyID: f.policy}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+item+"/events", r.redemption.Credential, event), http.StatusOK)
	return bound
}

// TestCoordinatorItemClaimGating proves only an enrolled runner with a fresh
// live-control provider report may claim a coordinator item, and that a
// skipped coordinator item does not consume the claim.
func TestCoordinatorItemClaimGating(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)

	// A runner with no live-control report skips the coordinator item and
	// claims the ordinary issue instead.
	plain := f.runner(t, false)
	response := f.claim(t, plain, tracker.NativeClaim{PolicyID: f.policy, MachineID: plain.binding.MachineID, SessionID: "plain-session", TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if string(lease.WorkItemID) != string(f.ordinary.WorkItemID) {
		t.Fatalf("claimed work item = %q, want the ordinary issue %q", lease.WorkItemID, f.ordinary.WorkItemID)
	}

	// Pinning the coordinator item explicitly is refused as well.
	pinned := f.claim(t, plain, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(f.item), MachineID: plain.binding.MachineID, SessionID: "plain-pinned", TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	if pinned.Code == http.StatusOK {
		t.Fatalf("a runner without a live-control backend claimed the coordinator item: %s", pinned.Body.String())
	}

	// A runner with a fresh codex report claims it.
	live := f.runner(t, true)
	issue, _, err := readNativeIssue(t.Context(), f.service.database.db, f.scope(), f.item)
	if err != nil {
		t.Fatal(err)
	}
	response = f.claim(t, live, providerClaim(live, issue, "live-session"))
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &lease)
	if string(lease.WorkItemID) != f.item {
		t.Fatalf("claimed work item = %q, want the coordinator item %q", lease.WorkItemID, f.item)
	}
}

// TestCoordinatorItemVisibility proves coordinator items stay out of the
// issue lists and the attention scan unless they are asked for.
func TestCoordinatorItemVisibility(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)

	page := func(t *testing.T, query string) []tracker.NativeIssue {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items"+query, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var items tracker.Page[tracker.NativeIssue]
		decodeHubResponse(t, response, &items)
		return items.Items
	}
	contains := func(items []tracker.NativeIssue, id string) bool {
		for _, item := range items {
			if string(item.WorkItemID) == id {
				return true
			}
		}
		return false
	}

	listed := page(t, "")
	if contains(listed, f.item) {
		t.Errorf("work-items listed the coordinator item %s", f.item)
	}
	if !contains(listed, string(f.ordinary.WorkItemID)) {
		t.Errorf("work-items dropped the ordinary issue %s", f.ordinary.WorkItemID)
	}
	included := page(t, "?include=coordinator")
	if !contains(included, f.item) {
		t.Errorf("include=coordinator dropped the coordinator item %s", f.item)
	}
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=other", f.token, nil), http.StatusUnprocessableEntity, "invalid_request")

	// The hub coordinator's attention scan skips coordinator items too. Both
	// items are leased so an unfiltered scan would report both as running.
	f.start(t, f.runner(t, true), f.item, "attention-live")
	f.start(t, f.runner(t, true), string(f.ordinary.WorkItemID), "attention-ordinary")
	tools := newCoordinatorToolset(f.service.conversations.coordinator.(*conversationTurnCoordinator), nil)
	result, err := tools.listAttention(t.Context(), conversationRecord{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID}, "project", coordinatorAttentionMax)
	if err != nil {
		t.Fatalf("listAttention() error = %v", err)
	}
	attention, ok := result.(coordinatorAttention)
	if !ok {
		t.Fatalf("listAttention() = %T, want coordinatorAttention", result)
	}
	groups := [][]coordinatorAttentionItem{attention.Blocked, attention.WaitingForInput, attention.Running, attention.Review, attention.Unavailable}
	found := false
	for _, group := range groups {
		for _, item := range group {
			if item.WorkItemID == f.item {
				t.Errorf("attention scan reported the coordinator item %s", f.item)
			}
			if item.WorkItemID == string(f.ordinary.WorkItemID) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("attention scan dropped the ordinary leased issue %s", f.ordinary.WorkItemID)
	}

	// A closed item stays hidden.
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if err := f.service.conversations.store.closeCoordinatorItem(t.Context(), tx, f.item, f.at()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if closed := page(t, ""); contains(closed, f.item) {
		t.Errorf("work-items listed the closed coordinator item %s", f.item)
	}
	if included := page(t, "?include=coordinator"); !contains(included, f.item) {
		t.Errorf("include=coordinator dropped the closed coordinator item %s", f.item)
	}
}

// coordinatorBindResponse is the worker bind payload with the coordinator
// fields the runner-dispatched path adds.
type coordinatorBindResponse struct {
	ConversationID string `json:"conversation_id"`
	Coordinator    bool   `json:"coordinator"`
	Resume         struct {
		ThreadID string `json:"thread_id"`
		// ThreadOrigin is the kind of turn the thread belongs to, so a runner
		// can see which one it was handed (decisions section 9.3).
		ThreadOrigin string `json:"thread_origin"`
		Transcript   []struct {
			Role string `json:"role"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"transcript"`
	} `json:"resume"`
	Pending []workerControlEnvelope `json:"pending"`
	Cursor  int64                   `json:"cursor"`
}

func (b coordinatorTurnBinding) identity() map[string]any {
	return map[string]any{"lease_id": b.lease.ID, "fencing_token": b.lease.FencingToken, "attempt_id": b.attempt}
}

// seedAssistant records an earlier assistant reply so the conversation has
// history that is not also a pending control.
func (f *coordinatorItemFixture) seedAssistant(t *testing.T, text string) {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	message := conversationMessageRecord{
		ConversationID: f.conversationID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: text,
		Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.at(),
	}
	if err := f.service.conversations.store.appendMessage(t.Context(), tx, &message); err != nil {
		t.Fatalf("appendMessage() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *coordinatorItemFixture) bind(t *testing.T, b coordinatorTurnBinding) coordinatorBindResponse {
	t.Helper()
	body := b.identity()
	body["run_id"] = b.run
	body["capabilities"] = map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+b.item+"/conversation/bind", b.runner.redemption.Credential, body)
	requireNativeStatus(t, response, http.StatusOK)
	var bound coordinatorBindResponse
	decodeHubResponse(t, response, &bound)
	return bound
}

func (f *coordinatorItemFixture) turnEvents(t *testing.T, b coordinatorTurnBinding, events ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := b.identity()
	body["events"] = events
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.conversationID+"/turn-events", b.runner.redemption.Credential, body)
}

func (f *coordinatorItemFixture) unbind(t *testing.T, b coordinatorTurnBinding, outcome string) *httptest.ResponseRecorder {
	t.Helper()
	body := b.identity()
	body["outcome"] = outcome
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+b.item+"/conversation/unbind", b.runner.redemption.Credential, body)
}

// TestCoordinatorItemBindAndUnbind walks the runner side of one coordinator
// turn: bind through the coordinator item, the thread origin rule, the turn
// that records the thread, and the unbind that closes the item.
func TestCoordinatorItemBindAndUnbind(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)
	// The conversation already carries a provider thread produced elsewhere.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET provider_thread_id = 'hub-thread', provider_thread_runner_id = '' WHERE id = ?", f.conversationID); err != nil {
		t.Fatal(err)
	}
	f.seedAssistant(t, "An earlier reply.")
	requireNativeStatus(t, f.command(t, f.token, f.conversationID, conversation.Command{Key: "m-follow", Kind: conversation.CommandMessage, Text: "What is blocked?"}), http.StatusOK)
	live := f.runner(t, true)
	first := f.start(t, live, f.item, "coordinator-1")

	bound := f.bind(t, first)
	if !bound.Coordinator {
		t.Error("bind response coordinator = false, want true for a coordinator item")
	}
	if bound.ConversationID != f.conversationID {
		t.Fatalf("conversation = %q, want %q", bound.ConversationID, f.conversationID)
	}
	if len(bound.Pending) != 2 || bound.Pending[0].Kind != "message" || bound.Pending[0].Text != "What is blocked?" {
		t.Fatalf("pending = %#v, want both queued messages", bound.Pending)
	}
	if bound.Resume.ThreadID != "" {
		t.Errorf("resume thread = %q, want none: the thread came from elsewhere", bound.Resume.ThreadID)
	}
	if len(bound.Resume.Transcript) != 1 || bound.Resume.Transcript[0].Text != "An earlier reply." || bound.Resume.Transcript[0].Role != string(conversation.RoleAssistant) {
		t.Fatalf("resume transcript = %#v, want the history without the pending controls", bound.Resume.Transcript)
	}

	// turn_started records the thread together with the reporting runner.
	requireNativeStatus(t, f.turnEvents(t, first, map[string]any{"type": "turn_started", "turn_id": "turn-1", "thread_id": "runner-thread"}), http.StatusAccepted)
	var threadID, threadRunner string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT provider_thread_id, provider_thread_runner_id FROM conversations WHERE id = ?", f.conversationID).Scan(&threadID, &threadRunner); err != nil {
		t.Fatal(err)
	}
	if threadID != "runner-thread" || threadRunner != live.binding.RunnerID {
		t.Fatalf("thread = %q from runner %q, want runner-thread from %q", threadID, threadRunner, live.binding.RunnerID)
	}

	// The unbind closes the item and moves the issue to a terminal state.
	requireNativeStatus(t, f.unbind(t, first, "succeeded"), http.StatusOK)
	items := coordinatorIssues(t, f.service, string(f.project.ID))
	if len(items) != 1 || !items[0].closed || items[0].state != "Done" {
		t.Fatalf("coordinator items after unbind = %#v, want one closed item in the terminal state", items)
	}

	// A follow-up message opens a new item, and the same runner rebinding
	// resumes the thread it produced.
	requireNativeStatus(t, f.command(t, f.token, f.conversationID, conversation.Command{Key: "m-2", Kind: conversation.CommandMessage, Text: "And after that?"}), http.StatusOK)
	items = coordinatorIssues(t, f.service, string(f.project.ID))
	if len(items) != 2 {
		t.Fatalf("coordinator items after a follow-up = %#v, want a second item", items)
	}
	next := ""
	for _, candidate := range items {
		if !candidate.closed {
			next = candidate.workItemID
		}
	}
	if next == "" || next == f.item {
		t.Fatalf("the follow-up did not open a new coordinator item: %#v", items)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(first.lease.ID)+"/release", live.redemption.Credential,
		tracker.NativeLeaseMutation{FencingToken: first.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	second := f.start(t, live, next, "coordinator-2")
	rebound := f.bind(t, second)
	if !rebound.Coordinator || rebound.Resume.ThreadID != "runner-thread" {
		t.Fatalf("rebind resume = %#v, want the runner's own thread", rebound.Resume)
	}
	if len(rebound.Resume.Transcript) != 0 {
		t.Errorf("rebind transcript = %#v, want none when the thread resumes", rebound.Resume.Transcript)
	}
}

// TestCoordinatorItemTurnEventData covers the item event's structured data
// and the cancel that reaches a bound coordinator attempt as an interrupt.
func TestCoordinatorItemTurnEventData(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)
	bound := f.start(t, f.runner(t, true), f.item, "coordinator-1")
	f.bind(t, bound)
	requireNativeStatus(t, f.turnEvents(t, bound, map[string]any{"type": "turn_started", "turn_id": "turn-1", "thread_id": "runner-thread"}), http.StatusAccepted)

	proposal := map[string]any{"proposal": map[string]any{"project_id": string(f.project.ID), "title": "Split the parser", "objective": "Make it testable"}}
	requireNativeStatus(t, f.turnEvents(t, bound, map[string]any{
		"type": "item", "kind": "status", "summary": "Proposed an issue", "provider_item_id": "item-1", "data": proposal,
	}), http.StatusAccepted)

	var stored string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT data_json FROM conversation_messages WHERE conversation_id = ? AND provider_item_id = ?", f.conversationID, "item-1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(stored), &round); err != nil {
		t.Fatalf("stored data is not JSON: %v", err)
	}
	body, ok := round["proposal"].(map[string]any)
	if !ok || body["title"] != "Split the parser" || body["project_id"] != string(f.project.ID) {
		t.Fatalf("item data = %#v, want the proposal round trip", round)
	}

	// An oversized data object is refused.
	requireNativeError(t, f.turnEvents(t, bound, map[string]any{
		"type": "item", "kind": "status", "summary": "too big", "data": map[string]any{"blob": strings.Repeat("x", 17<<10)},
	}), http.StatusUnprocessableEntity, "invalid_request")

	// A cancel on the bound coordinator turn is delivered as an interrupt.
	response := f.command(t, f.token, f.conversationID, conversation.Command{Key: "cancel-1", Kind: conversation.CommandCancel})
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryQueued || receipt.Kind != conversation.CommandCancel || receipt.MessageID == "" {
		t.Fatalf("cancel receipt = %#v, want a queued cancel with a message", receipt)
	}
	var kind, delivery string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT kind, delivery FROM conversation_messages WHERE id = ?", receipt.MessageID).Scan(&kind, &delivery); err != nil {
		t.Fatal(err)
	}
	if kind != string(conversation.MessageInterrupt) || delivery != string(conversation.DeliveryQueued) {
		t.Fatalf("cancel message = %q/%q, want a queued interrupt", kind, delivery)
	}
	snapshot := f.snapshot(t, f.token, f.conversationID)
	if snapshot.Conversation.Execution.Status != conversation.ExecutionInterrupting {
		t.Fatalf("execution after cancel = %#v, want interrupting", snapshot.Conversation.Execution)
	}
}

// TestCoordinatorItemCancelWithoutAttempt keeps the rejection when no runner
// holds the turn.
func TestCoordinatorItemCancelWithoutAttempt(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)
	response := f.command(t, f.token, f.conversationID, conversation.Command{Key: "cancel-1", Kind: conversation.CommandCancel})
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryRejected || receipt.Error == nil || receipt.Error.Code != "no_active_turn" {
		t.Fatalf("cancel receipt = %#v, want rejected with no_active_turn", receipt)
	}
}

// TestCoordinatorItemHostedVisibilityAndCapability proves the hosted project
// page hides coordinator items and the bootstrap capability follows an
// enrolled live-control runner.
func TestCoordinatorItemHostedVisibilityAndCapability(t *testing.T) {
	f := newConversationHostedFixture(t)
	bootstrap := func(t *testing.T) conversationBootstrapResponse {
		t.Helper()
		response := f.request(t, "owner", http.MethodGet, "/chat/bootstrap", nil)
		browserHostedStatus(t, response, http.StatusOK)
		var payload conversationBootstrapResponse
		decodeHubResponse(t, response, &payload)
		return payload
	}
	if payload := bootstrap(t); payload.Capabilities.Coordinator {
		t.Fatal("capabilities.coordinator = true without a backend or a live-control runner")
	}

	// A chat message opens a coordinator item that the project page hides.
	response := f.request(t, "owner", http.MethodPost, f.base+"/conversations", map[string]any{"key": "hosted-create", "title": "Hosted chat"})
	browserHostedStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	response = f.request(t, "owner", http.MethodPost, f.base+"/conversations/"+created.Conversation.ID+"/commands",
		conversation.Command{Key: "hosted-1", Kind: conversation.CommandMessage, Text: "What is blocked?"})
	browserHostedStatus(t, response, http.StatusOK)
	items := coordinatorIssues(t, f.service, f.project)
	if len(items) != 1 {
		t.Fatalf("coordinator items = %#v, want one", items)
	}
	list := f.request(t, "owner", http.MethodGet, f.base+"/work-items", nil)
	browserHostedStatus(t, list, http.StatusOK)
	if strings.Contains(list.Body.String(), items[0].workItemID) || strings.Contains(list.Body.String(), coordinatorItemTitlePrefix) {
		t.Error("the hosted work list returned the coordinator item")
	}
	// The conversation product is mounted, so the client shows its screens
	// (decisions section 10.14).
	if !bootstrap(t).Feature.Conversation {
		t.Error("the bootstrap did not report the conversation feature")
	}

	// Enrolling a runner that reports a fresh codex backend flips the
	// capability; a runner without one does not.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner = 1 WHERE user_id = ?", "user_chat_owner"); err != nil {
		t.Fatal(err)
	}
	hostedRunner(t, f, nil)
	if payload := bootstrap(t); payload.Capabilities.Coordinator {
		t.Fatal("capabilities.coordinator = true for a runner without a live-control backend")
	}
	hostedRunner(t, f, []providercapacity.Report{capacityReport(f.service.config.now())})
	if payload := bootstrap(t); !payload.Capabilities.Coordinator {
		t.Fatal("capabilities.coordinator = false with an enrolled live-control runner")
	}
}

// hostedRunner enrolls one runner in the hosted organization and publishes
// its provider observations through the ordinary heartbeat.
func hostedRunner(t *testing.T, f *conversationHostedFixture, reports []providercapacity.Report) {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	enrollments := "/api/v2/organizations/" + conversationHostedOrganization + "/runner-enrollments"
	response := f.request(t, "owner", http.MethodPost, enrollments, runnerauth.EnrollmentRequest{Binding: binding,
		ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)},
		Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	browserHostedStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "chat-host", DisplayName: "Runner", Capacity: 2, Version: "test"}
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, enrollments+"/redeem", enrollment.Token, redemption), http.StatusCreated)
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential,
		map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "provider_reports": reports}), http.StatusNoContent)
}

// TestRunnerSupportsLiveControl covers the freshness and backend rules the
// claim gate and the bootstrap capability share.
func TestRunnerSupportsLiveControl(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)
	codex := f.runner(t, true)
	other := f.runner(t, false)
	publishCapacity(t, f.nativeFixture, other, f.report(t, other, "claude"))
	tests := []struct {
		name   string
		runner string
		at     time.Time
		want   bool
	}{
		{name: "fresh codex report", runner: codex.binding.RunnerID, at: f.at(), want: true},
		{name: "report at the edge of the window", runner: codex.binding.RunnerID, at: f.at().Add(providercapacity.MaxAge - time.Second), want: true},
		{name: "stale report", runner: codex.binding.RunnerID, at: f.at().Add(providercapacity.MaxAge), want: false},
		{name: "observation in the future", runner: codex.binding.RunnerID, at: f.at().Add(-time.Second), want: false},
		{name: "backend without live control", runner: other.binding.RunnerID, at: f.at(), want: false},
		{name: "legacy machine registration", runner: "", at: f.at(), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := runnerSupportsLiveControl(t.Context(), f.service.database.db, test.runner, test.at)
			if err != nil {
				t.Fatalf("runnerSupportsLiveControl() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("runnerSupportsLiveControl() = %t, want %t", got, test.want)
			}
		})
	}
}

// TestCoordinatorItemHostedPlanPolicy proves a coordinator item is native
// issue creation and runs the same hosted plan policy: when the plan no
// longer carries the collaboration feature, opening a chat turn is refused
// with the code POST /work-items reports (decisions section 10.10).
func TestCoordinatorItemHostedPlanPolicy(t *testing.T) {
	f := newConversationHostedFixture(t)
	created := f.request(t, "owner", http.MethodPost, f.base+"/conversations", map[string]any{"key": "plan-create", "title": "Hosted chat"})
	browserHostedStatus(t, created, http.StatusCreated)
	var conversationCreated conversationCreateResponse
	decodeHubResponse(t, created, &conversationCreated)

	revokeHostedCollaboration(t, f.service)

	// POST /work-items is the reference: the coordinator item must be
	// refused with the same status and code.
	issue := f.request(t, "owner", http.MethodPost, f.base+"/work-items", map[string]any{"idempotency_key": "plan-issue", "title": "Ordinary issue", "body": "b"})
	browserHostedStatus(t, issue, http.StatusTooManyRequests)
	var reference apiErrorResponse
	decodeHubResponse(t, issue, &reference)

	turn := f.request(t, "owner", http.MethodPost, f.base+"/conversations/"+conversationCreated.Conversation.ID+"/commands",
		conversation.Command{Key: "plan-message", Kind: conversation.CommandMessage, Text: "What is blocked?"})
	browserHostedStatus(t, turn, http.StatusTooManyRequests)
	var refused apiErrorResponse
	decodeHubResponse(t, turn, &refused)
	if refused.Code != reference.Code || refused.Code != "allowance_exhausted" {
		t.Fatalf("coordinator turn refusal = %#v, want the same code as %#v", refused, reference)
	}
	if items := coordinatorIssues(t, f.service, f.project); len(items) != 0 {
		t.Fatalf("coordinator items = %#v, want none: a refused turn must persist nothing", items)
	}
	var messages int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id = ?", conversationCreated.Conversation.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("messages = %d, want none", messages)
	}

	// Creating a conversation whose first message opens the turn is refused
	// the same way, and leaves no conversation behind.
	withMessage := f.request(t, "owner", http.MethodPost, f.base+"/conversations", map[string]any{
		"key": "plan-create-2", "title": "Blocked", "first_message": map[string]any{"key": "plan-first", "text": "What is blocked?"},
	})
	browserHostedStatus(t, withMessage, http.StatusTooManyRequests)
	var conversations int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if conversations != 1 {
		t.Fatalf("conversations = %d, want only the one created before the plan changed", conversations)
	}
}

// revokeHostedCollaboration rewrites the organization's plan so that it no
// longer carries the collaboration feature, the way an expired plan looks.
func revokeHostedCollaboration(t *testing.T, service *Service) {
	t.Helper()
	var id string
	var version int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT base_id, base_version FROM hosted_plan_assignments").Scan(&id, &version); err != nil {
		t.Fatal(err)
	}
	if _, err := service.database.db.ExecContext(t.Context(),
		`UPDATE hosted_plans SET record_json = json_set(record_json, '$.features', json_array('native_execution')) WHERE id = ? AND version = ?`, id, version); err != nil {
		t.Fatal(err)
	}
}

// TestCoordinatorLabelIsReserved proves the label that selects the runner's
// read-only coordinator run mode cannot be set through the tracker: a
// triager who could add it to any issue would turn that issue into a run
// that produces no deliverable. Only the hub's own coordinator-item path
// may use it (decisions section 9.1).
func TestCoordinatorLabelIsReserved(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	worker := f.worker(t, "reserved-label-worker")
	issue := f.nativeFixture.create(t, "ordinary")
	labels := []string{coordinatorItemLabel}

	tests := []struct {
		name   string
		token  string
		method string
		path   string
		body   any
	}{
		{
			name: "operator create", token: f.token, method: http.MethodPost, path: f.base + "/work-items",
			body: tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-1"}, Title: "Sneaky", Body: "b", State: "Todo", Labels: labels},
		},
		{
			name: "operator create with different case", token: f.token, method: http.MethodPost, path: f.base + "/work-items",
			body: tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-2"}, Title: "Sneaky", Body: "b", State: "Todo", Labels: []string{"Detent:Coordinator"}},
		},
		{
			name: "operator update", token: f.token, method: http.MethodPatch, path: f.base + "/work-items/" + string(issue.WorkItemID),
			body: tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-3"}, ExpectedRevision: issue.Revision, Labels: &labels},
		},
		{
			name: "worker create", token: worker, method: http.MethodPost, path: f.base + "/work-items",
			body: tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-4"}, Title: "Sneaky", Body: "b", State: "Todo", Labels: labels},
		},
		{
			name: "worker update", token: worker, method: http.MethodPatch, path: f.base + "/work-items/" + string(issue.WorkItemID),
			body: tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-5"}, ExpectedRevision: issue.Revision, Labels: &labels},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, test.method, test.path, test.token, test.body)
			failure := requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
			if !strings.Contains(failure.Message, coordinatorItemLabel) || !strings.Contains(failure.Message, "reserved") {
				t.Fatalf("message = %q, want it to name the reserved label", failure.Message)
			}
		})
	}

	t.Run("the hub still opens its own coordinator item", func(t *testing.T) {
		record := f.create(t, f.token, map[string]any{"title": "Reserved"}).Conversation
		requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "reserved-message", Kind: conversation.CommandMessage, Text: "What is blocked?"}), http.StatusOK)
		items := coordinatorIssues(t, f.service, string(f.project.ID))
		if len(items) != 1 || !strings.Contains(items[0].labels, coordinatorItemLabel) {
			t.Fatalf("coordinator items = %#v, want one carrying the reserved label", items)
		}
	})
}

// TestCoordinatorItemSurvivesSettling proves a settle does not close the
// conversation's open coordinator item. Settled replaces Archive and carries
// none of its read-only meaning, so the turn a runner already owes the
// conversation still has an item to bind to (decisions section 14).
func TestCoordinatorItemSurvivesSettling(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Settled"}).Conversation
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "settle-message", Kind: conversation.CommandMessage, Text: "What is blocked?"}), http.StatusOK)
	items := coordinatorIssues(t, f.service, string(f.project.ID))
	if len(items) != 1 || items[0].closed {
		t.Fatalf("coordinator items = %#v, want one open item", items)
	}

	f.settle(t, record.ID)

	open := coordinatorIssues(t, f.service, string(f.project.ID))
	if len(open) != 1 || open[0].closed {
		t.Fatalf("coordinator items after settling = %#v, want the item still open", open)
	}

	// The next message unsettles the conversation and reuses the same item.
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "settle-message-2", Kind: conversation.CommandMessage, Text: "And now?"}), http.StatusOK)
	reused := coordinatorIssues(t, f.service, string(f.project.ID))
	if len(reused) != 1 || reused[0].closed {
		t.Fatalf("coordinator items = %#v, want the open item reused", reused)
	}
}
