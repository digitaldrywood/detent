package conversation_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationSubjectOwnership(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	project := hubfixture.NewNative(t, h, h.Primary, "Subject ownership")
	other := hubtest.Credential(t, h.Repository, project.Organization(), hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.Project.ID}, Now: h.Now()})
	path := project.Base + "/conversations"
	create := func(t *testing.T, token string, request conversations.ConversationCreateRequest) conversations.ConversationResource {
		t.Helper()
		response := h.Request(t, http.MethodPost, path, token, request)
		hubfixture.RequireStatus(t, response, http.StatusCreated)
		var result conversations.ConversationCreatedResponse
		hubfixture.Decode(t, response, &result)
		return result.Conversation
	}
	worker := create(t, project.Token, conversations.ConversationCreateRequest{Key: "worker"})
	linked := project.Request(t, http.MethodPost, path+"/"+worker.ID+"/link", conversations.ConversationLinkRequest{
		Key: "worker-link", ShareHistory: true,
		Issue: conversations.ConversationLinkIssue{Title: "Subject issue", Description: "From the chat" + hubfixture.IssueContractSections},
	})
	hubfixture.RequireStatus(t, linked, http.StatusOK)
	var handoff conversations.ConversationLinkResult
	hubfixture.Decode(t, linked, &handoff)
	issue := handoff.Issue.NativeIssue
	subject := string(issue.WorkItemID)
	visible := map[string][]string{}
	for _, test := range []struct {
		name, token string
	}{{"first", project.Token}, {"second", project.Token}, {"other owner", other.Token}} {
		t.Run(test.name, func(t *testing.T) {
			record := create(t, test.token, conversations.ConversationCreateRequest{
				Key: "creation-" + test.name, SubjectWorkItemID: subject,
				FirstMessage: &conversations.ConversationFirstMessage{Key: "question-" + test.name, Text: "Why blocked?"},
			})
			if record.SubjectWorkItemID == nil || *record.SubjectWorkItemID != subject || record.WorkItemID != nil || record.Visibility != conversation.VisibilityPrivate || !strings.HasPrefix(record.Title, fmt.Sprintf("#%d · ", issue.Number)) {
				t.Fatalf("conversation = %+v", record)
			}
			visible[test.token] = append(visible[test.token], record.ID)
			response := h.Request(t, http.MethodGet, path+"?subject_work_item_id="+subject, test.token, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var list conversations.ConversationListPage
			hubfixture.Decode(t, response, &list)
			var ids []string
			for _, conversation := range list.Conversations {
				ids = append(ids, conversation.ID)
			}
			slices.Sort(ids)
			want := slices.Clone(visible[test.token])
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("visible subject conversations = %v, want %v", ids, want)
			}
			if test.name == "first" {
				request := conversations.ConversationLinkRequest{Key: "private-share", ShareHistory: true, Issue: conversations.ConversationLinkIssue{Title: "Cannot share questions"}}
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, path+"/"+record.ID+"/link", test.token, request), http.StatusUnprocessableEntity)
			}
		})
	}
	hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, path+"/"+visible[project.Token][0], other.Token, nil), http.StatusNotFound)
	foreign := hubfixture.NewNative(t, h, h.Primary, "Foreign subject").Create(t, "Foreign issue")
	for _, unavailable := range []string{"wi_missing", string(foreign.WorkItemID)} {
		request := conversations.ConversationCreateRequest{Key: "unavailable-" + unavailable, SubjectWorkItemID: unavailable}
		hubfixture.RequireStatus(t, project.Request(t, http.MethodPost, path, request), http.StatusNotFound)
	}
	comments := project.Request(t, http.MethodGet, project.Base+"/work-items/"+subject+"/comments", nil)
	hubfixture.RequireStatus(t, comments, http.StatusOK)
	if strings.Contains(comments.Body.String(), "Why blocked?") {
		t.Fatal("private question appeared as an issue comment")
	}
}
