package hubserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationPreferencesModel is the model the fixture's runner reports, so
// a preference naming it is accepted and one naming anything else is not.
const conversationPreferencesModel = "gpt-6-astra"

// reportConversationModels enrolls a runner on the fixture's project and
// publishes a provider report naming models, which is where the bootstrap's
// model choices and the PATCH validation come from (decisions section 14).
func reportConversationModels(t *testing.T, f conversationAPIFixture, models ...string) {
	t.Helper()
	runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	publishCapacity(t, f.nativeFixture, runner, providercapacity.Report{
		Provider: "openai", Backend: "codex", AccountAlias: "primary", Models: models,
		MaxConcurrent: 2, Availability: "available", ObservedAt: now,
	})
}

func conversationPatch(t *testing.T, f conversationAPIFixture, token, id string, body map[string]any) conversationResource {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/conversations/"+id, token, body)
	requireNativeStatus(t, response, http.StatusOK)
	var updated conversationResource
	decodeHubResponse(t, response, &updated)
	return updated
}

// TestConversationPreferences covers the PATCH half of decisions section 14:
// a fresh conversation is all auto, each field can be set on its own, an
// unknown value is refused, and "auto" puts a field back.
func TestConversationPreferences(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	reportConversationModels(t, f, conversationPreferencesModel, "claude-opus-5")
	record := f.create(t, f.token, map[string]any{"title": "Preferences"}).Conversation
	if got := record.Preferences; got != conversation.DefaultPreferences() {
		t.Fatalf("new conversation preferences = %#v, want every field auto", got)
	}

	t.Run("partial updates keep the other fields", func(t *testing.T) {
		updated := conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"reasoning_effort": "high"}})
		want := conversation.Preferences{Model: conversation.PreferenceAuto, ReasoningEffort: conversation.EffortHigh, Access: conversation.PreferenceAuto}
		if updated.Preferences != want {
			t.Fatalf("preferences = %#v, want %#v", updated.Preferences, want)
		}
		updated = conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"model": conversationPreferencesModel, "access": conversation.AccessReadOnly}})
		want = conversation.Preferences{Model: conversationPreferencesModel, ReasoningEffort: conversation.EffortHigh, Access: conversation.AccessReadOnly}
		if updated.Preferences != want {
			t.Fatalf("preferences = %#v, want %#v", updated.Preferences, want)
		}
		if got := f.snapshot(t, f.token, record.ID).Conversation.Preferences; got != want {
			t.Fatalf("stored preferences = %#v, want %#v", got, want)
		}
	})

	t.Run("auto puts a field back", func(t *testing.T) {
		updated := conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"model": "auto", "reasoning_effort": "auto", "access": "auto"}})
		if updated.Preferences != conversation.DefaultPreferences() {
			t.Fatalf("preferences = %#v, want every field auto", updated.Preferences)
		}
	})

	t.Run("title and preferences travel together", func(t *testing.T) {
		updated := conversationPatch(t, f, f.token, record.ID, map[string]any{"title": "Renamed", "preferences": map[string]any{"reasoning_effort": "low"}})
		if updated.Title != "Renamed" || updated.Preferences.ReasoningEffort != conversation.EffortLow {
			t.Fatalf("updated = %#v", updated)
		}
	})

	for _, test := range []struct {
		name        string
		preferences map[string]any
	}{
		{name: "unknown model", preferences: map[string]any{"model": "gpt-4"}},
		{name: "unknown effort", preferences: map[string]any{"reasoning_effort": "max"}},
		{name: "unknown access", preferences: map[string]any{"access": "write"}},
	} {
		t.Run(test.name+" is refused", func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/conversations/"+record.ID, f.token, map[string]any{"preferences": test.preferences})
			requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
		})
	}
}

// TestConversationModelChoicesFallBackToTheDefault proves a hub whose runners
// have reported no catalog still offers the configured coordinator model
// beside "auto", and accepts a preference naming it (decisions section 14).
func TestConversationModelChoicesFallBackToTheDefault(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, Model: conversationPreferencesModel})
	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	models, err := f.service.conversationModelChoices(t.Context(), f.service.database.db, f.project.OrganizationID, []string{string(f.project.ID)}, now)
	if err != nil {
		t.Fatalf("conversationModelChoices() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != conversationPreferencesModel {
		t.Fatalf("models = %v, want only the configured default", models)
	}
	record := f.create(t, f.token, map[string]any{"title": "Default"}).Conversation
	updated := conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"model": conversationPreferencesModel}})
	if updated.Preferences.Model != conversationPreferencesModel {
		t.Fatalf("preferences = %#v", updated.Preferences)
	}
}

// TestConversationPreferencesReachTheIssue proves the handoff writes the
// conversation's explicit effort into the issue's detent-agent block, never
// its model,
// and that a later change rewrites that block exactly once (section 14).
func TestConversationPreferencesReachTheIssue(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	reportConversationModels(t, f, conversationPreferencesModel)
	record := f.create(t, f.token, map[string]any{"title": "Handoff"}).Conversation
	conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"model": conversationPreferencesModel, "reasoning_effort": "high"}})

	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, map[string]any{
		"key": "link-preferences", "share_history": true,
		"issue": map[string]any{"title": "Fix the renewal", "description": "From the chat"},
	})
	requireNativeStatus(t, response, http.StatusOK)
	var linked conversationLinkResult
	decodeHubResponse(t, response, &linked)

	issue := readConversationIssue(t, f, linked.Issue.ID)
	override, found, err := agentoverride.FromIssueBody(issue.Body)
	if err != nil || !found {
		t.Fatalf("FromIssueBody() = %#v found %t error %v", override, found, err)
	}
	if override.Model != "" || override.Effort != conversation.EffortHigh {
		t.Fatalf("override = %#v, want the effort and no block-level model", override)
	}
	if issue.Revision != 1 {
		t.Fatalf("revision = %d, want the block written with the first revision", issue.Revision)
	}

	// A change after the link rewrites the block, and only the block.
	conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"reasoning_effort": "low"}})
	changed := readConversationIssue(t, f, linked.Issue.ID)
	override, _, err = agentoverride.FromIssueBody(changed.Body)
	if err != nil {
		t.Fatalf("FromIssueBody() error = %v", err)
	}
	if override.Effort != conversation.EffortLow || override.Model != "" {
		t.Fatalf("override after the change = %#v", override)
	}
	if changed.Revision != issue.Revision+1 {
		t.Fatalf("revision = %d, want exactly one update", changed.Revision)
	}
	if !strings.Contains(changed.Body, "Conversation: "+record.ID) {
		t.Fatalf("body lost its conversation line: %q", changed.Body)
	}

	// Re-applying the same preferences writes nothing: the body already says
	// what it should.
	conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"reasoning_effort": "low"}})
	again := readConversationIssue(t, f, linked.Issue.ID)
	if again.Revision != changed.Revision {
		t.Fatalf("revision = %d, want the unchanged %d", again.Revision, changed.Revision)
	}

	// Returning every field to auto removes the block rather than leaving a
	// stale override behind.
	conversationPatch(t, f, f.token, record.ID, map[string]any{"preferences": map[string]any{"model": "auto", "reasoning_effort": "auto"}})
	cleared := readConversationIssue(t, f, linked.Issue.ID)
	if _, found, _ := agentoverride.FromIssueBody(cleared.Body); found {
		t.Fatalf("body still carries a detent-agent block: %q", cleared.Body)
	}
}

func readConversationIssue(t *testing.T, f conversationAPIFixture, id string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+id, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

// TestConversationPreferencesReachTheBind proves the runner is told what the
// conversation prefers when it binds, so it can apply the model, the effort
// and the access level to its turn (decisions section 14).
func TestConversationPreferencesReachTheBind(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	if err := f.chat.transact(t.Context(), func(tx *sql.Tx, now time.Time) error {
		record, err := f.chat.store.readConversationByID(t.Context(), tx, f.record.ID)
		if err != nil {
			return err
		}
		record.Preferences = conversation.Preferences{Model: conversationPreferencesModel, ReasoningEffort: conversation.EffortHigh, Access: conversation.AccessReadOnly}
		return f.chat.saveConversation(t.Context(), tx, &record, now)
	}); err != nil {
		t.Fatalf("store preferences: %v", err)
	}
	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound conversationBindResponse
	decodeHubResponse(t, response, &bound)
	want := conversation.Preferences{Model: conversationPreferencesModel, ReasoningEffort: conversation.EffortHigh, Access: conversation.AccessReadOnly}
	if bound.Preferences != want {
		t.Fatalf("bind preferences = %#v, want %#v", bound.Preferences, want)
	}
}

// TestConversationSettleSweep proves the sweep settles a conversation whose
// execution has ended and that has been quiet for the project's settle
// window, leaves a fresher or still-running one alone, and that a new
// message unsettles (decisions section 14).
func TestConversationSettleSweep(t *testing.T) {
	t.Parallel()
	window := time.Hour
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, SettleWindow: window})
	idle := f.create(t, f.token, map[string]any{"title": "Idle"}).Conversation
	fresh := f.create(t, f.token, map[string]any{"title": "Fresh"}).Conversation
	running := f.create(t, f.token, map[string]any{"title": "Running"}).Conversation
	setConversationExecution(t, f, running.ID, conversation.ExecutionRunning)
	backdateConversation(t, f, idle.ID, 2*window)
	backdateConversation(t, f, running.ID, 2*window)

	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	settled, err := f.service.conversations.settleIdleConversations(t.Context(), now)
	if err != nil {
		t.Fatalf("settleIdleConversations() error = %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want only the idle conversation", settled)
	}
	if got := f.snapshot(t, f.token, idle.ID).Conversation.Status; got != conversation.StatusSettled {
		t.Fatalf("idle status = %q, want settled", got)
	}
	for _, record := range []conversationResource{fresh, running} {
		if got := f.snapshot(t, f.token, record.ID).Conversation.Status; got != conversation.StatusActive {
			t.Fatalf("%s status = %q, want active", record.Title, got)
		}
	}

	// The settle is announced, so an open stream and every other tab sees it.
	if !conversationEmitted(t, f, idle.ID, conversation.EventConversationUpdated) {
		t.Fatal("settling emitted no conversation.updated event")
	}

	// A new message wakes it, and the sweep leaves it alone afterwards.
	requireNativeStatus(t, f.command(t, f.token, idle.ID, conversation.Command{Key: "wake", Kind: conversation.CommandMessage, Text: "Still here?"}), http.StatusOK)
	if got := f.snapshot(t, f.token, idle.ID).Conversation.Status; got != conversation.StatusActive {
		t.Fatalf("status after a message = %q, want active", got)
	}
	settled, err = f.service.conversations.settleIdleConversations(t.Context(), now)
	if err != nil {
		t.Fatalf("settleIdleConversations() error = %v", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d, want none after the wake", settled)
	}
}

// setConversationExecution forces an execution status without going through
// a runner bind, so a sweep test can stage a running conversation.
func setConversationExecution(t *testing.T, f conversationAPIFixture, id string, status conversation.ExecutionStatus) {
	t.Helper()
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE conversations SET execution_json = json_set(execution_json, '$.status', ?) WHERE id = ?", status, id); err != nil {
		t.Fatalf("set execution: %v", err)
	}
}

// backdateConversation moves a conversation's activity into the past so the
// settle window has already elapsed.
func backdateConversation(t *testing.T, f conversationAPIFixture, id string, age time.Duration) {
	t.Helper()
	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	stamp := conversationTime(now.Add(-age))
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE conversations SET updated_at = ?, last_message_at = NULL WHERE id = ?", stamp, id); err != nil {
		t.Fatalf("backdate conversation: %v", err)
	}
}

func conversationEmitted(t *testing.T, f conversationAPIFixture, id string, want conversation.EventType) bool {
	t.Helper()
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM conversation_events WHERE conversation_id = ? AND type = ?", id, want).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count > 0
}

// TestConversationDefaultSettleWindow proves the configuration default is the
// 24 hours decisions section 14 names.
func TestConversationDefaultSettleWindow(t *testing.T) {
	t.Parallel()
	if got := (ConversationConfig{Enabled: true}).normalized().SettleWindow; got != 24*time.Hour {
		t.Fatalf("SettleWindow = %s, want 24h", got)
	}
}

// TestConversationHandoffNextStep covers the handoff's next step: the default
// lane, an explicit state, a priority, and dispatch now or later.
func TestConversationHandoffNextStep(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	link := func(t *testing.T, title string, next map[string]any) conversationLinkResult {
		t.Helper()
		record := f.create(t, f.token, map[string]any{"title": title}).Conversation
		body := map[string]any{
			"key": "link-" + title, "share_history": true,
			"issue": map[string]any{"title": title, "description": "From the chat"},
		}
		if next != nil {
			body["next"] = next
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, body)
		requireNativeStatus(t, response, http.StatusOK)
		var result conversationLinkResult
		decodeHubResponse(t, response, &result)
		return result
	}
	for _, test := range []struct {
		name         string
		next         map[string]any
		wantState    string
		wantDispatch string
		wantPriority *int
	}{
		{name: "absent", wantState: "Todo", wantDispatch: conversationDispatchNow},
		{name: "dispatch-now", next: map[string]any{"dispatch": "now"}, wantState: "Todo", wantDispatch: conversationDispatchNow},
		// This project has no Backlog, so "later" falls back to the first
		// dispatchable state (decisions section 14).
		{name: "dispatch-later-without-backlog", next: map[string]any{"dispatch": "later"}, wantState: "Todo", wantDispatch: conversationDispatchLater},
		{name: "explicit-state", next: map[string]any{"state": "In Progress"}, wantState: "In Progress", wantDispatch: conversationDispatchNow},
		{name: "priority", next: map[string]any{"priority": 1}, wantState: "Todo", wantDispatch: conversationDispatchNow, wantPriority: conversationRank1()},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := link(t, test.name, test.next)
			if result.Next.State != test.wantState || result.Next.Dispatch != test.wantDispatch {
				t.Fatalf("next = %#v", result.Next)
			}
			if result.Issue.State != test.wantState {
				t.Fatalf("issue state = %q, want %q", result.Issue.State, test.wantState)
			}
			switch {
			case test.wantPriority == nil && result.Next.Priority != nil:
				t.Fatalf("priority = %d, want none", *result.Next.Priority)
			case test.wantPriority != nil && (result.Next.Priority == nil || *result.Next.Priority != *test.wantPriority):
				t.Fatalf("priority = %v, want %d", result.Next.Priority, *test.wantPriority)
			}
		})
	}
	t.Run("unknown state is refused", func(t *testing.T) {
		record := f.create(t, f.token, map[string]any{"title": "Bad state"}).Conversation
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, map[string]any{
			"key": "link-bad-state", "share_history": true,
			"issue": map[string]any{"title": "Bad state", "description": "From the chat"},
			"next":  map[string]any{"state": "Nowhere"},
		})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
	t.Run("unknown dispatch is refused", func(t *testing.T) {
		record := f.create(t, f.token, map[string]any{"title": "Bad dispatch"}).Conversation
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, map[string]any{
			"key": "link-bad-dispatch", "share_history": true,
			"issue": map[string]any{"title": "Bad dispatch", "description": "From the chat"},
			"next":  map[string]any{"dispatch": "eventually"},
		})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
}

func conversationRank1() *int {
	rank := 1
	return &rank
}

// TestConversationHandoffLaterUsesBacklog proves "later" parks the issue in a
// project's Backlog lane when it has one, however that lane is cased.
func TestConversationHandoffLaterUsesBacklog(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: t.TempDir() + "/hub.db", Conversation: &ConversationConfig{Enabled: true}})
	var organization tracker.OrganizationID
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	states := []tracker.NativeState{
		{Name: "backlog", Transitions: []string{"Todo"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"backlog", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", testHubAdminToken,
		map[string]any{"idempotency_key": "project-backlog", "name": "backlog-project", "states": states})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "operator-backlog", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken,
		map[string]any{"organization_id": organization, "project_id": project.ID}), http.StatusNoContent)
	base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID)

	response = performHubAPIRequest(t, service, http.MethodPost, base+"/conversations", token.Token, map[string]any{"key": "create-backlog", "title": "Later"})
	requireNativeStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)

	response = performHubAPIRequest(t, service, http.MethodPost, base+"/conversations/"+created.Conversation.ID+"/link", token.Token, map[string]any{
		"key": "link-later", "share_history": true,
		"issue": map[string]any{"title": "Later", "description": "From the chat"},
		"next":  map[string]any{"dispatch": "later"},
	})
	requireNativeStatus(t, response, http.StatusOK)
	var result conversationLinkResult
	decodeHubResponse(t, response, &result)
	if result.Next.State != "backlog" || result.Next.Dispatch != conversationDispatchLater {
		t.Fatalf("next = %#v, want the Backlog lane", result.Next)
	}
	if result.Issue.State != "backlog" {
		t.Fatalf("issue state = %q", result.Issue.State)
	}
}

// TestConversationMessageReferences covers extraction, resolution and the
// "referenced from" listing (decisions section 14).
func TestConversationMessageReferences(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	issue := f.nativeFixture.create(t, "Referenced issue")
	other := f.nativeFixture.create(t, "Second issue")
	target := f.create(t, f.token, map[string]any{"title": "Target chat"}).Conversation
	record := f.create(t, f.token, map[string]any{"title": "References"}).Conversation

	text := "See #" + strconv.Itoa(issue.Number) +
		" and " + f.project.Name + "#" + strconv.Itoa(other.Number) +
		" and " + target.ID + ", but not #9999 or conv_" + strings.Repeat("0", 32) + "."
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "refs", Kind: conversation.CommandMessage, Text: text}), http.StatusOK)

	messages := f.snapshot(t, f.token, record.ID).Messages
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	got := messages[0].References
	if len(got) != 3 {
		t.Fatalf("references = %#v, want the three resolvable targets", got)
	}
	want := []conversationReferenceResource{
		{Kind: conversation.ReferenceIssue, ID: string(issue.WorkItemID), Label: "#" + strconv.Itoa(issue.Number), URL: "/work/i/" + string(issue.WorkItemID)},
		{Kind: conversation.ReferenceIssue, ID: string(other.WorkItemID), Label: f.project.Name + "#" + strconv.Itoa(other.Number), URL: "/work/i/" + string(other.WorkItemID)},
		{Kind: conversation.ReferenceConversation, ID: target.ID, Label: target.ID, URL: "/chat/c/" + target.ID},
	}
	for index, reference := range want {
		if got[index] != reference {
			t.Fatalf("references[%d] = %#v, want %#v", index, got[index], reference)
		}
	}

	t.Run("the target lists what referenced it", func(t *testing.T) {
		page := listConversationReferences(t, f, f.token, string(issue.WorkItemID))
		if len(page.References) != 1 {
			t.Fatalf("references = %#v", page.References)
		}
		entry := page.References[0]
		if entry.MessageID != messages[0].ID || entry.ConversationID != record.ID || entry.CreatedAt.IsZero() {
			t.Fatalf("reference = %#v", entry)
		}
		if !strings.HasPrefix(entry.Excerpt, "See #") {
			t.Fatalf("excerpt = %q", entry.Excerpt)
		}
	})

	// The chat is private, so another reader of the same issue must not be
	// told what it said (decisions section 14).
	t.Run("a private chat is invisible to another reader", func(t *testing.T) {
		page := listConversationReferences(t, f, f.other, string(issue.WorkItemID))
		if len(page.References) != 0 {
			t.Fatalf("references = %#v, want none for a private chat", page.References)
		}
	})

	t.Run("a shared chat is visible to every issue reader", func(t *testing.T) {
		requireNativeStatus(t, f.link(t, f.token, record.ID, "share-refs", true, "Shared now"), http.StatusOK)
		page := listConversationReferences(t, f, f.other, string(issue.WorkItemID))
		if len(page.References) != 1 {
			t.Fatalf("references = %#v, want the shared chat", page.References)
		}
	})

	t.Run("an unknown issue is not found", func(t *testing.T) {
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/wi_missing/references", f.token, nil)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
	})
}

// TestConversationReferencesSurviveMessageUpdates proves a message.updated
// event keeps the message's references. The event replaces the client's copy,
// so a projection that dropped them would take the links off the transcript
// as soon as the delivery moved on (decisions section 14).
func TestConversationReferencesSurviveMessageUpdates(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	issue := f.nativeFixture.create(t, "Updated reference")
	record := f.create(t, f.token, map[string]any{"title": "Updates"}).Conversation
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{
		Key: "update-refs", Kind: conversation.CommandMessage, Text: "Look at #" + strconv.Itoa(issue.Number) + ".",
	}), http.StatusOK)
	messages := f.snapshot(t, f.token, record.ID).Messages
	stored := messages[0]
	if len(stored.References) != 1 {
		t.Fatalf("references = %#v", stored.References)
	}

	// Move the message on without changing its text, the way the coordinator
	// and the worker do, and read the event the update emitted.
	if err := f.service.conversations.transact(t.Context(), func(tx *sql.Tx, now time.Time) error {
		pending, err := f.service.conversations.queryMessages(t.Context(), tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND delivery = 'saved' ORDER BY seq", record.ID)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			t.Fatalf("pending = %#v", pending)
		}
		pending[0].Delivery = conversation.DeliverySending
		return f.service.conversations.updateMessage(t.Context(), tx, pending[0], now)
	}); err != nil {
		t.Fatalf("update message: %v", err)
	}

	events, err := f.service.conversations.store.listEvents(t.Context(), f.service.database.db, record.ID, 0, 100)
	if err != nil {
		t.Fatalf("listEvents() error = %v", err)
	}
	updated := false
	for _, event := range events {
		if event.Type != conversation.EventMessageUpdated {
			continue
		}
		var body conversationMessageResource
		if err := json.Unmarshal(event.Data, &body); err != nil {
			t.Fatalf("decode message.updated: %v", err)
		}
		updated = true
		if len(body.References) != 1 || body.References[0].ID != stored.References[0].ID {
			t.Fatalf("message.updated references = %#v, want %#v", body.References, stored.References)
		}
	}
	if !updated {
		t.Fatal("no message.updated event was emitted")
	}
}

func listConversationReferences(t *testing.T, f conversationAPIFixture, token, item string) conversationWorkItemReferencePage {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+item+"/references", token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page conversationWorkItemReferencePage
	decodeHubResponse(t, response, &page)
	return page
}

// TestConversationReferencesAreBounded proves one message can never write
// more than conversation.MaxReferences rows, however many issues it names.
func TestConversationReferencesAreBounded(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	var text strings.Builder
	text.WriteString("Everything: ")
	for index := range conversation.MaxReferences + 5 {
		issue := f.nativeFixture.create(t, "Issue "+strconv.Itoa(index))
		text.WriteString(" #" + strconv.Itoa(issue.Number))
	}
	record := f.create(t, f.token, map[string]any{"title": "Many"}).Conversation
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "many", Kind: conversation.CommandMessage, Text: text.String()}), http.StatusOK)
	messages := f.snapshot(t, f.token, record.ID).Messages
	if got := len(messages[0].References); got != conversation.MaxReferences {
		t.Fatalf("references = %d, want %d", got, conversation.MaxReferences)
	}
}

// TestConversationReferenceExcerptIsBounded proves the listing never
// republishes more than 200 runes of a message.
func TestConversationReferenceExcerptIsBounded(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	issue := f.nativeFixture.create(t, "Long reference")
	record := f.create(t, f.token, map[string]any{"title": "Long"}).Conversation
	text := "#" + strconv.Itoa(issue.Number) + " " + strings.Repeat("é", 500)
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "long", Kind: conversation.CommandMessage, Text: text}), http.StatusOK)
	page := listConversationReferences(t, f, f.token, string(issue.WorkItemID))
	if len(page.References) != 1 {
		t.Fatalf("references = %#v", page.References)
	}
	if got := len([]rune(page.References[0].Excerpt)); got != conversationReferenceExcerptRunes {
		t.Fatalf("excerpt runes = %d, want %d", got, conversationReferenceExcerptRunes)
	}
}
