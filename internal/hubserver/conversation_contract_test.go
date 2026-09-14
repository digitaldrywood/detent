package hubserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The shared contract fixtures live with the client, which validates every
// payload against them. They are the cross-language truth for
// docs/conversation/decisions.md section 5: this test decodes each fixture
// into the Go type that carries it and re-encodes the value the real
// projection functions produce, so a field the hub adds, drops or renames
// fails here before it reaches the client.
const conversationFixtureDirectory = "../../web/conversation/src/contracts/fixtures"

// Timestamps reused across the fixture values. Only the shape is compared,
// but realistic values keep the failures readable.
var (
	conversationFixtureCreatedAt = time.Date(2026, 9, 9, 9, 38, 0, 0, time.UTC)
	conversationFixtureLinkedAt  = time.Date(2026, 9, 9, 9, 41, 12, 0, time.UTC)
	conversationFixtureUpdatedAt = time.Date(2026, 9, 9, 10, 2, 44, 0, time.UTC)
	conversationFixtureMessageAt = time.Date(2026, 9, 9, 10, 2, 41, 0, time.UTC)
)

// conversationFixtureCase binds one fixture file to the Go half of the
// contract. Response fixtures carry the value the hub projects; request
// fixtures carry decode, because the hub only reads them.
type conversationFixtureCase struct {
	// value is the Go projection compared against the fixture, nil for a
	// request fixture.
	value any
	// decode reads a request fixture into the Go request type.
	decode func(t *testing.T, raw []byte)
	// extra lists dotted paths where the hub deliberately carries more than
	// the client reads. "[]" stands for any array element. A path that names
	// an object exempts every key of it; a path that names one key exempts
	// only that key, which is what a field the client has not bound yet
	// needs.
	extra []string
	// optional lists dotted paths the hub omits when it has no value.
	optional []string
}

func conversationFixtureLinkedRecord() conversationRecord {
	linkedAt := conversationFixtureLinkedAt
	lastMessageAt := conversationFixtureMessageAt
	return conversationRecord{
		ID:               "conv_9f2c41a0b7d84e6fa1c35d92e4780b16",
		OrganizationID:   tracker.OrganizationID("org_4d1f"),
		ProjectID:        tracker.ProjectID("proj_parable"),
		OwnerPrincipalID: "tok_7b21",
		OwnerSubject:     "usr_michael",
		Title:            "Flaky checkout lock renewal",
		Visibility:       conversation.VisibilityShared,
		Status:           conversation.StatusActive,
		WorkItemID:       "wi_3363",
		LinkedAt:         &linkedAt,
		WorkItem: &conversationWorkItem{
			ID:         "wi_3363",
			Identifier: "parable#3363",
			Title:      "Checkout lock renewal waits on a healthy handoff",
			Lane:       "Todo",
		},
		Execution: conversation.Execution{
			Status: conversation.ExecutionRunning,
			Owner: conversation.Owner{
				AttemptID: "att_18f4", RunID: "run_2c9a", LeaseID: "lease_secret", FencingToken: 7,
				RunnerID: "rnr_mac_studio", MachineID: "mac_studio", ThreadID: "thr_0d7e", TurnID: "turn_5",
			},
			Capabilities: conversation.Capabilities{Steer: true, Interrupt: true, Answer: true, Continue: true},
			UpdatedAt:    conversationFixtureUpdatedAt,
		},
		Revision: 12, EventSeq: 40,
		CreatedAt: conversationFixtureCreatedAt, UpdatedAt: conversationFixtureUpdatedAt, LastMessageAt: &lastMessageAt,
	}
}

func conversationFixturePrivateRecord() conversationRecord {
	return conversationRecord{
		ID:               "conv_1b884c37a90d42f0932e5d6c7ae10f45",
		OrganizationID:   tracker.OrganizationID("org_4d1f"),
		ProjectID:        tracker.ProjectID("proj_port_app"),
		OwnerPrincipalID: "tok_7b21",
		OwnerSubject:     "usr_michael",
		Title:            conversationDefaultTitle,
		Visibility:       conversation.VisibilityPrivate,
		Status:           conversation.StatusActive,
		Execution: conversation.Execution{
			Status:       conversation.ExecutionIdle,
			Capabilities: conversation.Capabilities{Interrupt: true},
			UpdatedAt:    conversationFixtureCreatedAt,
		},
		Revision: 1, EventSeq: 2,
		CreatedAt: conversationFixtureCreatedAt, UpdatedAt: conversationFixtureCreatedAt,
	}
}

func conversationFixtureUserMessage() conversationMessageRecord {
	return conversationMessageRecord{
		ID: "msg_6a20f1c4", ConversationID: "conv_9f2c41a0b7d84e6fa1c35d92e4780b16", Seq: 7,
		Role: conversation.RoleUser, Kind: conversation.MessageText,
		Text: "Check whether the lock renewal waits on a healthy handoff.", Delivery: conversation.DeliveryDelivered,
		AttemptID: "att_18f4", TurnID: "turn_5",
		Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: "tok_7b21"}, CommandKey: "cmd_01J8Z0V6C3N4",
		CreatedAt: conversationFixtureMessageAt, UpdatedAt: conversationFixtureMessageAt,
	}
}

func conversationFixtureAssistantMessage() conversationMessageRecord {
	message := conversationFixtureUserMessage()
	message.ID = "msg_6a20f1c5"
	message.Seq = 8
	message.Role = conversation.RoleAssistant
	message.Text = "The renewal path returns before the handoff completes, so the lease can lapse."
	message.Delivery = conversation.DeliveryCompleted
	message.ProviderItemID = "item_9d1"
	message.Actor = conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "rnr_mac_studio"}
	message.CommandKey = ""
	return message
}

// conversationFixtureDataMessage is a status message carrying a structured
// payload in data, the shape the client renders as a card.
func conversationFixtureDataMessage(t *testing.T, id string, seq int64, data any) conversationMessageRecord {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode message data: %v", err)
	}
	return conversationMessageRecord{
		ID: id, ConversationID: "conv_9f2c41a0b7d84e6fa1c35d92e4780b16", Seq: seq,
		Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: "Status", Data: encoded,
		Delivery:  conversation.DeliveryCompleted,
		Actor:     conversation.Actor{Kind: conversation.ActorCoordinator, PrincipalID: "tok_hub"},
		CreatedAt: conversationFixtureUpdatedAt, UpdatedAt: conversationFixtureUpdatedAt,
	}
}

// conversationFixtureFirstMessage is the opening message of a conversation:
// it predates any attempt, so its owner identifiers are null.
func conversationFixtureFirstMessage() conversationMessageRecord {
	message := conversationFixtureUserMessage()
	message.ID = "msg_6a20f1b9"
	message.Seq = 1
	message.Text = "Why does checkout lock renewal fail after a handoff?"
	message.Delivery = conversation.DeliveryCompleted
	message.AttemptID = ""
	message.TurnID = ""
	message.CommandKey = "cmd_01J8Z0V6C3M1"
	return message
}

// conversationFixtureTurnMessage is a data message bound to an owner
// generation; either identifier may be empty and then projects as null.
func conversationFixtureTurnMessage(t *testing.T, id string, seq int64, attempt, turn string, data any) conversationMessageRecord {
	t.Helper()
	message := conversationFixtureDataMessage(t, id, seq, data)
	message.AttemptID = attempt
	message.TurnID = turn
	return message
}

func conversationFixtureQuestion() conversation.Question {
	expires := conversationFixtureUpdatedAt.Add(24 * time.Hour)
	return conversation.Question{
		ID: "q_4f81ba07", ConversationID: "conv_9f2c41a0b7d84e6fa1c35d92e4780b16", MessageID: "msg_6a20f1c7",
		Status: conversation.QuestionPending,
		Owner:  conversation.Owner{AttemptID: "att_18f4", TurnID: "turn_5", LeaseID: "lease_secret", FencingToken: 7},
		Prompts: []conversation.Prompt{{
			ID: "renewal-window", Header: "Renewal window", Question: "Which renewal window should the lock use?",
			Options: []conversation.Option{
				{Label: "Half the lease", Description: "Renew at 50 percent of the lease duration."},
				{Label: "Fixed 20 seconds", Description: "Renew on a fixed cadence regardless of lease length."},
			},
			FreeText: true,
		}},
		ExpiresAt: &expires, CreatedAt: conversationFixtureUpdatedAt, UpdatedAt: conversationFixtureUpdatedAt,
	}
}

func conversationFixtureAnsweredQuestion() conversation.Question {
	question := conversationFixtureQuestion()
	question.Status = conversation.QuestionAnswered
	question.Prompts[0].FreeText = false
	question.Prompts[0].Options = question.Prompts[0].Options[:1]
	question.Answers = map[string][]string{"renewal-window": {"Half the lease"}}
	question.AnsweredBy = "usr_michael"
	question.ExpiresAt = nil
	return question
}

func conversationFixtureMultipleQuestion() conversation.Question {
	question := conversationFixtureQuestion()
	question.ID = "q_4f81ba08"
	question.MessageID = "msg_6a20f1cb"
	question.ExpiresAt = nil
	question.Prompts = []conversation.Prompt{
		{
			ID: "surfaces", Header: "Surfaces", Question: "Which surfaces should the fix cover?",
			Options: []conversation.Option{
				{Label: "Checkout lock", Description: "The renewal path this conversation is about."},
				{Label: "Orphan completion lease", Description: "The neighbouring lease refresh with the same shape."},
			},
			Multiple: true,
		},
		{
			ID: "notes", Header: "Notes", Question: "Anything else the runner should know?",
			Options: []conversation.Option{}, FreeText: true,
		},
	}
	return question
}

func conversationFixtureReceipt() conversation.Receipt {
	return conversation.Receipt{
		Key: "cmd_01J8Z0V6C3N4", Kind: conversation.CommandMessage, Status: conversation.DeliveryDelivered,
		MessageID: "msg_6a20f1c4", UpdatedAt: conversationFixtureMessageAt,
	}
}

// conversationFixtureBootstrap is the payload /chat/bootstrap answers with.
// Since decisions section 12 it is the same extended payload /app/bootstrap
// carries, so the conversation fixture reads a subset of the account one.
func conversationFixtureBootstrap() appBootstrap {
	return appBootstrap{
		Organization: appBootstrapOrganization{ID: "org_4d1f", Name: "Threefold Solutions"},
		Organizations: []appBootstrapOrganization{
			{ID: "org_4d1f", Name: "Threefold Solutions", PublicURL: "https://threefold.detent.cloud", Current: true},
		},
		Actor: appBootstrapActor{PrincipalID: "tok_7b21", Subject: "usr_michael", Email: "info@threefold.solutions", Role: "owner", CanManage: true, CanManageRunners: true},
		Projects: []appBootstrapProject{
			{ID: "proj_parable", Name: "parable", Profile: "native", CanWrite: true, CanManageRunners: true, States: []tracker.NativeState{}},
			{ID: "proj_port_app", Name: "port-app", Profile: "native", CanWrite: true, States: []tracker.NativeState{}},
			{ID: "proj_church_kit", Name: "church-kit", Profile: "native", States: []tracker.NativeState{}},
		},
		CSRFToken:    "csrf_5f0c8ad1c2b34e7a",
		Capabilities: appBootstrapCapabilities{Coordinator: true},
		APIBase:      "/api/v2/organizations/org_4d1f",
		Feature:      appBootstrapFeature{Conversation: true},
	}
}

func conversationFixtureLinkResult() conversationLinkResult {
	record := conversationFixtureLinkedRecord()
	record.Execution = conversation.Execution{
		Status:       conversation.ExecutionWaitingForRunner,
		Capabilities: conversation.Capabilities{Continue: true},
		UpdatedAt:    conversationFixtureLinkedAt,
	}
	return conversationLinkResult{
		Conversation: projectConversation(record),
		Issue: conversationLinkedIssue{
			ID: "wi_3363", Identifier: "parable#3363",
			NativeIssue: tracker.NativeIssue{
				Title: "Checkout lock renewal waits on a healthy handoff", State: "Todo",
				Labels: []string{"bug"}, Assignees: []string{},
				Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{},
				ExternalReferences: []tracker.ExternalReference{},
			},
		},
		Scheduling: conversationScheduling{Lane: "Todo"},
	}
}

// conversationFixtureCases lists every shared fixture. A file without an
// entry fails the test, so a new fixture must be given a Go counterpart.
func conversationFixtureCases(t *testing.T) map[string]conversationFixtureCase {
	t.Helper()
	linked := conversationFixtureLinkedRecord()
	private := conversationFixturePrivateRecord()
	messages := []conversationMessageResource{
		projectMessage(conversationFixtureUserMessage()),
		projectMessage(conversationFixtureAssistantMessage()),
	}
	nextCursor := "eyJzZXEiOjQwfQ"
	command := func(t *testing.T, raw []byte) {
		t.Helper()
		var envelope conversation.Command
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("decode command: %v", err)
		}
		if envelope.Key == "" || !envelope.Kind.Valid() {
			t.Fatalf("command = %#v", envelope)
		}
		if err := conversation.ValidateCommand(envelope); err != nil {
			t.Fatalf("ValidateCommand() error = %v", err)
		}
	}
	return map[string]conversationFixtureCase{
		// The conversation client reads a subset of the extended payload.
		"bootstrap.json":            {value: conversationFixtureBootstrap(), extra: []string{"", "organization", "actor", "projects[]"}},
		"command-answer.json":       {decode: command},
		"command-cancel.json":       {decode: command},
		"command-continue.json":     {decode: command},
		"command-interrupt.json":    {decode: command},
		"command-message.json":      {decode: command},
		"conversation.json":         {value: projectConversation(linked), extra: conversationSection14Fields("")},
		"conversation-private.json": {value: projectConversation(private), extra: conversationSection14Fields("")},
		"conversation-list.json": {value: conversationListPage{
			Conversations: []conversationResource{projectConversation(linked)}, NextCursor: &nextCursor,
		}, extra: conversationSection14Fields("conversations[]")},
		"conversation-snapshot.json": {value: conversationSnapshot{
			Conversation: projectConversation(linked), Messages: messages,
			Questions: []conversation.Question{conversationFixtureQuestion()}, Cursor: 40, HasMore: true,
		}, extra: append(conversationSection14Fields("conversation"), conversationSection14Fields("messages[]")...)},
		"command-retry.json": {decode: command},
		"create-conversation-request.json": {decode: func(t *testing.T, raw []byte) {
			t.Helper()
			var request conversationCreateRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			// The top-level key makes creation idempotent; the first
			// message keeps its own command key (decisions section 10.2).
			if request.Key == "" || request.FirstMessage == nil || request.FirstMessage.Key == "" || request.FirstMessage.Key == request.Key || request.FirstMessage.Text == "" {
				t.Fatalf("create request = %#v", request)
			}
		}},
		"create-conversation.json": {value: func() conversationCreatedResponse {
			receipt := conversationFixtureReceipt()
			return conversationCreatedResponse{Conversation: projectConversation(private), Receipt: &receipt}
		}(), extra: conversationSection14Fields("conversation")},
		"error.json":                      {value: conversationAlreadyLinked("conv_9f2c41a0b7d84e6fa1c35d92e4780b16")},
		"error-stale-execution.json":      {value: conversationStaleOwner("The attempt this control targeted is no longer current.", "att_18f4", "att_2b70")},
		"event-closed.json":               {value: conversationFixtureEvent(conversation.EventClosed, map[string]string{"reason": conversationClosedCursorExpired})},
		"event-conversation-updated.json": {value: conversationFixtureEvent(conversation.EventConversationUpdated, projectConversation(linked)), extra: conversationSection14Fields("data")},
		"event-heartbeat.json":            {value: conversationFixtureEvent(conversation.EventHeartbeat, map[string]any{"seq": int64(41)})},
		"event-message-delta.json":        {value: conversationFixtureEvent(conversation.EventMessageDelta, conversationDeltaBody("msg_6a20f1c5", 3, " so the lease can lapse."))},
		"execution.json": {value: projectExecution(conversation.Execution{
			Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: "thr_0d7e"},
			Capabilities: conversation.Capabilities{Continue: true}, UpdatedAt: conversationFixtureUpdatedAt,
		})},
		"link-request.json": {decode: func(t *testing.T, raw []byte) {
			t.Helper()
			var request conversationLinkRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatalf("decode link request: %v", err)
			}
			if request.Key == "" || !request.ShareHistory || request.Issue.Title == "" || request.Issue.Description == "" {
				t.Fatalf("link request = %#v", request)
			}
		}},
		"link-response.json":     {value: conversationFixtureLinkResult(), extra: append(conversationSection14Fields(""), append([]string{"issue"}, conversationSection14Fields("conversation")...)...)},
		"message-user.json":      {value: projectMessage(conversationFixtureUserMessage()), extra: conversationSection14Fields("")},
		"message-assistant.json": {value: projectMessage(conversationFixtureAssistantMessage()), extra: conversationSection14Fields("")},
		"message-attention.json": {value: projectMessage(conversationFixtureDataMessage(t, "msg_6a20f1ca", 13, map[string]any{
			"attention": []map[string]any{
				{
					"work_item_id": "wi_3363", "title": "Checkout lock renewal waits on a healthy handoff",
					"state": "waiting_input", "reason": "The runner asked which renewal window to use.", "url": "/chat/issues/wi_3363",
				},
				{
					"work_item_id": "wi_3401", "title": "Board lane shows scheduler dispatch waits",
					"state": "failed", "reason": "The last attempt failed while applying the migration.", "url": "/chat/issues/wi_3401",
				},
			},
		})), extra: conversationSection14Fields("")},
		"message-issue.json": {value: projectMessage(conversationFixtureDataMessage(t, "msg_6a20f1c9", 12, map[string]any{
			"issue": conversationIssueResult(conversationFixtureLinkedRecord()),
		})), extra: conversationSection14Fields("")},
		"message-page.json": {value: conversationMessagesPage{Messages: []conversationMessageResource{projectMessage(conversationFixtureFirstMessage())}, NextCursor: nil}, extra: conversationSection14Fields("messages[]")},
		"message-proposal.json": {value: projectMessage(conversationFixtureTurnMessage(t, "msg_6a20f1c8", 11, "", "turn_5", map[string]any{
			"proposal": map[string]any{
				"project_id": "proj_parable", "title": "Checkout lock renewal waits on a healthy handoff",
				"objective": "Move the renewal behind the handoff acknowledgement.",
			},
		})), extra: conversationSection14Fields("")},
		"message-status.json": {value: projectMessage(conversationFixtureTurnMessage(t, "msg_6a20f1c6", 9, "att_18f4", "", map[string]any{
			"available_slots": 0, "total_slots": 2,
		})), extra: conversationSection14Fields("")},
		"question.json":          {value: conversationFixtureQuestion()},
		"question-answered.json": {value: conversationFixtureAnsweredQuestion()},
		"question-multiple.json": {value: conversationFixtureMultipleQuestion()},
		"receipt.json":           {value: conversationFixtureReceipt()},
		"receipt-unknown.json": {
			value: func() conversation.Receipt {
				receipt := conversationFixtureReceipt()
				receipt.Key = "cmd_01J8Z0V6C3N5"
				receipt.Status = conversation.DeliveryUnknown
				receipt.MessageID = ""
				receipt.Error = &conversation.ReceiptError{Code: "queue_full", Message: "The control queue is full. Retry later."}
				return receipt
			}(),
			// conversation.ReceiptError has no details field yet; the client
			// reads it as optional.
			optional: []string{"error.details"},
		},
	}
}

// conversationSection14Fields names the fields decisions sections 14 and 17
// add to the conversation and message resources and to the link response. The
// client owns the shared fixtures; until it has bound these fields, the hub
// carries more than the fixture shows, and the Go half of the contract says so
// here rather than by dropping the field. Each returned path names one key of
// the object at prefix, so every other key of that object is still compared.
func conversationSection14Fields(prefix string) []string {
	fields := []string{"preferences", "references", "next", "attachments"}
	paths := make([]string, 0, len(fields))
	for _, field := range fields {
		paths = append(paths, conversationJoinPath(prefix, field))
	}
	return paths
}

// conversationFixtureEvent wraps a projected event body the way the client
// reads an SSE frame: the type comes from the event field, the body from data.
func conversationFixtureEvent(eventType conversation.EventType, data any) map[string]any {
	return map[string]any{"type": string(eventType), "data": data}
}

func TestConversationContractFixtures(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(conversationFixtureDirectory)
	if err != nil {
		t.Skipf("shared contract fixtures are not available at %s: %v", conversationFixtureDirectory, err)
	}
	cases := conversationFixtureCases(t)
	for name, fixture := range hostedAccountFixtureCases() {
		cases[name] = fixture
	}
	for name, fixture := range nativeWorkFixtureCases(t) {
		cases[name] = fixture
	}
	names := make([]string, 0, len(entries))
	present := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
		present[entry.Name()] = true
	}
	for name, fixture := range usageFixtureCases(t, conversationFixtureDirectory, present) {
		cases[name] = fixture
	}
	slices.Sort(names)
	bound := make([]string, 0, len(cases))
	for name := range cases {
		bound = append(bound, name)
	}
	slices.Sort(bound)
	if !slices.Equal(names, bound) {
		t.Fatalf("fixture files %v do not match the bound cases %v", names, bound)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(conversationFixtureDirectory, name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			fixture := cases[name]
			if fixture.decode != nil {
				fixture.decode(t, raw)
				return
			}
			var want any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			encoded, err := json.Marshal(fixture.value)
			if err != nil {
				t.Fatalf("encode Go value: %v", err)
			}
			var got any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode Go value: %v", err)
			}
			compareConversationShape(t, "", want, got, conversationPathSet(fixture.extra), conversationPathSet(fixture.optional))
		})
	}
}

func conversationPathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		set[path] = true
	}
	return set
}

func conversationJoinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// compareConversationShape asserts that the Go payload carries exactly the
// fixture's key set at every level and agrees on whether a value is null.
// Values themselves are not compared: the fixtures are examples, the shape
// is the contract.
func compareConversationShape(t *testing.T, path string, want, got any, extra, optional map[string]bool) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: fixture null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	if want == nil {
		return
	}
	switch wanted := want.(type) {
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: fixture is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			child := conversationJoinPath(path, key)
			other, present := gotten[key]
			if !present {
				if !optional[child] {
					t.Errorf("%s: Go value is missing key %q", label, key)
				}
				continue
			}
			compareConversationShape(t, child, value, other, extra, optional)
		}
		if extra[path] {
			return
		}
		for key := range gotten {
			if _, present := wanted[key]; present || extra[conversationJoinPath(path, key)] {
				continue
			}
			t.Errorf("%s: Go value has extra key %q", label, key)
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: fixture is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: fixture has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			compareConversationShape(t, path+"[]", wanted[i], gotten[i], extra, optional)
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: fixture is %T, Go value is %T", label, want, got)
		}
	}
}
