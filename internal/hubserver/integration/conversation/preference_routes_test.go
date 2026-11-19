package conversation_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const conversationPreferencesModel = "gpt-6-astra"

func createRouteConversation(t *testing.T, p hubfixture.Native, title string) conversations.ConversationResource {
	t.Helper()
	response := p.Request(t, http.MethodPost, p.Base+"/conversations", conversations.ConversationCreateRequest{Key: hubapi.NewID("create"), Title: title})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var result conversations.ConversationCreatedResponse
	hubfixture.Decode(t, response, &result)
	return result.Conversation
}

func readRouteConversation(t *testing.T, p hubfixture.Native, id string) conversations.ConversationSnapshot {
	t.Helper()
	response := p.Request(t, http.MethodGet, p.Base+"/conversations/"+id, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var result conversations.ConversationSnapshot
	hubfixture.Decode(t, response, &result)
	return result
}

func TestConversationPreferences(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	p := hubfixture.NewNative(t, h, h.Primary, "Conversation preferences")
	runner := hubfixture.PrepareRunner(t, p, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.Enroll(t)
	report := hubfixture.CapacityReport(h.Now())
	report.Models = []string{conversationPreferencesModel, "claude-opus-5"}
	hubfixture.PublishCapacity(t, p, runner, report)
	record := createRouteConversation(t, p, "Preferences")
	path := p.Base + "/conversations/" + record.ID
	patch := func(t *testing.T, body map[string]any) conversations.ConversationResource {
		t.Helper()
		response := p.Request(t, http.MethodPatch, path, body)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var updated conversations.ConversationResource
		hubfixture.Decode(t, response, &updated)
		return updated
	}
	snapshot := func(t *testing.T) conversations.ConversationResource {
		t.Helper()
		return readRouteConversation(t, p, record.ID).Conversation
	}
	if got := record.Preferences; got != conversation.DefaultPreferences() {
		t.Fatalf("new conversation preferences = %#v, want every field auto", got)
	}

	t.Run("partial updates keep the other fields", func(t *testing.T) {
		updated := patch(t, map[string]any{"preferences": map[string]any{"reasoning_effort": "high"}})
		want := conversation.Preferences{Model: conversation.PreferenceAuto, ReasoningEffort: conversation.EffortHigh, Access: conversation.PreferenceAuto}
		if updated.Preferences != want {
			t.Fatalf("preferences = %#v, want %#v", updated.Preferences, want)
		}
		updated = patch(t, map[string]any{"preferences": map[string]any{"model": conversationPreferencesModel, "access": conversation.AccessReadOnly}})
		want = conversation.Preferences{Model: conversationPreferencesModel, ReasoningEffort: conversation.EffortHigh, Access: conversation.AccessReadOnly}
		if updated.Preferences != want {
			t.Fatalf("preferences = %#v, want %#v", updated.Preferences, want)
		}
		if got := snapshot(t).Preferences; got != want {
			t.Fatalf("stored preferences = %#v, want %#v", got, want)
		}
	})

	t.Run("auto puts a field back", func(t *testing.T) {
		updated := patch(t, map[string]any{"preferences": map[string]any{"model": "auto", "reasoning_effort": "auto", "access": "auto"}})
		if updated.Preferences != conversation.DefaultPreferences() {
			t.Fatalf("preferences = %#v, want every field auto", updated.Preferences)
		}
	})

	t.Run("title and preferences travel together", func(t *testing.T) {
		updated := patch(t, map[string]any{"title": "Renamed", "preferences": map[string]any{"reasoning_effort": "low"}})
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
			before := snapshot(t)
			response := p.Request(t, http.MethodPatch, path, map[string]any{"preferences": test.preferences})
			hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
			after := snapshot(t)
			if after.Preferences != before.Preferences || after.Title != before.Title {
				t.Fatalf("refused preferences changed conversation: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestConversationMessageReferences(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	p := hubfixture.NewNative(t, h, h.Primary, "native")
	otherActor := hubtest.Credential(t, h.Repository, p.Organization(), hubtest.CredentialOptions{Projects: []tracker.ProjectID{p.Project.ID}, Now: h.Now()})
	issue := p.Create(t, "Referenced issue")
	other := p.Create(t, "Second issue")
	target := createRouteConversation(t, p, "Target chat")
	record := createRouteConversation(t, p, "References")
	path := p.Base + "/conversations/" + record.ID
	var organizationSlug, projectSlug string
	if err := h.Repository.ReadTenant(t.Context(), p.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(t.Context())
		if err != nil {
			return err
		}
		project, err := r.Project(t.Context(), p.Project.ID)
		if err != nil {
			return err
		}
		organizationSlug, projectSlug = organization.Slug, project.Slug
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	listReferences := func(t *testing.T, token string) conversations.ConversationWorkItemReferencePage {
		t.Helper()
		response := h.Request(t, http.MethodGet, p.Base+"/work-items/"+string(issue.WorkItemID)+"/references", token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page conversations.ConversationWorkItemReferencePage
		hubfixture.Decode(t, response, &page)
		return page
	}

	text := "See #" + strconv.Itoa(issue.Number) +
		" and " + p.Project.Name + "#" + strconv.Itoa(other.Number) +
		" and " + target.ID + ", but not #9999 or conv_" + strings.Repeat("0", 32) + "."
	hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, path+"/commands", conversation.Command{Key: "refs", Kind: conversation.CommandMessage, Text: text}), http.StatusOK)

	messages := readRouteConversation(t, p, record.ID).Messages
	index := slices.IndexFunc(messages, func(message conversations.ConversationMessageResource) bool { return message.Text == text })
	if index < 0 {
		t.Fatal("submitted message is missing")
	}
	message := messages[index]
	want := []conversations.ConversationReferenceResource{
		{Kind: conversation.ReferenceIssue, ID: string(issue.WorkItemID), Label: "#" + strconv.Itoa(issue.Number), URL: "/" + organizationSlug + "/" + projectSlug + "/issues/" + strconv.Itoa(issue.Number)},
		{Kind: conversation.ReferenceIssue, ID: string(other.WorkItemID), Label: p.Project.Name + "#" + strconv.Itoa(other.Number), URL: "/" + organizationSlug + "/" + projectSlug + "/issues/" + strconv.Itoa(other.Number)},
		{Kind: conversation.ReferenceConversation, ID: target.ID, Label: target.ID, URL: "/chat/c/" + target.ID},
	}
	if !slices.Equal(message.References, want) {
		t.Fatalf("resolved references = %v, want %v", message.References, want)
	}

	t.Run("the target lists what referenced it", func(t *testing.T) {
		for _, entry := range listReferences(t, p.Token).References {
			if entry.MessageID == message.ID && entry.ConversationID == record.ID {
				if !strings.HasPrefix(entry.Excerpt, "See #") {
					t.Fatalf("excerpt = %q", entry.Excerpt)
				}
				return
			}
		}
		t.Fatalf("issue is missing the reference from message %s", message.ID)
	})

	t.Run("a private chat is invisible to another reader", func(t *testing.T) {
		for _, entry := range listReferences(t, otherActor.Token).References {
			if entry.ConversationID == record.ID {
				t.Fatalf("private conversation %s appeared for another reader", record.ID)
			}
		}
	})

	t.Run("a shared chat is visible to every issue reader", func(t *testing.T) {
		hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, path+"/link", conversations.ConversationLinkRequest{
			Key: "share-refs", ShareHistory: true,
			Issue: conversations.ConversationLinkIssue{Title: "Shared now", Description: "Share the discussion" + hubfixture.IssueContractSections},
		}), http.StatusOK)
		for _, entry := range listReferences(t, otherActor.Token).References {
			if entry.MessageID == message.ID && entry.ConversationID == record.ID {
				return
			}
		}
		t.Fatalf("issue reader is missing the shared conversation %s", record.ID)
	})

	t.Run("an unknown issue is not found", func(t *testing.T) {
		response := p.Request(t, http.MethodGet, p.Base+"/work-items/wi_missing/references", nil)
		hubfixture.RequireCode(t, response, http.StatusNotFound, "not_found")
	})
}
