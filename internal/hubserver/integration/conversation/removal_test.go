package conversation_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestConversationRemovalUsesAudienceAndArchivedContent(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	now := h.Now().UTC().Truncate(time.Microsecond)
	objects := &conversations.ConversationStore{Contents: conversations.ArchiveStore{Call: h.Config.Content}}
	payload, err := json.Marshal(conversations.ConversationRecordContent{
		Execution:   conversation.Execution{Status: conversation.ExecutionIdle},
		Preferences: conversation.Preferences{Model: "example-model", ReasoningEffort: "auto", Access: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := conversations.MessageContent(conversations.ConversationMessageRecord{Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		allowed bool
		running bool
	}{{"matching audience", true, false}, {"different audience", false, false}, {"typed running overrides archived idle", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			project := hubfixture.NewNative(t, h, h.Primary, "Removal "+test.name)
			org := project.Organization()
			owner, err := project.Credential(t, project.Token)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.PublishPayload(t.Context(), string(org), wrapped)
			if err != nil {
				t.Fatal(err)
			}
			contentID := conversations.ContentArchiveID(string(org), ref)
			id := conversation.NewConversationID()
			execution := conversation.Execution{Status: conversation.ExecutionIdle}
			if test.running {
				execution.Status = conversation.ExecutionRunning
			}
			err = h.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				audience, err := conversations.ConversationReadAudienceRecord(t.Context(), w, org, hubauth.Credential{ID: owner.ID, Scope: hubauth.Operator})
				if err != nil {
					return err
				}
				if !test.allowed {
					audience = "different"
				}
				if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: org, ID: contentID, Reference: ref, CreatedAt: now}); err != nil {
					return err
				}
				_, err = w.CreateConversation(t.Context(), conversationstore.Conversation{
					OrganizationID: org, ProjectID: project.Project.ID, ID: id, OwnerPrincipalID: owner.ID,
					Origin: conversations.ConversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate,
					Status: conversation.StatusActive, Revision: 1, Execution: execution,
					Preferences:  conversation.Preferences{Model: "current-model", ReasoningEffort: "auto", Access: "auto"},
					ReadAudience: audience, ContentReference: contentID, CreatedAt: now, UpdatedAt: now,
				})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}

			path := project.Base + "/conversations/" + id
			title := "Updated"
			patch := conversations.ConversationPatchRequest{Title: &title}
			get := project.Request(t, http.MethodGet, path, nil)
			messages := project.Request(t, http.MethodGet, path+"/messages", nil)
			if !test.allowed {
				hubfixture.RequireStatus(t, get, http.StatusNotFound)
				hubfixture.RequireStatus(t, messages, http.StatusNotFound)
				hubfixture.RequireStatus(t, project.Request(t, http.MethodPatch, path, patch), http.StatusNotFound)
				hubfixture.RequireStatus(t, project.Request(t, http.MethodPost, path+"/archive", nil), http.StatusNotFound)
				hubfixture.RequireStatus(t, project.Request(t, http.MethodDelete, path, nil), http.StatusNotFound)
				stream := openRemovalStream(t, server, path, project.Token)
				if stream.StatusCode != http.StatusNotFound {
					t.Fatalf("different audience stream status = %d", stream.StatusCode)
				}
				return
			}
			hubfixture.RequireStatus(t, get, http.StatusOK)
			hubfixture.RequireStatus(t, messages, http.StatusOK)
			var snapshot conversations.ConversationSnapshot
			hubfixture.Decode(t, get, &snapshot)
			if snapshot.Conversation.Execution.Status != execution.Status || snapshot.Conversation.Preferences.Model != "current-model" {
				t.Fatalf("current state = %+v", snapshot.Conversation)
			}
			archived := project.Request(t, http.MethodPost, path+"/archive", nil)
			if test.running {
				hubfixture.RequireStatus(t, archived, http.StatusConflict)
				hubfixture.RequireStatus(t, project.Request(t, http.MethodDelete, path, nil), http.StatusConflict)
				return
			}
			hubfixture.RequireStatus(t, archived, http.StatusOK)
			var record conversations.ConversationResource
			hubfixture.Decode(t, archived, &record)
			if !record.Archived || record.Revision != 2 || record.Preferences.Model != "current-model" {
				t.Fatalf("archived record = %+v", record)
			}
			hubfixture.RequireStatus(t, project.Request(t, http.MethodPatch, path, patch), http.StatusOK)
			stream := openRemovalStream(t, server, path, project.Token)
			if stream.StatusCode != http.StatusOK {
				t.Fatalf("matching audience stream status = %d", stream.StatusCode)
			}
			scanner := bufio.NewScanner(stream.Body)
			for index := 1; index <= 2; index++ {
				if !scanRemovalEvent(scanner, "id: "+strconv.Itoa(index)) || !scanRemovalEvent(scanner, "event: "+string(conversation.EventConversationUpdated)) {
					t.Fatalf("missing conversation update %d: %v", index, scanner.Err())
				}
				if !scanRemovalEvent(scanner, "data: ") {
					t.Fatalf("missing conversation payload %d: %v", index, scanner.Err())
				}
				hydrated := conversations.ConversationResource{}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &hydrated); err != nil {
					t.Fatal(err)
				}
				if hydrated.ID != id || !hydrated.Archived || hydrated.Preferences.Model != "current-model" || index == 2 && hydrated.Title != "Updated" {
					t.Fatalf("replayed conversation = %+v", hydrated)
				}
			}
			hubfixture.RequireStatus(t, project.Request(t, http.MethodDelete, path, nil), http.StatusNoContent)
			hubfixture.RequireStatus(t, project.Request(t, http.MethodDelete, path, nil), http.StatusNotFound)
		})
	}
}

func openRemovalStream(t *testing.T, server *httptest.Server, path, token string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func scanRemovalEvent(scanner *bufio.Scanner, prefix string) bool {
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), prefix) {
			return true
		}
	}
	return false
}
