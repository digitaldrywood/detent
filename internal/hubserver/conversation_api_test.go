package hubserver

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

// conversationAPIFixture is a token-authenticated hub with the conversation
// product enabled. The coordinator stays the no-op stub, so unlinked
// messages report coordinator_unavailable.
type conversationAPIFixture struct {
	nativeFixture
	ownerID string
	other   string
	otherID string
}

func newConversationAPIFixture(t *testing.T, cfg *ConversationConfig) conversationAPIFixture {
	t.Helper()
	if cfg == nil {
		cfg = &ConversationConfig{Enabled: true}
	}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Conversation: cfg})
	f := newNativeFixture(t, service, "", "chat")
	var ownerID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = ?", "operator-chat").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "operator-chat-other", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID})
	requireNativeStatus(t, response, http.StatusNoContent)
	return conversationAPIFixture{nativeFixture: f, ownerID: ownerID, other: token.Token, otherID: token.ID}
}

type conversationCreateResponse struct {
	Conversation conversationResource  `json:"conversation"`
	Receipt      *conversation.Receipt `json:"receipt"`
}

type conversationSnapshotResponse struct {
	Conversation conversationResource          `json:"conversation"`
	Messages     []conversationMessageResource `json:"messages"`
	Questions    []map[string]any              `json:"questions"`
	Cursor       int64                         `json:"cursor"`
	HasMore      bool                          `json:"has_more"`
}

type conversationListResponse struct {
	Conversations []conversationResource `json:"conversations"`
	NextCursor    *string                `json:"next_cursor"`
}

type conversationLinkResponse struct {
	Conversation conversationResource    `json:"conversation"`
	Issue        conversationLinkedIssue `json:"issue"`
	Scheduling   struct {
		Lane        string `json:"lane"`
		RunnerBound bool   `json:"runner_bound"`
	} `json:"scheduling"`
}

func (f conversationAPIFixture) create(t *testing.T, token string, body map[string]any) conversationCreateResponse {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", token, conversationCreateBody(body))
	requireNativeStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	return created
}

// conversationCreateBody fills the top-level idempotency key creation
// requires (decisions section 10.2) unless the test names one itself. Every
// call gets a fresh key: the key is scoped to the actor and the route, so a
// reused one would replay the first conversation.
func conversationCreateBody(body map[string]any) map[string]any {
	filled := make(map[string]any, len(body)+1)
	for name, value := range body {
		filled[name] = value
	}
	if _, named := filled["key"]; !named {
		filled["key"] = newNativeID("cck")
	}
	return filled
}

func (f conversationAPIFixture) command(t *testing.T, token, id string, command conversation.Command) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+id+"/commands", token, command)
}

func (f conversationAPIFixture) snapshot(t *testing.T, token, id string) conversationSnapshotResponse {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+id, token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot conversationSnapshotResponse
	decodeHubResponse(t, response, &snapshot)
	return snapshot
}

// settle runs the settle sweep against one conversation with a window that
// is already over, so the test does not have to wait a day. The sweep only
// settles a finished or idle conversation, so an execution still waiting for
// a runner is finished first.
func (f conversationAPIFixture) settle(t *testing.T, id string) {
	t.Helper()
	service := f.service.conversations
	if err := service.transact(t.Context(), func(tx *sql.Tx, now time.Time) error {
		record, err := service.store.readConversationByID(t.Context(), tx, id)
		if err != nil {
			return err
		}
		if record.Execution.Status.Terminal() || record.Execution.Status == conversation.ExecutionIdle {
			return nil
		}
		execution := record.Execution
		execution.Status = conversation.ExecutionCompleted
		return service.updateExecution(t.Context(), tx, &record, execution, now)
	}); err != nil {
		t.Fatalf("finish execution: %v", err)
	}
	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	settled, err := service.settleConversation(t.Context(), id, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("settleConversation() error = %v", err)
	}
	if !settled {
		t.Fatalf("settleConversation(%s) settled nothing", id)
	}
}

func (f conversationAPIFixture) link(t *testing.T, token, id, key string, share bool, title string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+id+"/link", token, map[string]any{
		"key": key, "share_history": share, "issue": map[string]any{"title": title, "description": "From the chat"},
	})
}

func decodeNativeError(t *testing.T, response *httptest.ResponseRecorder) nativeError {
	t.Helper()
	var failure nativeError
	decodeHubResponse(t, response, &failure)
	return failure
}

func requireNativeError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) nativeError {
	t.Helper()
	requireNativeStatus(t, response, status)
	failure := decodeNativeError(t, response)
	if failure.Code != code {
		t.Fatalf("error code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}

func TestConversationAPICreate(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	tests := []struct {
		name      string
		body      map[string]any
		wantTitle string
		receipt   bool
	}{
		{name: "explicit title", body: map[string]any{"title": "  Plan the release  "}, wantTitle: "Plan the release"},
		{name: "derived title", body: map[string]any{"first_message": map[string]any{"key": "first", "text": "Fix the login bug\nwith details"}}, wantTitle: "Fix the login bug", receipt: true},
		{name: "derived title truncates", body: map[string]any{"first_message": map[string]any{"key": "first", "text": strings.Repeat("é", 100)}}, wantTitle: strings.Repeat("é", 80), receipt: true},
		{name: "default title", body: map[string]any{}, wantTitle: "New chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created := f.create(t, f.token, tt.body)
			record := created.Conversation
			if record.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", record.Title, tt.wantTitle)
			}
			if !strings.HasPrefix(record.ID, "conv_") || record.Visibility != conversation.VisibilityPrivate || record.Status != conversation.StatusActive {
				t.Errorf("conversation = %#v", record)
			}
			if record.Owner.PrincipalID != f.ownerID || record.ProjectID != f.project.ID || record.Execution.Status != conversation.ExecutionIdle {
				t.Errorf("owner/project/execution = %#v", record)
			}
			if !tt.receipt {
				if created.Receipt != nil {
					t.Fatalf("receipt = %#v, want none", created.Receipt)
				}
				return
			}
			if created.Receipt == nil {
				t.Fatal("receipt is missing")
			}
			if created.Receipt.Key != "first" || created.Receipt.Status != conversation.DeliverySaved || created.Receipt.Error != nil || created.Receipt.MessageID == "" {
				t.Fatalf("receipt = %#v", created.Receipt)
			}
			snapshot := f.snapshot(t, f.token, record.ID)
			if len(snapshot.Messages) != 1 || snapshot.Messages[0].Kind != conversation.MessageText || snapshot.Messages[0].Role != conversation.RoleUser || snapshot.Messages[0].Delivery != conversation.DeliverySaved {
				t.Fatalf("messages = %#v", snapshot.Messages)
			}
			if snapshot.Cursor != snapshot.Conversation.EventSeq || snapshot.Cursor < 2 || record.LastMessageAt == nil {
				t.Fatalf("cursor = %d event_seq = %d last_message_at = %v", snapshot.Cursor, snapshot.Conversation.EventSeq, record.LastMessageAt)
			}
		})
	}
	t.Run("invalid body", func(t *testing.T) {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, map[string]any{"unexpected": true})
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, map[string]any{"first_message": map[string]any{"key": "", "text": "x"}})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
}

func TestConversationAPIListVisibility(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	private := f.create(t, f.token, map[string]any{"title": "Private plan"}).Conversation
	shared := f.create(t, f.token, map[string]any{"title": "Shared release"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, shared.ID, "link-shared", true, "Release checklist"), http.StatusOK)
	list := func(token, path string) conversationListResponse {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page conversationListResponse
		decodeHubResponse(t, response, &page)
		return page
	}
	ids := func(page conversationListResponse) []string {
		var out []string
		for _, record := range page.Conversations {
			out = append(out, record.ID)
		}
		return out
	}
	organizationPath := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/conversations"
	tests := []struct {
		name  string
		token string
		path  string
		want  []string
	}{
		{name: "owner sees both newest first", token: f.token, path: f.base + "/conversations", want: []string{shared.ID, private.ID}},
		{name: "other sees only shared", token: f.other, path: f.base + "/conversations", want: []string{shared.ID}},
		{name: "title filter", token: f.token, path: f.base + "/conversations?q=PLAN", want: []string{private.ID}},
		{name: "organization list", token: f.token, path: organizationPath, want: []string{shared.ID, private.ID}},
		{name: "organization list other", token: f.other, path: organizationPath + "?q=release", want: []string{shared.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ids(list(tt.token, tt.path))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ids = %v, want %v", got, tt.want)
			}
		})
	}
	t.Run("pagination", func(t *testing.T) {
		first := list(f.token, f.base+"/conversations?limit=1")
		if len(first.Conversations) != 1 || first.NextCursor == nil || first.Conversations[0].ID != shared.ID {
			t.Fatalf("first page = %#v", first)
		}
		second := list(f.token, f.base+"/conversations?limit=1&cursor="+*first.NextCursor)
		if len(second.Conversations) != 1 || second.Conversations[0].ID != private.ID || second.NextCursor != nil {
			t.Fatalf("second page = %#v", second)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if !strings.Contains(response.Body.String(), `"next_cursor":null`) {
			t.Fatalf("last page must carry a null cursor: %s", response.Body.String())
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations?cursor=not-a-cursor", f.token, nil)
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	})
	t.Run("private read is opaque", func(t *testing.T) {
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+private.ID, f.other, nil)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+shared.ID, f.other, nil)
		requireNativeStatus(t, response, http.StatusOK)
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/conv_missing", f.token, nil)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
	})
	t.Run("cross project", func(t *testing.T) {
		otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "chat-second")
		response := performHubAPIRequest(t, f.service, http.MethodGet, otherProject.base+"/conversations/"+shared.ID, otherProject.token, nil)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+shared.ID, otherProject.token, nil)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
		page := list(otherProject.token, organizationPath)
		if len(page.Conversations) != 0 {
			t.Fatalf("organization list = %#v, want none", page.Conversations)
		}
	})
	// Settled replaces Archive: both groups are listed by default, active
	// first, and the settled filter selects one group (decisions section 14).
	t.Run("settled filter", func(t *testing.T) {
		f.settle(t, private.ID)
		if got := ids(list(f.token, f.base+"/conversations")); strings.Join(got, ",") != shared.ID+","+private.ID {
			t.Fatalf("ids = %v, want the active conversation first", got)
		}
		if got := ids(list(f.token, f.base+"/conversations?settled=false")); strings.Join(got, ",") != shared.ID {
			t.Fatalf("settled=false ids = %v", got)
		}
		if got := ids(list(f.token, f.base+"/conversations?settled=true")); strings.Join(got, ",") != private.ID {
			t.Fatalf("settled=true ids = %v", got)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations?settled=maybe", f.token, nil)
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
}

func TestConversationAPIMessagesPagination(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Paging"}).Conversation
	for index := range 5 {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "m" + string(rune('a'+index)), Kind: conversation.CommandMessage, Text: "message " + string(rune('a'+index))})
		requireNativeStatus(t, response, http.StatusOK)
	}
	snapshot := f.snapshot(t, f.token, record.ID)
	if len(snapshot.Messages) != 5 || snapshot.Messages[0].Seq != 1 || snapshot.Messages[4].Seq != 5 || snapshot.HasMore {
		t.Fatalf("messages = %#v has_more = %t", snapshot.Messages, snapshot.HasMore)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=4&limit=2", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page struct {
		Messages   []conversationMessageResource `json:"messages"`
		NextCursor *string                       `json:"next_cursor"`
	}
	decodeHubResponse(t, response, &page)
	if len(page.Messages) != 2 || page.Messages[0].Seq != 2 || page.Messages[1].Seq != 3 || page.NextCursor == nil || *page.NextCursor != "2" {
		t.Fatalf("page = %#v next_cursor = %v", page.Messages, page.NextCursor)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=2", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	if len(page.Messages) != 1 || page.Messages[0].Seq != 1 || page.NextCursor != nil {
		t.Fatalf("oldest page = %#v next_cursor = %v", page.Messages, page.NextCursor)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=x", f.token, nil)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
}

func TestConversationAPICommands(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Commands"}).Conversation
	message := conversation.Command{Key: "hello", Kind: conversation.CommandMessage, Text: "Hello"}
	t.Run("validation", func(t *testing.T) {
		for _, command := range []conversation.Command{
			{Key: "", Kind: conversation.CommandMessage, Text: "x"},
			{Key: "k", Kind: "bogus"},
			{Key: "k", Kind: conversation.CommandMessage, Text: "   "},
			{Key: "k", Kind: conversation.CommandInterrupt},
		} {
			response := f.command(t, f.token, record.ID, command)
			requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
		}
	})
	t.Run("message replay and conflict", func(t *testing.T) {
		first := f.command(t, f.token, record.ID, message)
		requireNativeStatus(t, first, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, first, &receipt)
		if receipt.Status != conversation.DeliverySaved || receipt.Error != nil || receipt.MessageID == "" {
			t.Fatalf("receipt = %#v", receipt)
		}
		replay := f.command(t, f.token, record.ID, message)
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != first.Body.String() {
			t.Fatalf("replay = %s, want %s", replay.Body.String(), first.Body.String())
		}
		conflict := f.command(t, f.token, record.ID, conversation.Command{Key: "hello", Kind: conversation.CommandMessage, Text: "Different"})
		requireNativeError(t, conflict, http.StatusConflict, "idempotency_conflict")
		snapshot := f.snapshot(t, f.token, record.ID)
		if len(snapshot.Messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(snapshot.Messages))
		}
	})
	t.Run("unsupported and rejected controls", func(t *testing.T) {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "cont", Kind: conversation.CommandContinue})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "unsupported_control")
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "intr", Kind: conversation.CommandInterrupt, Expected: conversation.Expected{AttemptID: "att_x"}})
		requireNativeError(t, response, http.StatusConflict, "stale_execution")
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "ans", Kind: conversation.CommandAnswer, QuestionID: "q_missing", Answers: map[string][]string{"p": {"a"}}})
		requireNativeError(t, response, http.StatusNotFound, "not_found")
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "cancel", Kind: conversation.CommandCancel})
		requireNativeStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, response, &receipt)
		if receipt.Status != conversation.DeliveryRejected || receipt.Error == nil || receipt.Error.Code != "no_active_turn" {
			t.Fatalf("cancel receipt = %#v", receipt)
		}
	})
	t.Run("other principal cannot see private", func(t *testing.T) {
		response := f.command(t, f.other, record.ID, message)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
	})
	// A settled conversation is not read-only: the command is accepted and
	// the conversation goes back to active (decisions section 14).
	t.Run("a settled conversation accepts commands and unsettles", func(t *testing.T) {
		settled := f.create(t, f.token, map[string]any{"title": "Old"}).Conversation
		f.settle(t, settled.ID)
		if got := f.snapshot(t, f.token, settled.ID).Conversation.Status; got != conversation.StatusSettled {
			t.Fatalf("status = %q, want settled", got)
		}
		requireNativeStatus(t, f.command(t, f.token, settled.ID, message), http.StatusOK)
		if got := f.snapshot(t, f.token, settled.ID).Conversation.Status; got != conversation.StatusActive {
			t.Fatalf("status after a command = %q, want active", got)
		}
	})
}

func TestConversationAPIAnswer(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Questions"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Ask me"), http.StatusOK)
	store := f.service.conversations.store
	ctx := t.Context()
	tx, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now()
	linked, err := store.readConversation(ctx, tx, f.project.OrganizationID, f.project.ID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	execution := conversation.Execution{Status: conversation.ExecutionWaitingInput, Owner: conversation.Owner{AttemptID: "att_1", RunID: "run_1", TurnID: "turn_1"}, Capabilities: conversation.Capabilities{Answer: true}}
	if err := f.service.conversations.updateExecution(ctx, tx, &linked, execution, now); err != nil {
		t.Fatal(err)
	}
	question := conversationQuestionRecord{Question: conversation.Question{
		ID: conversation.NewQuestionID(), ConversationID: record.ID, Status: conversation.QuestionPending,
		Owner: execution.Owner, Prompts: []conversation.Prompt{{ID: "color", Question: "Which color?", Options: []conversation.Option{{Label: "red"}, {Label: "blue"}}}},
		CreatedAt: now, UpdatedAt: now,
	}}
	if err := store.upsertQuestion(ctx, tx, question); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, f.token, record.ID)
	if len(snapshot.Questions) != 1 || snapshot.Questions[0]["id"] != question.ID {
		t.Fatalf("pending questions = %#v", snapshot.Questions)
	}
	tests := []struct {
		name    string
		command conversation.Command
		status  int
		code    string
	}{
		{name: "wrong prompt", command: conversation.Command{Key: "a1", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: map[string][]string{"size": {"big"}}}, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "stale attempt", command: conversation.Command{Key: "a2", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: map[string][]string{"color": {"red"}}, Expected: conversation.Expected{AttemptID: "att_old"}}, status: http.StatusConflict, code: "stale_execution"},
		{name: "accepted", command: conversation.Command{Key: "a3", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: map[string][]string{"color": {"red"}}, Expected: conversation.Expected{AttemptID: "att_1"}}, status: http.StatusOK},
		{name: "second answer", command: conversation.Command{Key: "a4", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: map[string][]string{"color": {"blue"}}}, status: http.StatusConflict, code: "question_already_answered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.command(t, f.token, record.ID, tt.command)
			if tt.code != "" {
				requireNativeError(t, response, tt.status, tt.code)
				return
			}
			requireNativeStatus(t, response, tt.status)
			var receipt conversation.Receipt
			decodeHubResponse(t, response, &receipt)
			if receipt.Status != conversation.DeliveryQueued || receipt.QuestionID != question.ID || receipt.MessageID == "" {
				t.Fatalf("receipt = %#v", receipt)
			}
		})
	}
	snapshot = f.snapshot(t, f.token, record.ID)
	var answer *conversationMessageResource
	for index := range snapshot.Messages {
		if snapshot.Messages[index].Kind == conversation.MessageAnswer {
			answer = &snapshot.Messages[index]
		}
	}
	if answer == nil || answer.Text != "Answered: red" || answer.Delivery != conversation.DeliveryQueued || answer.AttemptID == nil || *answer.AttemptID != "att_1" {
		t.Fatalf("answer message = %#v", answer)
	}
	if len(snapshot.Questions) != 1 || snapshot.Questions[0]["status"] != string(conversation.QuestionSending) || snapshot.Questions[0]["answered_by"] != f.ownerID {
		t.Fatalf("question = %#v", snapshot.Questions)
	}
}

func TestConversationAPILink(t *testing.T) {
	t.Parallel()
	// The first message queues a coordinator control before the link, so the
	// queue holds one control already.
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 3})
	record := f.create(t, f.token, map[string]any{"title": "Ship it", "first_message": map[string]any{"key": "first", "text": "Please ship the release"}}).Conversation
	t.Run("requires share_history", func(t *testing.T) {
		response := f.link(t, f.token, record.ID, "no-share", false, "Ship the release")
		requireNativeError(t, response, http.StatusUnprocessableEntity, "share_history_required")
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, map[string]any{"key": "no-title", "share_history": true, "issue": map[string]any{"title": " "}})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
	var linked conversationLinkResponse
	t.Run("links and shares", func(t *testing.T) {
		response := f.link(t, f.token, record.ID, "link", true, "Ship the release")
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &linked)
		if linked.Conversation.Visibility != conversation.VisibilityShared || linked.Conversation.WorkItemID == nil || *linked.Conversation.WorkItemID != string(linked.Issue.WorkItemID) || linked.Conversation.LinkedAt == nil {
			t.Fatalf("conversation = %#v", linked.Conversation)
		}
		if linked.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner || linked.Scheduling.Lane != "Todo" || linked.Scheduling.RunnerBound {
			t.Fatalf("execution = %#v scheduling = %#v", linked.Conversation.Execution, linked.Scheduling)
		}
		if linked.Issue.Title != "Ship the release" || !strings.Contains(linked.Issue.Body, "Conversation: "+record.ID) || linked.Issue.State != "Todo" {
			t.Fatalf("issue = %#v", linked.Issue)
		}
		if linked.Issue.ID != string(linked.Issue.WorkItemID) || linked.Issue.Identifier != f.project.Name+"#1" {
			t.Fatalf("issue identity = %q %q", linked.Issue.ID, linked.Issue.Identifier)
		}
		var audience int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_audience_events WHERE conversation_id = ? AND from_visibility = 'private' AND to_visibility = 'shared' AND actor_principal_id = ?", record.ID, f.ownerID).Scan(&audience); err != nil {
			t.Fatal(err)
		}
		if audience != 1 {
			t.Fatalf("audience events = %d, want 1", audience)
		}
		snapshot := f.snapshot(t, f.other, record.ID)
		last := snapshot.Messages[len(snapshot.Messages)-1]
		if last.Role != conversation.RoleSystem || last.Kind != conversation.MessageStatus || !strings.Contains(last.Text, "Ship the release") {
			t.Fatalf("status message = %#v", last)
		}
		replay := f.link(t, f.token, record.ID, "link", true, "Ship the release")
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != response.Body.String() {
			t.Fatalf("replay = %s, want %s", replay.Body.String(), response.Body.String())
		}
	})
	t.Run("second link conflicts", func(t *testing.T) {
		response := f.link(t, f.token, record.ID, "again", true, "Another issue")
		failure := requireNativeError(t, response, http.StatusConflict, "conversation_already_linked")
		if failure.Details["existing_conversation_id"] != record.ID {
			t.Fatalf("details = %#v", failure.Details)
		}
		var issues int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues i WHERE i.organization_id = ? AND i.project_id = ?",
			f.project.OrganizationID, f.project.ID).Scan(&issues); err != nil {
			t.Fatal(err)
		}
		if issues != 1 {
			t.Fatalf("issues = %d, want 1 (no issue created by a rejected link)", issues)
		}
	})
	t.Run("linked message queues", func(t *testing.T) {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "q1", Kind: conversation.CommandMessage, Text: "Also update the docs"})
		requireNativeStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, response, &receipt)
		if receipt.Status != conversation.DeliveryQueued {
			t.Fatalf("receipt = %#v", receipt)
		}
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "q-stale", Kind: conversation.CommandMessage, Text: "Steer", Expected: conversation.Expected{AttemptID: "att_none"}})
		requireNativeError(t, response, http.StatusConflict, "stale_execution")
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "q2", Kind: conversation.CommandMessage, Text: "And tests"})
		requireNativeStatus(t, response, http.StatusOK)
		response = f.command(t, f.token, record.ID, conversation.Command{Key: "q3", Kind: conversation.CommandMessage, Text: "Too many"})
		requireNativeError(t, response, http.StatusServiceUnavailable, "queue_full")
		var commands int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_commands WHERE conversation_id = ? AND key = 'q3'", record.ID).Scan(&commands); err != nil {
			t.Fatal(err)
		}
		if commands != 0 {
			t.Fatal("queue_full must not persist the command")
		}
		response = f.command(t, f.other, record.ID, conversation.Command{Key: "cancel", Kind: conversation.CommandCancel})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "unsupported_control")
	})
	t.Run("continue records intent", func(t *testing.T) {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "cont", Kind: conversation.CommandContinue})
		requireNativeStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, response, &receipt)
		if receipt.Status != conversation.DeliverySaved || receipt.MessageID == "" {
			t.Fatalf("receipt = %#v", receipt)
		}
		snapshot := f.snapshot(t, f.token, record.ID)
		if snapshot.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
			t.Fatalf("execution = %#v", snapshot.Conversation.Execution)
		}
	})
	t.Run("private conversation of another principal is opaque", func(t *testing.T) {
		second := f.create(t, f.token, map[string]any{"title": "Second"}).Conversation
		response := f.link(t, f.other, second.ID, "other-link", true, "Other")
		requireNativeError(t, response, http.StatusNotFound, "not_found")
	})
}

func TestConversationAPIPatchAndSettle(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Rename me"}).Conversation
	path := f.base + "/conversations/" + record.ID
	tests := []struct {
		name   string
		token  string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{name: "rename", token: f.token, method: http.MethodPatch, path: path, body: map[string]any{"title": " Renamed "}, status: http.StatusOK},
		{name: "rename empty", token: f.token, method: http.MethodPatch, path: path, body: map[string]any{"title": "  "}, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "rename too long", token: f.token, method: http.MethodPatch, path: path, body: map[string]any{"title": strings.Repeat("x", 201)}, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "rename by other is opaque", token: f.other, method: http.MethodPatch, path: path, body: map[string]any{"title": "Nope"}, status: http.StatusNotFound, code: "not_found"},
		{name: "empty patch", token: f.token, method: http.MethodPatch, path: path, body: map[string]any{}, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		// The archive and unarchive routes are gone, not merely refused.
		{name: "archive is gone", token: f.token, method: http.MethodPost, path: path + "/archive", status: http.StatusNotFound},
		{name: "unarchive is gone", token: f.token, method: http.MethodPost, path: path + "/unarchive", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, tt.method, tt.path, tt.token, tt.body)
			if tt.code != "" {
				requireNativeError(t, response, tt.status, tt.code)
				return
			}
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.status, response.Body.String())
			}
			if tt.status != http.StatusOK {
				return
			}
			var updated conversationResource
			decodeHubResponse(t, response, &updated)
			switch tt.name {
			case "rename":
				if updated.Title != "Renamed" || updated.Revision != record.Revision+1 {
					t.Fatalf("updated = %#v", updated)
				}
			}
		})
	}
}

// TestConversationAPILinkedWorkItem covers the linked-issue half of the
// conversation resource: the summary the client renders, the durable issue
// result message and the link constraint reported with the existing link.
func TestConversationAPILinkedWorkItem(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Ship it"}).Conversation
	if record.WorkItem != nil {
		t.Fatalf("an unlinked conversation must report work_item null, got %#v", record.WorkItem)
	}
	execution := record.Execution
	if execution.AttemptID != nil || execution.RunID != nil || execution.RunnerID != nil || execution.ThreadID != nil || execution.TurnID != nil {
		t.Fatalf("empty owner identifiers must project as null, got %#v", execution)
	}
	response := f.link(t, f.token, record.ID, "link", true, "Ship the release")
	requireNativeStatus(t, response, http.StatusOK)
	var linked conversationLinkResponse
	decodeHubResponse(t, response, &linked)
	want := conversationWorkItemResource{
		ID: string(linked.Issue.WorkItemID), Identifier: f.project.Name + "#1", Title: "Ship the release", Lane: "Todo",
	}
	if linked.Conversation.WorkItem == nil || *linked.Conversation.WorkItem != want {
		t.Fatalf("work item = %#v, want %#v", linked.Conversation.WorkItem, want)
	}
	snapshot := f.snapshot(t, f.other, record.ID)
	if snapshot.Conversation.WorkItem == nil || *snapshot.Conversation.WorkItem != want {
		t.Fatalf("snapshot work item = %#v, want %#v", snapshot.Conversation.WorkItem, want)
	}
	t.Run("issue result survives reload", func(t *testing.T) {
		last := snapshot.Messages[len(snapshot.Messages)-1]
		if last.Role != conversation.RoleSystem || last.Kind != conversation.MessageStatus || !strings.Contains(last.Text, "Ship the release") {
			t.Fatalf("status message = %#v", last)
		}
		data, ok := last.Data.(map[string]any)
		if !ok {
			t.Fatalf("message data = %#v", last.Data)
		}
		issue, ok := data["issue"].(map[string]any)
		if !ok {
			t.Fatalf("issue result = %#v", data)
		}
		for key, expected := range map[string]any{
			"id": want.ID, "identifier": want.Identifier, "title": want.Title,
			"state": want.Lane, "lane": want.Lane, "runner_bound": false,
		} {
			if issue[key] != expected {
				t.Errorf("issue result %s = %#v, want %#v", key, issue[key], expected)
			}
		}
		if len(issue) != 6 {
			t.Errorf("issue result = %#v, want exactly the six contract fields", issue)
		}
	})
	t.Run("a different key on a linked conversation conflicts", func(t *testing.T) {
		failure := requireNativeError(t, f.link(t, f.token, record.ID, "relink", true, "Another issue"), http.StatusConflict, "conversation_already_linked")
		if failure.Details["existing_conversation_id"] != record.ID {
			t.Fatalf("details = %#v", failure.Details)
		}
	})
	t.Run("the same key and payload replays the stored result", func(t *testing.T) {
		replay := f.link(t, f.token, record.ID, "link", true, "Ship the release")
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != response.Body.String() {
			t.Fatalf("replay = %s, want %s", replay.Body.String(), response.Body.String())
		}
	})
	t.Run("a priority is the numeric rank", func(t *testing.T) {
		second := f.create(t, f.token, map[string]any{"title": "Priority"}).Conversation
		link := func(key string, priority any) *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+second.ID+"/link", f.token, map[string]any{
				"key": key, "share_history": true, "issue": map[string]any{"title": "Ranked", "description": "From the chat", "priority": priority},
			})
		}
		requireNativeError(t, link("named", "High"), http.StatusUnprocessableEntity, "invalid_request")
		requireNativeStatus(t, link("ranked", "1"), http.StatusOK)
	})
	t.Run("a stale control reports both attempts", func(t *testing.T) {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "steer", Kind: conversation.CommandMessage, Text: "Steer", Expected: conversation.Expected{AttemptID: "att_gone"}})
		failure := requireNativeError(t, response, http.StatusConflict, "stale_execution")
		if failure.Details["expected_attempt_id"] != "att_gone" || failure.Details["current_attempt_id"] != nil {
			t.Fatalf("details = %#v", failure.Details)
		}
	})
}

// TestConversationMessageCount proves the conversation resource reports the
// whole history, not the page a read returned: the share confirmation shows
// that number before a private chat becomes readable by the project
// (decisions section 10.5).
func TestConversationMessageCount(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Counted"})
	id := created.Conversation.ID
	if created.Conversation.MessageCount != 0 {
		t.Fatalf("message_count on a fresh conversation = %d, want 0", created.Conversation.MessageCount)
	}
	requireNativeStatus(t, f.link(t, f.token, id, "link", true, "Count me"), http.StatusOK)
	// The link appends a status message; every command appends one more.
	for _, key := range []string{"c-1", "c-2", "c-3"} {
		requireNativeStatus(t, f.command(t, f.token, id, conversation.Command{Key: key, Kind: conversation.CommandMessage, Text: "Steer " + key}), http.StatusOK)
	}

	snapshot := f.snapshot(t, f.token, id)
	if got := int64(len(snapshot.Messages)); snapshot.Conversation.MessageCount != got {
		t.Fatalf("message_count = %d, want the %d stored messages", snapshot.Conversation.MessageCount, got)
	}
	if snapshot.Conversation.MessageCount != 4 {
		t.Fatalf("message_count = %d, want 4 (one link status and three commands)", snapshot.Conversation.MessageCount)
	}

	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page conversationListResponse
	decodeHubResponse(t, response, &page)
	if len(page.Conversations) != 1 || page.Conversations[0].MessageCount != 4 {
		t.Fatalf("listed conversations = %#v, want one with message_count 4", page.Conversations)
	}
}

// TestConversationCreateIsIdempotent covers the top-level key creation
// requires: a retry returns the stored response instead of a second
// conversation, a different payload under the same key conflicts, and a
// request without a key is refused (decisions section 10.2).
func TestConversationCreateIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	body := map[string]any{"key": "create-1", "title": "Idempotent", "first_message": map[string]any{"key": "first-1", "text": "Where does the renewal happen?"}}
	first := f.create(t, f.token, body)
	if first.Receipt == nil || first.Receipt.Key != "first-1" {
		t.Fatalf("receipt = %#v, want the first message key", first.Receipt)
	}

	t.Run("a retry returns the stored response", func(t *testing.T) {
		replay := f.create(t, f.token, body)
		if replay.Conversation.ID != first.Conversation.ID {
			t.Fatalf("replayed id = %q, want %q", replay.Conversation.ID, first.Conversation.ID)
		}
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversations WHERE project_id = ?", f.project.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("conversations = %d, want exactly 1", count)
		}
	})

	t.Run("a different payload conflicts", func(t *testing.T) {
		changed := map[string]any{"key": "create-1", "title": "Something else"}
		requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, changed), http.StatusConflict, "idempotency_conflict")
	})

	t.Run("another actor may reuse the key", func(t *testing.T) {
		other := f.create(t, f.other, body)
		if other.Conversation.ID == first.Conversation.ID {
			t.Fatal("the key is scoped to the actor; another actor must get its own conversation")
		}
	})

	t.Run("a missing key is refused", func(t *testing.T) {
		requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, map[string]any{"title": "No key"}), http.StatusUnprocessableEntity, "invalid_request")
	})
}

// TestConversationContinueRequiresExpectedAttempt covers the attempt a
// continue must name: null is only honest for a conversation that never had
// an attempt, so a conversation that has or had one refuses it
// (decisions section 10.8).
func TestConversationContinueRequiresExpectedAttempt(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Continue"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Continue me"), http.StatusOK)

	t.Run("null is accepted before any attempt", func(t *testing.T) {
		requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "continue-1", Kind: conversation.CommandContinue}), http.StatusOK)
	})

	// Record an attempt the way a bind does.
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"INSERT INTO conversation_starts (attempt_id, conversation_id, created_at) VALUES (?, ?, ?)", "att_past", record.ID, testTimestamp); err != nil {
		t.Fatal(err)
	}

	t.Run("null is refused once an attempt existed", func(t *testing.T) {
		failure := requireNativeError(t, f.command(t, f.token, record.ID, conversation.Command{Key: "continue-2", Kind: conversation.CommandContinue}), http.StatusUnprocessableEntity, "invalid_request")
		if !strings.Contains(failure.Message, "expected.attempt_id") {
			t.Fatalf("message = %q, want it to name expected.attempt_id", failure.Message)
		}
	})

	t.Run("a stale attempt is a stale execution, not a validation failure", func(t *testing.T) {
		requireNativeError(t, f.command(t, f.token, record.ID, conversation.Command{Key: "continue-3", Kind: conversation.CommandContinue, Expected: conversation.Expected{AttemptID: "att_past"}}), http.StatusConflict, "stale_execution")
	})

	t.Run("null is refused while an attempt owns the conversation", func(t *testing.T) {
		owned := f.create(t, f.token, map[string]any{"title": "Owned"}).Conversation
		requireNativeStatus(t, f.link(t, f.token, owned.ID, "link-owned", true, "Owned"), http.StatusOK)
		if _, err := f.service.database.db.ExecContext(t.Context(),
			`UPDATE conversations SET execution_json = json_set(execution_json, '$.attempt_id', 'att_live', '$.status', 'completed') WHERE id = ?`, owned.ID); err != nil {
			t.Fatal(err)
		}
		requireNativeError(t, f.command(t, f.token, owned.ID, conversation.Command{Key: "continue-4", Kind: conversation.CommandContinue}), http.StatusUnprocessableEntity, "invalid_request")
	})
}

// TestConversationListFiltersInSQL covers the filters the listing pushes
// into the query rather than applying in Go: the title needle is a literal
// substring with its wildcards escaped, and the organization listing is
// restricted to the projects the actor can read.
func TestConversationListFiltersInSQL(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	for _, title := range []string{"Renewal under load", "100% of the lease", "renewal x lease", "unrelated"} {
		f.create(t, f.token, map[string]any{"title": title})
	}

	list := func(t *testing.T, query string) []string {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations"+query, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page conversationListResponse
		decodeHubResponse(t, response, &page)
		titles := make([]string, 0, len(page.Conversations))
		for _, record := range page.Conversations {
			titles = append(titles, record.Title)
		}
		return titles
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "case insensitive substring", query: "?q=renewal", want: 2},
		{name: "percent is a literal", query: "?q=100%25", want: 1},
		{name: "underscore is a literal", query: "?q=renewal_under", want: 0},
		{name: "no match", query: "?q=nothing-here", want: 0},
		{name: "no filter", query: "", want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := list(t, test.query); len(got) != test.want {
				t.Fatalf("titles = %v, want %d", got, test.want)
			}
		})
	}

	t.Run("the organization listing is limited to readable projects", func(t *testing.T) {
		other := newNativeFixture(t, f.service, f.project.OrganizationID, "chat-elsewhere")
		record := conversationRecord{
			ID: conversation.NewConversationID(), OrganizationID: other.project.OrganizationID, ProjectID: other.project.ID,
			OwnerPrincipalID: f.ownerID, Title: "Elsewhere", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
			Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: testTime(t)},
			CreatedAt: testTime(t), UpdatedAt: testTime(t),
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.conversations.store.createConversation(t.Context(), tx, &record); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/conversations", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page conversationListResponse
		decodeHubResponse(t, response, &page)
		for _, listed := range page.Conversations {
			if listed.ProjectID == other.project.ID {
				t.Fatalf("listed a conversation in a project without a grant: %#v", listed)
			}
		}
	})
}

// testTime is the fixture clock, used where a record is written directly.
func testTime(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
}

// Messages saved before the link, when no coordinator answered them, are
// queued for the linked issue's first attempt and handed to it on bind.
func TestConversationLinkQueuesSavedMessages(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"first_message": map[string]any{"key": "first", "text": "Fix the renewal"}})
	id := created.Conversation.ID
	if created.Receipt == nil || created.Receipt.Status != conversation.DeliverySaved {
		t.Fatalf("receipt = %#v, want saved", created.Receipt)
	}
	requireNativeStatus(t, f.link(t, f.token, id, "link", true, "Fix the renewal"), http.StatusOK)
	snapshot := f.snapshot(t, f.token, id)
	var first *conversationMessageResource
	for i := range snapshot.Messages {
		if snapshot.Messages[i].ID == created.Receipt.MessageID {
			first = &snapshot.Messages[i]
		}
	}
	if first == nil || first.Delivery != conversation.DeliveryQueued {
		t.Fatalf("first message after link = %#v, want queued", first)
	}
}
