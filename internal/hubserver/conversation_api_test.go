package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
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

type conversationDispatchRequestFunc func(context.Context, tenantstore.Writer, tracker.ProjectID, string, leasestore.Fence, time.Time) error

func (f conversationDispatchRequestFunc) RequestDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return f(ctx, writer, project, item, fence, at)
}

func newConversationAPIFixture(t *testing.T, cfg *ConversationConfig, clocks ...func() time.Time) conversationAPIFixture {
	t.Helper()
	if cfg == nil {
		cfg = &ConversationConfig{Enabled: true}
	}
	if cfg.ArchiveContents == nil {
		cfg.ArchiveContents = &conversationContentFixture{}
	}
	config := Config{Conversation: cfg}
	if len(clocks) > 0 {
		config.now = clocks[0]
	}
	f := newDefaultNativeFixture(t, config)
	f.service.registerConversationRoutes(f.service.echo)
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	tokens := []tokenResponse{{ID: newNativeID("credential"), Token: newNativeID("token")}, {ID: newNativeID("credential"), Token: newNativeID("token")}}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for _, token := range tokens {
			hash := apikey.HashToken(token.Token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: token.ID, Name: "Example conversation operator " + token.ID, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: string(apiScopeOperator), NativeOnly: true, Kind: "personal", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: token.ID, ProjectID: f.project.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens {
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(token.Token), "", "")
		if err != nil || credential.ID != token.ID {
			t.Fatalf("conversation credential status=%d id=%s error=%v", status, credential.ID, err)
		}
	}
	f.token = tokens[0].Token
	return conversationAPIFixture{nativeFixture: f, ownerID: tokens[0].ID, other: tokens[1].Token, otherID: tokens[1].ID}
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
// requires unless the test names one itself. Every
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

func (f conversationAPIFixture) setExecution(t *testing.T, id string, execution conversation.Execution) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		record, err := writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		record.Execution, record.UpdatedAt = execution, f.service.config.now()
		_, err = writer.UpdateConversation(t.Context(), record, record.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}

}

// settle runs the settle sweep against one conversation with a window that
// is already over, so the test does not have to wait a day. The sweep only
// settles a finished or idle conversation, so an execution still waiting for
// a runner is finished first.
func (f conversationAPIFixture) settle(t *testing.T, id string) {
	t.Helper()
	var record conversationstore.Conversation
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		var err error
		record, err = writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		if !record.Execution.Status.Terminal() && record.Execution.Status != conversation.ExecutionIdle {
			record.Execution.Status = conversation.ExecutionCompleted
			record, err = writer.UpdateConversation(t.Context(), record, record.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	settled, err := f.service.conversations.settleIdleConversationPG(t.Context(), f.project.OrganizationID, record, f.service.config.now().Add(time.Hour), f.service.config.now().Add(time.Hour))
	if err != nil || !settled {
		t.Fatalf("settle conversation: settled=%t err=%v", settled, err)
	}

}

func (f conversationAPIFixture) link(t *testing.T, token, id, key string, share bool, title string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+id+"/link", token, map[string]any{
		"key": key, "share_history": share, "issue": map[string]any{"title": title, "description": "From the chat" + issueContractTestSections},
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
	linkedResponse := f.link(t, f.token, shared.ID, "link-shared", true, "Release checklist")
	requireNativeStatus(t, linkedResponse, http.StatusOK)
	var linked conversationLinkResponse
	decodeHubResponse(t, linkedResponse, &linked)
	shared = f.snapshot(t, f.token, shared.ID).Conversation
	issue := f.nativeFixture.create(t, "Runner session")
	now := f.service.config.now()
	worker := conversationRecord{ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, OwnerPrincipalID: f.ownerID, Origin: conversationOriginWorker, Title: "Runner session", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(issue.WorkItemID), LinkedAt: &now, Revision: 1, Preferences: conversation.DefaultPreferences(), Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateConversation(t.Context(), conversationRecordToStore(worker))
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
		{name: "project excludes worker search", token: f.token, path: f.base + "/conversations?q=Runner", want: nil},
		{name: "organization excludes worker search", token: f.token, path: organizationPath + "?q=Runner", want: nil},
	}
	for _, record := range []struct {
		name   string
		id     string
		item   string
		origin string
	}{
		{"worker", worker.ID, worker.WorkItemID, conversationOriginWorker},
		{"linked user", shared.ID, linked.Issue.ID, conversationOriginUser},
	} {
		t.Run(record.name+" issue conversation remains accessible", func(t *testing.T) {
			for _, token := range []string{f.token, f.other} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+record.item+"/conversation", token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot conversationSnapshotResponse
				decodeHubResponse(t, response, &snapshot)
				if snapshot.Conversation.ID != record.id {
					t.Fatalf("issue conversation = %q, want %q", snapshot.Conversation.ID, record.id)
				}
				if snapshot.Conversation.Origin != record.origin {
					t.Fatalf("issue conversation origin = %q, want %q", snapshot.Conversation.Origin, record.origin)
				}
			}
		})
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
	// first, and the settled filter selects one group.
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
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
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
	f.settle(t, record.ID)
	linked := f.link(t, f.token, record.ID, "archive-shared", true, "Archived chat")
	requireNativeStatus(t, linked, http.StatusOK)
	paths := []string{"", "/messages?before=4&limit=2", "/messages?before=2", "/messages?limit=2"}
	hot := make([][]byte, len(paths))
	for i, path := range paths {
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+path, f.other, nil)
		requireNativeStatus(t, response, http.StatusOK)
		hot[i] = append([]byte(nil), response.Body.Bytes()...)
	}

	var stored []conversationstore.Message
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		messages, err := r.Messages(t.Context(), record.ID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		stored = messages.Items
		for _, message := range stored {
			if message.Text != "" || message.ContentReference == "" {
				t.Fatalf("message payload has no object owner: %+v", message)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	messageCount := len(stored)
	fetches := 0
	objects.onIO = func() error { fetches++; return nil }
	for i, path := range paths {
		fetches = 0
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+path, f.other, nil)
		requireNativeStatus(t, response, http.StatusOK)
		assertArchiveGolden(t, hot[i], response.Body.Bytes())
		want := []int{messageCount, 2, 1, 2}[i]
		if fetches != want {
			t.Fatalf("page %s object reads=%d want=%d", path, fetches, want)
		}
	}
	allObjects := objects.content
	keys := map[int64]string{}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		for _, message := range stored {
			content, err := r.ConversationContent(t.Context(), message.ContentReference)
			if err != nil {
				return err
			}
			keys[message.Sequence] = content.Reference.Key
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []struct {
		before      string
		first, last int64
		want        int
		next        string
	}{
		{"", 4, int64(messageCount), messageCount - 3, "4"}, {"4", 1, 3, 3, ""}, {"1", 0, 0, 0, ""},
	} {
		objects.content = map[string][]byte{}
		for seq, key := range keys {
			if seq >= part.first && seq <= part.last && part.want > 0 {
				objects.content[key] = allObjects[key]
			}
		}
		fetches = 0
		query := "?limit=3"
		if part.before != "" {
			query += "&before=" + part.before
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages"+query, f.other, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &page)
		if len(page.Messages) != part.want {
			t.Fatalf("range page before=%s got=%d want=%d", part.before, len(page.Messages), part.want)
		}
		if (page.NextCursor == nil) != (part.next == "") || page.NextCursor != nil && *page.NextCursor != part.next {
			t.Fatalf("range cursor=%v want %s", page.NextCursor, part.next)
		}
		for _, message := range page.Messages {
			if message.Role == "user" && message.Text == "" {
				t.Fatal("message stub escaped the object reader")
			}
		}
		if fetches != part.want {
			t.Fatalf("range object reads=%d want %d", fetches, part.want)
		}
	}
	objects.content = allObjects
	missingKey := keys[1]
	missing := objects.content[missingKey]
	delete(objects.content, missingKey)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=2", f.other, nil)
	requireNativeStatus(t, response, http.StatusInternalServerError)
	objects.content[missingKey] = missing
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=2", f.other, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	if len(page.Messages) != 1 || page.Messages[0].Text != "message a" {
		t.Fatalf("restored object page=%+v", page.Messages)
	}
	fetches = 0
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages?before=x", f.token, nil)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if fetches != 0 {
		t.Fatalf("invalid cursor read %d objects", fetches)
	}
}

func TestConversationAPICommands(t *testing.T) {
	t.Parallel()
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
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
	t.Run("sparse recovery advances empty examined pages", func(t *testing.T) {
		fixture := newCoordinatorFixture(t, "sparse_pending_scan")
		id := func(number int) string { return fmt.Sprintf("conv_%032x", number) }
		record := fixture.seed(t, "Pending cancellation", func(record *conversationRecord) {
			record.ID = id(2*conversationLifecycleBatchSize + 1)
		})
		fixture.history(t, &record, conversation.RoleAssistant, "Interrupted earlier turn", conversation.DeliveryInterrupted)
		target, ok := lastReply(fixture.messages(t, record.ID))
		if !ok {
			t.Fatal("pending cancellation target is missing")
		}
		now := fixture.service.config.now()
		if err := fixture.service.config.TenantRepository.WriteTenant(t.Context(), fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
			for number := 1; number <= 2*conversationLifecycleBatchSize; number++ {
				if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: fixture.organization, ProjectID: fixture.project.ID, ID: id(number), OwnerPrincipalID: fixture.owner, Origin: conversationOriginUser, Title: "Inactive conversation", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			_, _, err := w.ReserveCommand(t.Context(), conversationstore.Command{OrganizationID: fixture.organization, ConversationID: record.ID, Key: "sparse_cancel", Kind: conversation.CommandCancel, TargetTurnID: target.ID, RequestHash: "sparse_cancel", Receipt: conversation.Receipt{Key: "sparse_cancel", Kind: conversation.CommandCancel, Status: conversation.DeliverySaved, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for page := 1; page <= 3; page++ {
			if err := fixture.conversations.wakePendingPG(t.Context(), fixture.organization); err != nil {
				t.Fatal(err)
			}
			wantBoundary, wantStatus := "", conversation.DeliveryDelivered
			if page < 3 {
				wantBoundary, wantStatus = id(page*conversationLifecycleBatchSize), conversation.DeliverySaved
			}
			if got := fixture.conversations.lifecycleCursor("pending"); got != wantBoundary {
				t.Fatalf("page %d examined boundary=%q want=%q", page, got, wantBoundary)
			}
			if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
				command, found, err := r.Command(t.Context(), record.ID, "sparse_cancel")
				if err == nil && (!found || command.Receipt.Status != wantStatus || command.TargetTurnID != target.ID) {
					t.Fatalf("page %d cancellation=%+v want=%s target=%s", page, command, wantStatus, target.ID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
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
		for _, test := range []struct {
			command conversation.Command
			status  int
			code    string
		}{
			{conversation.Command{Key: "cont", Kind: conversation.CommandContinue}, http.StatusUnprocessableEntity, "unsupported_control"},
			{conversation.Command{Key: "intr", Kind: conversation.CommandInterrupt, Expected: conversation.Expected{AttemptID: "att_x"}}, http.StatusConflict, "stale_execution"},
			{conversation.Command{Key: "ans", Kind: conversation.CommandAnswer, QuestionID: "q_missing", Answers: map[string][]string{"p": {"a"}}}, http.StatusNotFound, "not_found"},
			{conversation.Command{Key: "retry", Kind: conversation.CommandRetry, MessageID: conversation.NewMessageID()}, http.StatusUnprocessableEntity, "invalid_request"},
		} {
			before := objects.publications
			requireNativeError(t, f.command(t, f.token, record.ID, test.command), test.status, test.code)
			if objects.publications != before {
				t.Errorf("rejected %s published %d objects", test.command.Kind, objects.publications-before)
			}
		}
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "cancel", Kind: conversation.CommandCancel})
		requireNativeStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, response, &receipt)
		if receipt.Status != conversation.DeliveryRejected || receipt.Error == nil || receipt.Error.Code != "no_active_turn" {
			t.Fatalf("cancel receipt = %#v", receipt)
		}
	})
	for _, stage := range []string{"intent", "completion"} {
		t.Run("cancel "+stage+" persistence and successor replay", func(t *testing.T) {
			fixture := newCoordinatorFixture(t, "api_cancel_"+stage)
			api := conversationAPIFixture{nativeFixture: fixture.nativeFixture, ownerID: fixture.owner}
			contexts := make(chan context.Context, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			fixture.backend.setRun(func(ctx context.Context, _ int, _ runner.AgentToolHandler, _ runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
				contexts <- ctx
				select {
				case <-ctx.Done():
					return runner.AgentTurnResult{}, ctx.Err()
				case <-release:
					return runner.AgentTurnResult{}, nil
				}
			})
			record := fixture.seed(t, "Cancellation persistence", nil)
			fixture.say(t, &record, "First turn")
			nextContext := func() context.Context {
				select {
				case ctx := <-contexts:
					return ctx
				case <-testenv.WaitContext(t, t.Context()).Done():
					t.Fatal("coordinator turn did not start")
					return nil
				}
			}
			first := nextContext()
			coordinator := fixture.coordinator().(*conversationTurnCoordinator)
			coordinator.mu.Lock()
			originalTurn := coordinator.turns[record.ID]
			coordinator.mu.Unlock()
			if originalTurn == nil {
				t.Fatal("active coordinator turn is missing")
			}
			assistant, ok := lastReply(fixture.messages(t, record.ID))
			if !ok {
				t.Fatal("active coordinator assistant is missing")
			}
			original := fixture.service.config.TenantRepository
			repository := &conversationCancelFailureRepository{Repository: original}
			repository.intent.Store(stage == "intent")
			repository.completion.Store(stage == "completion")
			fixture.service.config.TenantRepository = repository
			t.Cleanup(func() { fixture.service.config.TenantRepository = original })
			stale := conversation.Command{Key: "cancel_wrong_turn", Kind: conversation.CommandCancel, Expected: conversation.Expected{TurnID: "turn_other"}}
			requireNativeError(t, api.command(t, api.token, record.ID, stale), http.StatusConflict, "stale_execution")
			select {
			case <-first.Done():
				t.Fatal("stale expected turn cancelled the active turn")
			default:
			}
			if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
				_, found, err := r.Command(t.Context(), record.ID, stale.Key)
				if found {
					t.Fatal("stale expected turn persisted a cancellation")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			command := conversation.Command{Key: "cancel_original", Kind: conversation.CommandCancel, Expected: conversation.Expected{TurnID: assistant.ID}}
			response := api.command(t, api.token, record.ID, command)
			if stage == "intent" {
				requireNativeStatus(t, response, http.StatusInternalServerError)
				if repository.intentFailures.Load() == 0 {
					t.Fatal("intent persistence failure was not injected")
				}
				select {
				case <-first.Done():
					t.Fatal("rolled-back cancel stopped the active turn without a receipt")
				default:
				}
				if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
					_, found, err := r.Command(t.Context(), record.ID, command.Key)
					if found {
						t.Fatal("rolled-back cancel retained a receipt")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				repository.intent.Store(false)
				response = api.command(t, api.token, record.ID, command)
			}
			requireNativeStatus(t, response, http.StatusOK)
			var accepted conversation.Receipt
			decodeHubResponse(t, response, &accepted)
			if accepted.Status != conversation.DeliverySaved && accepted.Status != conversation.DeliveryDelivered {
				t.Fatalf("cancel has no durable delivery state: %+v", accepted)
			}
			if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, found, err := r.Command(t.Context(), record.ID, command.Key)
				if err == nil && (!found || stored.TargetTurnID != assistant.ID || !stored.Receipt.Status.Valid()) {
					t.Fatalf("committed cancellation intent is invalid: %+v", stored)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}

			select {
			case <-first.Done():
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("committed cancel did not stop its intended turn")
			}
			fixture.waitAssistant(t, record.ID, conversation.DeliveryInterrupted)
			waitUntil(t, "cancelled coordinator turn settled", func() bool { return fixture.conversation(t, record.ID).Execution.Status == conversation.ExecutionIdle })
			if stage == "completion" {
				if repository.completionFailures.Load() == 0 {
					t.Fatal("completion persistence failure was not injected")
				}
				if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
					stored, found, err := r.Command(t.Context(), record.ID, command.Key)
					if err == nil && (!found || stored.Receipt.Status != conversation.DeliverySaved) {
						t.Fatalf("committed cancel intent was lost: found=%t receipt=%+v", found, stored.Receipt)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, found, err := r.Command(t.Context(), record.ID, command.Key)
				if err != nil {
					return err
				}
				if !found || stored.TargetTurnID != assistant.ID {
					t.Fatalf("cancel target was not retained: %+v", stored)
				}
				if stage == "completion" {
					commands, err := r.PendingCoordinatorCommands(t.Context(), record.ID, "", corestore.MaxPageSize)
					if err != nil {
						return err
					}
					if len(commands.Items) != 1 || commands.Items[0].Key != command.Key {
						t.Fatalf("pending coordinator command=%+v", commands)
					}
					pending, err := r.PendingConversations(t.Context(), "", corestore.MaxPageSize)
					if err != nil {
						return err
					}
					found := false
					for _, entry := range pending.Items {
						if entry.ID == record.ID && !entry.Linked {
							found = true
						}
					}
					if !found {
						t.Fatal("pending cancellation is absent from existing recovery wake scan")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-originalTurn.done:
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("original coordinator turn did not release its ownership")
			}

			fixture.say(t, &record, "Successor turn")
			var successor context.Context
			waitUntil(t, "successor coordinator turn started", func() bool {
				select {
				case successor = <-contexts:
					return true
				default:
				}
				if reply, ok := lastReply(fixture.messages(t, record.ID)); ok && reply.ID != assistant.ID && (reply.Delivery == conversation.DeliveryInterrupted || reply.Delivery == conversation.DeliveryFailed || reply.Delivery == conversation.DeliveryCompleted) {
					t.Fatalf("pending cancellation stopped successor before provider start: %+v", reply)
				}
				return false
			})
			repository.completion.Store(false)
			replay := api.command(t, api.token, record.ID, command)
			requireNativeStatus(t, replay, http.StatusOK)
			var receipt conversation.Receipt
			decodeHubResponse(t, replay, &receipt)
			if receipt.Status != conversation.DeliveryDelivered {
				t.Fatalf("cancel completion is not replayable: %+v", receipt)
			}
			select {
			case <-successor.Done():
				t.Fatal("replayed cancel stopped the successor turn")
			default:
			}
			repeated := api.command(t, api.token, record.ID, command)
			requireNativeStatus(t, repeated, http.StatusOK)
			if repeated.Body.String() != replay.Body.String() {
				t.Fatalf("completed replay changed: %s != %s", repeated.Body.String(), replay.Body.String())
			}
			select {
			case <-successor.Done():
				t.Fatal("completed cancel replay stopped the successor turn")
			default:
			}
		})
	}

	t.Run("cancel recovery scan context budget", func(t *testing.T) {
		objects := &conversationCancelBlockingContents{conversationContentFixture: &conversationContentFixture{}, entered: make(chan context.Context, 2), release: make(chan struct{})}
		fixture := newConfiguredCoordinatorFixture(t, "cancel_scan_budget", func(cfg *Config) { cfg.Conversation.ArchiveContents = objects })
		ids := []string{}
		for _, title := range []string{"First pending cancel", "Second pending cancel"} {
			record := fixture.seed(t, title, nil)
			fixture.history(t, &record, conversation.RoleAssistant, "Interrupted earlier turn", conversation.DeliveryInterrupted)
			target, ok := lastReply(fixture.messages(t, record.ID))
			if !ok {
				t.Fatal("scan fixture target assistant is missing")
			}

			ids = append(ids, record.ID)
			now := fixture.service.config.now()
			if err := fixture.service.config.TenantRepository.WriteTenant(t.Context(), fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, _, err := w.ReserveCommand(t.Context(), conversationstore.Command{OrganizationID: fixture.organization, ConversationID: record.ID, Key: "scan_cancel", Kind: conversation.CommandCancel, TargetTurnID: target.ID, RequestHash: "scan_" + record.ID, Receipt: conversation.Receipt{Key: "scan_cancel", Kind: conversation.CommandCancel, Status: conversation.DeliverySaved, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		organization, err := fixture.conversations.lifecycleOrganization()
		if err != nil || organization != fixture.organization {
			t.Fatalf("lifecycle organization=%s expected=%s error=%v", organization, fixture.organization, err)
		}

		if !fixture.coordinator().Available() {
			t.Fatal("coordinator delivery is unavailable in scan fixture")
		}
		if fixture.conversations.store.contents != objects {
			t.Fatal("scan fixture storage is not installed")
		}
		if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.PendingConversations(t.Context(), "", corestore.MaxPageSize)
			if err == nil && len(page.Items) != len(ids) {
				t.Fatalf("scan fixture pending rows=%+v", page)
			}
			for _, entry := range page.Items {
				if entry.Linked {
					t.Fatalf("pending cancellation classified as linked: %+v", entry)
				}
			}
			for _, id := range ids {
				commands, readErr := r.PendingCoordinatorCommands(t.Context(), id, "", corestore.MaxPageSize)
				if readErr != nil {
					return readErr
				}
				if len(commands.Items) != 1 || commands.Items[0].Receipt.Status != conversation.DeliverySaved {
					t.Fatalf("pending commands for %s: %+v", id, commands)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		original := fixture.service.config.TenantRepository
		repository := &conversationCancelFailureRepository{Repository: original, scanContexts: make(chan context.Context, 1)}
		fixture.service.config.TenantRepository = repository
		objects.block.Store(true)
		parent, cancel := context.WithCancel(context.WithValue(t.Context(), conversationCancelScanContextKey{}, true))
		done := make(chan struct{})
		var scanErr error
		go func() { scanErr = fixture.conversations.wakePendingPG(parent, fixture.organization); close(done) }()
		t.Cleanup(func() { cancel(); close(objects.release); <-done; fixture.service.config.TenantRepository = original })
		var storage, scan context.Context
		select {
		case storage = <-objects.entered:
		case <-done:
			t.Fatalf("scan exited before reaching storage: %v", scanErr)
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("pending cancellation did not reach storage")
		}
		select {
		case scan = <-repository.scanContexts:
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("lifecycle scan context was not captured")
		}
		cancel()
		select {
		case <-storage.Done():
		default:
			t.Fatal("blocked delivery detached from lifecycle cancellation")
		}
		scanDeadline, scanBound := scan.Deadline()
		storageDeadline, storageBound := storage.Deadline()
		if !scanBound || !storageBound || !storageDeadline.Equal(scanDeadline) {
			t.Fatalf("storage budget=%s scan budget=%s", storageDeadline, scanDeadline)
		}
		select {
		case <-done:
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("cancelled lifecycle scan did not terminate")
		}
		if !errors.Is(scanErr, context.Canceled) {
			t.Fatalf("scan cancellation error=%v", scanErr)
		}
		if objects.calls.Load() != 1 {
			t.Fatalf("cancelled scan visited %d storage operations, want 1", objects.calls.Load())
		}
		if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, id := range ids {
				command, found, err := r.Command(t.Context(), id, "scan_cancel")
				if err != nil {
					return err
				}
				if !found || command.Receipt.Status != conversation.DeliverySaved {
					t.Fatalf("cancelled scan consumed intent: id=%s command=%+v", id, command)
				}
			}
			pending, err := r.PendingConversations(t.Context(), "", corestore.MaxPageSize)
			if err != nil {
				return err
			}
			if len(pending.Items) != len(ids) {
				t.Fatalf("recoverable pending conversations=%+v", pending)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	for _, kind := range []string{"ordinary", "child deadline"} {
		t.Run("cancel recovery scan continues after "+kind, func(t *testing.T) {
			fixture := newCoordinatorFixture(t, "scan_continuation")
			issue := fixture.create(t, "Linked controls")
			ids := []string{}
			for index := 1; index <= 4; index++ {
				record := fixture.seed(t, "Pending scan work", func(record *conversationRecord) {
					record.ID = fmt.Sprintf("conv_%032x", index)
					if index == 3 {
						record.WorkItemID = string(issue.WorkItemID)
						linkedAt := fixture.service.config.now()
						record.LinkedAt = &linkedAt
						record.Visibility = conversation.VisibilityShared
					}
				})
				ids = append(ids, record.ID)
				if index == 3 {
					fixture.history(t, &record, conversation.RoleUser, "Queued linked work", conversation.DeliverySaved)
					continue
				}
				fixture.history(t, &record, conversation.RoleAssistant, "Interrupted earlier turn", conversation.DeliveryInterrupted)
				target, ok := lastReply(fixture.messages(t, record.ID))
				if !ok {
					t.Fatal("pending cancellation target is missing")
				}
				now := fixture.service.config.now()
				if err := fixture.service.config.TenantRepository.WriteTenant(t.Context(), fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, _, err := w.ReserveCommand(t.Context(), conversationstore.Command{OrganizationID: fixture.organization, ConversationID: record.ID, Key: "scan_continue", Kind: conversation.CommandCancel, TargetTurnID: target.ID, RequestHash: "scan_" + record.ID, Receipt: conversation.Receipt{Key: "scan_continue", Kind: conversation.CommandCancel, Status: conversation.DeliverySaved, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			original := fixture.service.config.TenantRepository
			var firstPage conversationstore.PendingConversationPage
			if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				firstPage, err = r.PendingConversations(t.Context(), "", 3)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(firstPage.Items) != 3 || firstPage.Items[0].ID != ids[0] || firstPage.Items[2].ID != ids[2] || !firstPage.Items[2].Linked || firstPage.Next != ids[2] {
				t.Fatalf("scan prerequisites=%+v", firstPage)
			}
			failure := errors.New("injected per-conversation delivery failure")
			if kind == "child deadline" {
				failure = fmt.Errorf("storage child deadline: %w", context.DeadlineExceeded)
			}
			repository := &conversationCancelFailureRepository{Repository: original, pendingLimit: 3, completionConversation: ids[0], completionError: failure}
			repository.completion.Store(true)
			fixture.service.config.TenantRepository = repository
			controls := &conversationScanControlSignals{conversationControlRouter: fixture.conversations.controls, wakes: make(chan string, 4)}
			fixture.conversations.controls = controls
			t.Cleanup(func() { fixture.service.config.TenantRepository = original })
			ctx := context.WithValue(t.Context(), conversationCancelScanContextKey{}, true)
			err := fixture.conversations.wakePendingPG(ctx, fixture.organization)
			if !errors.Is(err, failure) || ctx.Err() != nil {
				t.Fatalf("independent delivery error=%v scan context=%v", err, ctx.Err())
			}
			requireReceipt := func(id string, status conversation.Delivery) {
				if err := original.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
					command, found, err := r.Command(t.Context(), id, "scan_continue")
					if err == nil && (!found || command.Receipt.Status != status) {
						t.Fatalf("conversation %s receipt=%+v want=%s", id, command, status)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			requireReceipt(ids[0], conversation.DeliverySaved)
			requireReceipt(ids[1], conversation.DeliveryDelivered)
			select {
			case id := <-controls.wakes:
				if id != ids[2] {
					t.Fatalf("linked control wake=%s", id)
				}
			default:
				t.Fatal("earlier failure starved later linked control work")
			}
			if got := fixture.conversations.lifecycleCursor("pending"); got != firstPage.Next {
				t.Fatalf("traversed-page cursor=%q want=%q", got, firstPage.Next)
			}
			if err := fixture.conversations.wakePendingPG(ctx, fixture.organization); err != nil {
				t.Fatalf("later page did not progress: %v", err)
			}
			requireReceipt(ids[3], conversation.DeliveryDelivered)
			if got := fixture.conversations.lifecycleCursor("pending"); got != "" {
				t.Fatalf("cursor did not wrap: %q", got)
			}
			if err := fixture.conversations.wakePendingPG(ctx, fixture.organization); !errors.Is(err, failure) {
				t.Fatalf("wrapped failed intent=%v", err)
			}
			requireReceipt(ids[0], conversation.DeliverySaved)
			if repository.completionFailures.Load() != 2 {
				t.Fatalf("failed intent attempts=%d want=2 across wrap", repository.completionFailures.Load())
			}
		})
	}

	t.Run("other principal cannot see private", func(t *testing.T) {
		response := f.command(t, f.other, record.ID, message)
		requireNativeError(t, response, http.StatusNotFound, "not_found")
	})
	// A settled conversation is not read-only: the command is accepted and
	// the conversation goes back to active.
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
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
	record := f.create(t, f.token, map[string]any{"title": "Questions"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Ask me"), http.StatusOK)
	plan, err := f.service.conversations.newPublicationPlan(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	execution := conversation.Execution{Status: conversation.ExecutionWaitingInput, Owner: conversation.Owner{AttemptID: "att_1", RunID: "run_1", TurnID: "turn_1"}, Capabilities: conversation.Capabilities{Answer: true}}
	if err := plan.updateExecution(t.Context(), execution); err != nil {
		t.Fatal(err)
	}
	question := conversationQuestionRecord{Question: conversation.Question{
		ID: conversation.NewQuestionID(), ConversationID: record.ID, Status: conversation.QuestionPending,
		Owner: execution.Owner, Prompts: []conversation.Prompt{{ID: "color", Question: "Which color?", Options: []conversation.Option{{Label: "red"}, {Label: "blue"}}}},
		CreatedAt: plan.now, UpdatedAt: plan.now,
	}}
	if err := plan.prepareQuestion(t.Context(), question); err != nil {
		t.Fatal(err)
	}
	ctx := plan.context(t.Context())
	if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.project.OrganizationID, plan.prepared, plan.now); err != nil {
			return err
		}
		linked, err := w.Conversation(ctx, f.project.ID, record.ID)
		if err != nil {
			return err
		}
		linked.Execution, linked.UpdatedAt = execution, plan.now
		if _, err := w.UpdateConversation(ctx, linked, linked.Revision); err != nil {
			return err
		}
		return w.UpsertQuestion(ctx, conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Question: question.Question})
	}); err != nil {
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
			before := objects.publications
			response := f.command(t, f.token, record.ID, tt.command)
			if tt.code != "" {
				requireNativeError(t, response, tt.status, tt.code)
				if objects.publications != before {
					t.Errorf("rejected answer published %d objects", objects.publications-before)
				}
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
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 3, ArchiveContents: objects})
	dispatches := 0
	f.service.config.ConversationDispatch = conversationDispatchRequestFunc(func(_ context.Context, _ tenantstore.Writer, project tracker.ProjectID, item string, _ leasestore.Fence, _ time.Time) error {
		if project != f.project.ID || item == "" {
			t.Fatalf("dispatch scope: project=%s item=%s", project, item)
		}
		dispatches++
		return nil
	})

	record := f.create(t, f.token, map[string]any{"title": "Ship it", "first_message": map[string]any{"key": "first", "text": "Please ship the release"}}).Conversation
	t.Run("requires share_history", func(t *testing.T) {
		response := f.link(t, f.token, record.ID, "no-share", false, "Ship the release")
		requireNativeError(t, response, http.StatusUnprocessableEntity, "share_history_required")
		before := objects.publications
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/link", f.token, map[string]any{"key": "no-title", "share_history": true, "issue": map[string]any{"title": " "}})
		requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
		if objects.publications != before {
			t.Fatalf("rejected handoff published %d objects; want 0", objects.publications-before)
		}
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
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_audience_events WHERE organization_id=$1 AND conversation_id=$2 AND from_visibility='private' AND to_visibility='shared' AND actor_principal_id=$3", f.project.OrganizationID, record.ID, f.ownerID).Scan(&audience); err != nil {
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
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2",
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
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			_, found, err := r.Command(t.Context(), record.ID, "q3")
			if err == nil && found {
				t.Fatal("queue_full must not persist the command")
			}
			return err
		}); err != nil {
			t.Fatal(err)
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
		replay := f.command(t, f.token, record.ID, conversation.Command{Key: "cont", Kind: conversation.CommandContinue})
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != response.Body.String() || dispatches != 1 {
			t.Fatalf("continue replay=%s dispatches=%d", replay.Body.String(), dispatches)
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
				"key": key, "share_history": true, "issue": map[string]any{"title": "Ranked", "description": "From the chat" + issueContractTestSections, "priority": priority},
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
// that number before a private chat becomes readable by the project.
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
// request without a key is refused.
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
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.Conversations(t.Context(), conversationstore.ConversationQuery{ProjectID: f.project.ID, PrincipalID: f.ownerID, Limit: 2})
			if err != nil {
				return err
			}
			if len(page.Items) != 1 || page.Next != "" || page.Items[0].ID != first.Conversation.ID {
				t.Fatalf("conversations=%+v, want exactly the original conversation", page)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
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

func TestConversationPostgresAPIUsesTenantRecords(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	repository := f.service.config.TenantRepository

	contents := f.service.conversations.store.contents.(*conversationContentFixture)
	contents.setFail(true)
	failed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, map[string]any{"key": "pg-failed-create", "title": "Example"})
	if failed.Code < http.StatusInternalServerError {
		t.Fatalf("object publication failure status = %d, want server error", failed.Code)
	}
	contents.setFail(false)

	body := map[string]any{"key": "pg-create", "title": "Example", "first_message": map[string]any{"key": "pg-first", "text": "hello from PostgreSQL"}}
	apiCredentialValue, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.commandCreateConversation(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredentialValue}, conversationCreateRequest{Key: "pg-create", Title: "Example", FirstMessage: &conversationFirstMessage{Key: "pg-first", Text: "hello from PostgreSQL"}}); err != nil {
		t.Fatalf("commandCreateConversation() error = %v", err)
	}
	created := f.create(t, f.token, body)
	if created.Receipt == nil || created.Receipt.Key != "pg-first" {
		t.Fatalf("create receipt = %+v", created.Receipt)
	}
	replay := f.create(t, f.token, body)
	if replay.Conversation.ID != created.Conversation.ID {
		t.Fatalf("create replay id = %q, want %q", replay.Conversation.ID, created.Conversation.ID)
	}
	second := f.command(t, f.token, created.Conversation.ID, conversation.Command{Key: "pg-second", Kind: conversation.CommandMessage, Text: "another message"})
	requireNativeStatus(t, second, http.StatusOK)
	commandReplay := f.command(t, f.token, created.Conversation.ID, conversation.Command{Key: "pg-second", Kind: conversation.CommandMessage, Text: "another message"})
	requireNativeStatus(t, commandReplay, http.StatusOK)
	conflict := f.command(t, f.token, created.Conversation.ID, conversation.Command{Key: "pg-second", Kind: conversation.CommandMessage, Text: "different message"})
	requireNativeError(t, conflict, http.StatusConflict, "idempotency_conflict")
	snapshot := f.snapshot(t, f.token, created.Conversation.ID)
	if len(snapshot.Messages) != 2 || snapshot.Messages[0].Text != "hello from PostgreSQL" || snapshot.Messages[1].Text != "another message" {
		t.Fatalf("PostgreSQL messages = %+v", snapshot.Messages)
	}
	patched := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/conversations/"+created.Conversation.ID, f.token, map[string]any{"title": "Renamed in PostgreSQL"})
	requireNativeStatus(t, patched, http.StatusOK)
	var record conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Conversation(t.Context(), f.project.ID, created.Conversation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if record.Title != "Renamed in PostgreSQL" || record.Revision < 2 {
		t.Fatalf("patched conversation = %+v", record)
	}
	preferences := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/conversations/"+created.Conversation.ID, f.token, map[string]any{"preferences": map[string]any{"reasoning_effort": "low"}})
	requireNativeStatus(t, preferences, http.StatusOK)
	if updated := f.snapshot(t, f.token, created.Conversation.ID); updated.Conversation.Preferences.ReasoningEffort != conversation.EffortLow || updated.Conversation.Title != "Renamed in PostgreSQL" {
		t.Fatalf("persisted preference patch=%+v", updated.Conversation)
	}
	unsupported := f.command(t, f.token, created.Conversation.ID, conversation.Command{Key: "pg-unlinked-continue", Kind: conversation.CommandContinue})
	requireNativeError(t, unsupported, http.StatusUnprocessableEntity, "unsupported_control")
	linked := f.link(t, f.token, created.Conversation.ID, "pg-link", true, "Example linked issue")
	requireNativeStatus(t, linked, http.StatusOK)
	var linkResult conversationLinkResponse
	decodeHubResponse(t, linked, &linkResult)
	if linkResult.Conversation.Visibility != conversation.VisibilityShared || linkResult.Issue.ID == "" {
		t.Fatalf("PostgreSQL link result = %+v, want shared conversation and linked issue", linkResult)
	}
	replayedLink := f.link(t, f.token, created.Conversation.ID, "pg-link", true, "Example linked issue")
	requireNativeStatus(t, replayedLink, http.StatusOK)
	var replayedLinkResult conversationLinkResponse
	decodeHubResponse(t, replayedLink, &replayedLinkResult)
	if replayedLinkResult.Issue.ID != linkResult.Issue.ID || replayedLinkResult.Conversation.ID != linkResult.Conversation.ID {
		t.Fatalf("PostgreSQL link replay = %+v, want issue %q and conversation %q", replayedLinkResult, linkResult.Issue.ID, linkResult.Conversation.ID)
	}
	var linkedRecord conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		linkedRecord, err = r.Conversation(t.Context(), f.project.ID, created.Conversation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if linkedRecord.WorkItemID != linkResult.Issue.ID || linkedRecord.Visibility != conversation.VisibilityShared {
		t.Fatalf("stored PostgreSQL conversation link = %+v, want work item %q and shared visibility", linkedRecord, linkResult.Issue.ID)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.LockConversation(t.Context(), f.project.ID, created.Conversation.ID)
		if err != nil {
			return err
		}
		current.Execution = conversation.Execution{Status: conversation.ExecutionCompleted, Owner: conversation.Owner{AttemptID: "attempt-finished", RunID: "run-example", LeaseID: "lease-example", FencingToken: 7}}
		if _, err := w.UpdateConversation(t.Context(), current, current.Revision); err != nil {
			return err
		}
		return w.RecordStart(t.Context(), "attempt-finished", created.Conversation.ID, f.service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
	var beforeContinue conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		beforeContinue, err = r.Conversation(t.Context(), f.project.ID, created.Conversation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	continueCommand := conversation.Command{Key: "pg-continue", Kind: conversation.CommandContinue, Text: "Continue the example issue", Expected: conversation.Expected{AttemptID: "attempt-finished"}}
	missingExpected := f.command(t, f.token, created.Conversation.ID, conversation.Command{Key: "pg-continue-missing-attempt", Kind: conversation.CommandContinue, Text: "Continue the example issue"})
	requireNativeError(t, missingExpected, http.StatusUnprocessableEntity, "invalid_request")
	f.service.config.ConversationDispatch = nil
	refused := f.command(t, f.token, created.Conversation.ID, continueCommand)
	requireNativeError(t, refused, http.StatusServiceUnavailable, "unavailable")
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, found, err := r.Command(t.Context(), created.Conversation.ID, continueCommand.Key); err != nil || found {
			return errors.Join(err, errors.New("continue command reservation survived missing dispatch participant"))
		}
		messages, err := r.Messages(t.Context(), created.Conversation.ID, 0, 20)
		if err != nil {
			return err
		}
		if int64(len(messages.Items)) != beforeContinue.MessageCount {
			return errors.New("continue message survived missing dispatch participant")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var dispatches int
	var dispatchFences []leasestore.Fence
	f.service.config.ConversationDispatch = conversationDispatchRequestFunc(func(_ context.Context, _ tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, _ time.Time) error {
		dispatches++
		dispatchFences = append(dispatchFences, fence)
		if project != f.project.ID || item != linkResult.Issue.ID {
			t.Fatalf("dispatch target = %s/%s, want %s/%s", project, item, f.project.ID, linkResult.Issue.ID)
		}
		return nil
	})
	continued := f.command(t, f.token, created.Conversation.ID, continueCommand)
	requireNativeStatus(t, continued, http.StatusOK)
	replayedContinue := f.command(t, f.token, created.Conversation.ID, continueCommand)
	requireNativeStatus(t, replayedContinue, http.StatusOK)
	if dispatches != 1 {
		t.Fatalf("dispatch participant calls = %d, want one across replay", dispatches)
	}
	if dispatchFences[0] != (leasestore.Fence{LeaseID: "lease-example", FencingToken: 7}) {
		t.Fatalf("continue dispatch fence = %+v, want lease-example/7", dispatchFences[0])
	}
	var retryable conversationstore.Message
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		commandRecord, found, err := r.Command(t.Context(), created.Conversation.ID, "pg-second")
		if err != nil || !found {
			return errors.Join(err, errors.New("retry fixture command is missing"))
		}
		retryable, err = r.Message(t.Context(), created.Conversation.ID, commandRecord.Receipt.MessageID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		message, err := w.Message(t.Context(), created.Conversation.ID, retryable.ID)
		if err != nil {
			return err
		}
		message.Delivery, message.UpdatedAt = conversation.DeliveryFailed, f.service.config.now()
		message, err = w.UpdateMessage(t.Context(), message, message.Revision)
		if err != nil {
			return err
		}
		if err := appendPGConversationEvent(t.Context(), w, f.project.OrganizationID, created.Conversation.ID, conversation.EventMessageUpdated, projectMessage(conversationMessageFromStore(message)), message.UpdatedAt); err != nil {
			return err
		}
		commandRecord, found, err := w.Command(t.Context(), created.Conversation.ID, retryable.CommandKey)
		if err != nil || !found {
			return errors.Join(err, errors.New("retry fixture command receipt is missing"))
		}
		commandRecord.Receipt.Status, commandRecord.UpdatedAt = conversation.DeliveryFailed, message.UpdatedAt
		return w.UpdateCommand(t.Context(), commandRecord)
	}); err != nil {
		t.Fatal(err)
	}
	retryCommand := conversation.Command{Key: "pg-retry", Kind: conversation.CommandRetry, MessageID: retryable.ID}
	retried := f.command(t, f.token, created.Conversation.ID, retryCommand)
	requireNativeStatus(t, retried, http.StatusOK)
	replayedRetry := f.command(t, f.token, created.Conversation.ID, retryCommand)
	requireNativeStatus(t, replayedRetry, http.StatusOK)
	if dispatches != 2 {
		t.Fatalf("dispatch participant calls after retry replay = %d, want two total", dispatches)
	}
	if dispatchFences[1] != (leasestore.Fence{}) {
		t.Fatalf("retry dispatch fence = %+v, want no current owner fence", dispatchFences[1])
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		message, err := r.Message(t.Context(), created.Conversation.ID, retryable.ID)
		if err != nil {
			return err
		}
		if message.Delivery != conversation.DeliveryQueued {
			return fmt.Errorf("retried delivery = %q, want queued", message.Delivery)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if created.Conversation.ID == "" {
		t.Fatal("created conversation has no id")
	}
}

func TestConversationPostgresScopeUsesTenantRecords(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	repository := f.service.config.TenantRepository
	created := f.create(t, f.token, map[string]any{"title": "Scoped conversation"}).Conversation
	linked := f.link(t, f.token, created.ID, "scope-link", true, "Scoped work item")
	requireNativeStatus(t, linked, http.StatusOK)
	var linkedResult conversationLinkResponse
	decodeHubResponse(t, linked, &linkedResult)

	if f.service.database.db != nil || f.service.database.reader != nil || f.service.database.authReader != nil || f.service.database.claimReader != nil {
		t.Fatal("conversation fixture retained legacy SQL handles")
	}

	projectList := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations", f.token, nil)
	requireNativeStatus(t, projectList, http.StatusOK)
	organizationList := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/conversations", f.token, nil)
	requireNativeStatus(t, organizationList, http.StatusOK)
	itemConversation := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+linkedResult.Issue.ID+"/conversation?metadata=true", f.token, nil)
	requireNativeStatus(t, itemConversation, http.StatusOK)

	foreignProject := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects/prj_hidden/conversations", f.token, nil)
	requireNativeStatus(t, foreignProject, http.StatusForbidden)
	foreignOrganization := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/org_hidden/projects/prj_hidden/conversations", f.token, nil)
	requireNativeStatus(t, foreignOrganization, http.StatusUnauthorized)

	var item corestore.WorkItem
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		item, err = r.WorkItem(t.Context(), f.project.ID, tracker.NativeWorkItemID(linkedResult.Issue.ID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reporterScope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeOperator, HostedRole: "reporter", HostedPrincipal: stringValue(item.ActorPrincipalID)}}
	if _, _, err := f.service.readWorkItemRecord(t.Context(), reporterScope, linkedResult.Issue.ID); err != nil {
		t.Fatalf("reporter could not read its own work item: %v", err)
	}
	reporterScope.credential.HostedPrincipal = "other-reporter"
	if _, _, err := f.service.readWorkItemRecord(t.Context(), reporterScope, linkedResult.Issue.ID); err == nil {
		t.Fatal("reporter read another principal's work item")
	} else {
		var failure *nativeError
		if !errors.As(err, &failure) || failure.status != http.StatusNotFound {
			t.Fatalf("foreign reporter visibility error = %v, want not found", err)
		}
	}
}

// TestConversationContinueRequiresExpectedAttempt covers the attempt a
// continue must name: null is only honest for a conversation that never had
// an attempt, so a conversation that has or had one refuses it.
func TestConversationContinueRequiresExpectedAttempt(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Continue"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Continue me"), http.StatusOK)

	t.Run("null is accepted before any attempt", func(t *testing.T) {
		requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "continue-1", Kind: conversation.CommandContinue}), http.StatusOK)
	})

	// Record an attempt the way a bind does.
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RecordStart(t.Context(), "att_past", record.ID, f.service.config.now())
	}); err != nil {
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
		f.setExecution(t, owned.ID, conversation.Execution{Owner: conversation.Owner{AttemptID: "att_live"}, Status: conversation.ExecutionCompleted, UpdatedAt: f.service.config.now()})
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
		record.Origin, record.Revision, record.Preferences = conversationOriginUser, 1, conversation.DefaultPreferences()
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateConversation(t.Context(), conversationRecordToStore(record))
			return err
		}); err != nil {
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

func TestHostedConversationRemovalAuthorization(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"archive", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, browserPreviewConfig)
			f.seedConversation(t)
			base := browserHostedOrganizationBase + "/projects/" + f.project
			path := base + "/conversations/" + f.conversation
			method := http.MethodDelete
			if action == "archive" {
				method = http.MethodPost
				path += "/archive"
			}
			f.api(t, "viewer", method, path, nil, http.StatusForbidden)
			f.api(t, "wrong-organization", method, path, nil, http.StatusForbidden)
			var snapshot conversationSnapshotResponse
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/conversations/"+f.conversation, nil, http.StatusOK), &snapshot)
			if snapshot.Conversation.Archived || len(snapshot.Messages) == 0 {
				t.Fatal("unauthorized removal changed conversation history")
			}
		})
	}
}

type conversationCancelFailureRepository struct {
	tenantstore.Repository
	intent                 atomic.Bool
	completion             atomic.Bool
	intentFailures         atomic.Int32
	completionFailures     atomic.Int32
	scanContexts           chan context.Context
	pendingLimit           int
	completionConversation string
	completionError        error
}

func (r *conversationCancelFailureRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if r.scanContexts != nil && workload == corestore.Background && ctx.Value(conversationCancelScanContextKey{}) == true {
		select {
		case r.scanContexts <- ctx:
		default:
		}
	}
	if r.pendingLimit > 0 && workload == corestore.Background && ctx.Value(conversationCancelScanContextKey{}) == true {
		return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
			return read(conversationCancelScanReader{Reader: reader, limit: r.pendingLimit})
		})
	}
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

func (r *conversationCancelFailureRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error { return write(conversationCancelFailureWriter{Writer: w, owner: r}) })
}

type conversationCancelFailureWriter struct {
	tenantstore.Writer
	owner *conversationCancelFailureRepository
}

func (w conversationCancelFailureWriter) UpdateCommand(ctx context.Context, command conversationstore.Command) error {
	if command.Kind == conversation.CommandCancel && command.Receipt.Status == conversation.DeliveryDelivered && w.owner.completion.Load() && (w.owner.completionConversation == "" || command.ConversationID == w.owner.completionConversation) {
		w.owner.completionFailures.Add(1)
		if w.owner.completionError != nil {
			return w.owner.completionError
		}
		return errors.New("injected cancellation completion persistence failure")
	}
	return w.Writer.UpdateCommand(ctx, command)
}

func (w conversationCancelFailureWriter) AppendConversationEvent(ctx context.Context, event conversationstore.Event) (conversationstore.Event, error) {
	if event.Type == conversation.EventCommandReceipt && w.owner.intent.Load() {
		var receipt conversation.Receipt
		if json.Unmarshal(event.Data, &receipt) == nil && receipt.Kind == conversation.CommandCancel {
			w.owner.intentFailures.Add(1)
			return conversationstore.Event{}, errors.New("injected cancellation intent persistence failure")
		}
	}
	return w.Writer.AppendConversationEvent(ctx, event)
}

type conversationCancelScanContextKey struct{}

type conversationCancelBlockingContents struct {
	*conversationContentFixture
	block   atomic.Bool
	calls   atomic.Int32
	entered chan context.Context
	release chan struct{}
}

func (s *conversationCancelBlockingContents) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if !s.block.Load() {
		return s.conversationContentFixture.PublishContent(ctx, organization, content)
	}
	s.calls.Add(1)
	select {
	case s.entered <- ctx:
	default:
	}
	select {
	case <-ctx.Done():
		return attachment.ArchiveReference{}, ctx.Err()
	case <-s.release:
		return attachment.ArchiveReference{}, errors.New("released blocked cancellation storage")
	}
}

type conversationCancelScanReader struct {
	tenantstore.Reader
	limit int
}

func (r conversationCancelScanReader) PendingConversations(ctx context.Context, after string, limit int) (conversationstore.PendingConversationPage, error) {
	return r.Reader.PendingConversations(ctx, after, min(limit, r.limit))
}

type conversationScanControlSignals struct {
	conversationControlRouter
	wakes chan string
}

func (s *conversationScanControlSignals) Wake(id string) {
	s.wakes <- id
	s.conversationControlRouter.Wake(id)
}
