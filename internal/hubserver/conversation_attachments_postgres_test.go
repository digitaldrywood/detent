package hubserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestConversationAttachmentPostgresHTTP(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	objects := &conversationContentFixture{}
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), now: time.Now}}
	service.conversations = &conversationService{server: service, store: &conversationStore{contents: objects}}
	service.conversations.config.ControlQueueSize = 100
	service.conversations.broker = newNotificationBroker()
	service.conversations.coordinator = newConversationCoordinator(service.conversations)
	service.ready.Store(true)
	service.registerConversationAPIRoutes(service.echo)
	type fixture struct {
		org             tracker.OrganizationID
		project         tracker.ProjectID
		id, other, base string
	}
	fixtures := []fixture{}
	for i := range 2 {
		org := nativeOrganization{ID: tracker.OrganizationID(fmt.Sprintf("org_attachment_%d", i)), Name: "Example", Slug: fmt.Sprintf("example-%d", i)}
		seedPostgresHubAdmin(t, repository, service, org)
		f := fixture{org: org.ID, project: tracker.ProjectID(fmt.Sprintf("project_attachment_%d", i)), id: conversation.NewConversationID(), other: conversation.NewConversationID()}
		f.base = fmt.Sprintf("/api/v2/organizations/%s/projects/%s", f.org, f.project)
		now := time.Now().UTC()
		if err := repository.WriteTenant(t.Context(), f.org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.org, ID: f.project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			for _, id := range []string{f.id, f.other} {
				if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: f.org, ProjectID: f.project, ID: id, OwnerPrincipalID: "test_hub_admin", Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT(organization_id,name) DO UPDATE SET value=0`, f.org); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, f)
	}
	for _, f := range fixtures {
		t.Run(string(f.org)+"_delete_downgrade", func(t *testing.T) {
			payload := []byte("current write authority")
			upload := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload)
			var originalScope string
			var collaboration, archive int64
			if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
				credential, err := r.AccessCredential(t.Context(), "test_hub_admin")
				if err != nil {
					return err
				}
				originalScope = credential.KeyScope
				collaboration, err = r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				archive, err = r.ArchiveBytes(t.Context())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				service.config.TenantRepository = repository
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
				defer cancel()
				if _, err := admin.ExecContext(cleanup, `UPDATE api_tokens SET operator_key_scope=$2 WHERE organization_id=$1 AND id='test_hub_admin'`, f.org, originalScope); err != nil {
					t.Error(err)
				}
			})
			fired := false
			counter := &attachmentReadCounter{Repository: repository, beforeWrite: func() error {
				fired = true
				_, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET operator_key_scope='read' WHERE organization_id=$1 AND id='test_hub_admin'`, f.org)
				return err
			}}
			service.config.TenantRepository = counter
			response := performHubAPIRequest(t, service, http.MethodDelete, upload.Attachment.URL, testHubAdminToken, nil)
			service.config.TenantRepository = repository
			if !fired {
				t.Fatal("write callback did not observe the downgrade")
			}
			requireNativeStatus(t, response, http.StatusForbidden)
			if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
				attachment, err := r.(conversationstore.AttachmentReader).ConversationAttachment(t.Context(), f.project, f.id, upload.ID)
				if err != nil {
					return err
				}
				if attachment.DeletedAt != nil || attachment.ContentID == "" {
					return errors.New("downgraded deletion changed attachment ownership")
				}
				used, err := r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				stored, err := r.ArchiveBytes(t.Context())
				if err == nil && (used != collaboration || stored != archive) {
					return errors.New("downgraded deletion changed accounting")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil), http.StatusOK)
			if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET operator_key_scope=$2 WHERE organization_id=$1 AND id='test_hub_admin'`, f.org, originalScope); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, upload.Attachment.URL, testHubAdminToken, nil), http.StatusNoContent)
		})
	}
	for _, f := range fixtures {
		t.Run(string(f.org)+"_send", func(t *testing.T) {
			upload := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.other, "send.txt", "text/plain", []byte("sent payload"))
			command := conversation.Command{Key: "cmd_attachment_send", Kind: conversation.CommandMessage, Text: "attached", Attachments: []string{upload.ID}}
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/conversations/"+f.other+"/commands", testHubAdminToken, command)
			requireNativeStatus(t, response, http.StatusOK)
			var receipt conversation.Receipt
			decodeHubResponse(t, response, &receipt)
			replay := performHubAPIRequest(t, service, http.MethodPost, f.base+"/conversations/"+f.other+"/commands", testHubAdminToken, command)
			requireNativeStatus(t, replay, http.StatusOK)
			var replayReceipt conversation.Receipt
			decodeHubResponse(t, replay, &replayReceipt)
			if replayReceipt.MessageID != receipt.MessageID {
				t.Fatal("replay changed message")
			}
			page := performHubAPIRequest(t, service, http.MethodGet, f.base+"/conversations/"+f.other, testHubAdminToken, nil)
			requireNativeStatus(t, page, http.StatusOK)
			var body conversationSnapshotResponse
			decodeHubResponse(t, page, &body)
			if len(body.Messages) != 1 || body.Messages[0].ID != receipt.MessageID || len(body.Messages[0].Attachments) != 1 || body.Messages[0].Attachments[0] != upload.Attachment {
				t.Fatalf("read differs: %+v", body.Messages)
			}
			if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(conversationstore.AttachmentReader)
				items, err := owner.MessageAttachments(t.Context(), f.project, f.other, []string{receipt.MessageID, "msg_absent"})
				if err != nil {
					return err
				}
				if len(items) != 1 || len(items[receipt.MessageID]) != 1 || items[receipt.MessageID][0].ID != upload.ID {
					t.Fatalf("selected attachment batch differs: %+v", items)
				}
				other, err := owner.MessageAttachments(t.Context(), f.project, f.id, []string{receipt.MessageID})
				if err != nil {
					return err
				}
				if len(other) != 0 {
					t.Fatal("batch leaked another conversation")
				}
				for _, size := range []int{0, corestore.MaxPageSize, corestore.MaxPageSize + 1} {
					ids := make([]string, size)
					for i := range ids {
						ids[i] = fmt.Sprintf("msg_absent_%d", i)
					}
					_, err := owner.MessageAttachments(t.Context(), f.project, f.other, ids)
					if size > corestore.MaxPageSize {
						if !errors.Is(err, corestore.ErrInvalidPage) {
							t.Fatalf("attachment batch bound: %v", err)
						}
					} else if err != nil {
						return err
					}
					_, err = owner.MessageSequences(t.Context(), f.other, ids)
					if size > corestore.MaxPageSize {
						if !errors.Is(err, corestore.ErrInvalidPage) {
							t.Fatalf("sequence batch bound: %v", err)
						}
					} else if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			scope := nativeScope{organization: f.org, project: f.project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
			retainedMessageWant := body.Messages[0]
			retainedMessageWant.Attachments = nil
			retainedMessageWant.Text = "retained accepted snapshot"
			retainedData, err := json.Marshal(retainedMessageWant)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := json.Marshal(conversationContent{Data: retainedData})
			if err != nil {
				t.Fatal(err)
			}
			retainedRef, err := objects.PublishContent(t.Context(), string(f.org), envelope)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), f.org, corestore.Serving, func(w tenantstore.Writer) error {
				contentID := contentArchiveID(string(f.org), retainedRef)
				if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: f.org, ID: contentID, Reference: retainedRef, CreatedAt: time.Now().UTC()}); err != nil {
					return err
				}
				event, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.org, ConversationID: f.other, ContentReference: contentID, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}})
				body.Cursor = event.Seq
				return err
			}); err != nil {
				t.Fatal(err)
			}
			counter := &attachmentReadCounter{Repository: repository}
			service.config.TenantRepository = counter
			streamResponse := httptest.NewRecorder()
			stream := newConversationStream(service.echo.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), streamResponse))
			if _, err := service.replayPGConversationEvents(t.Context(), scope, stream, f.other, 0, body.Cursor); err != nil {
				t.Fatal(err)
			}
			service.config.TenantRepository = repository
			if counter.reads != 2 {
				t.Fatalf("archived event page used %d tenant admissions, want 2", counter.reads)
			}
			frameReader := bufio.NewReader(bytes.NewReader(streamResponse.Body.Bytes()))
			frame, err := readSSEFrame(frameReader)
			if err != nil {
				t.Fatal(err)
			}
			var accepted conversationMessageResource
			if err := json.Unmarshal([]byte(frame.Data), &accepted); err != nil {
				t.Fatal(err)
			}
			if frame.Event != string(conversation.EventMessageAccepted) || accepted.Seq != body.Messages[0].Seq || len(accepted.Attachments) != 1 || accepted.Attachments[0] != upload.Attachment {
				t.Fatalf("event differs: %+v %+v", frame, accepted)
			}
			receiptFrame, err := readSSEFrame(frameReader)
			if err != nil {
				t.Fatal(err)
			}
			if receiptFrame.Event != string(conversation.EventCommandReceipt) || receiptFrame.ID == frame.ID {
				t.Fatalf("receipt order differs: %+v", receiptFrame)
			}
			retainedFrame, err := readSSEFrame(frameReader)
			if err != nil {
				t.Fatal(err)
			}
			var retainedMessage conversationMessageResource
			if err := json.Unmarshal([]byte(retainedFrame.Data), &retainedMessage); err != nil {
				t.Fatal(err)
			}
			if retainedFrame.Event != frame.Event || retainedFrame.ID != fmt.Sprint(body.Cursor) || retainedFrame.Data != string(retainedData) || retainedMessage.ID != accepted.ID || retainedMessage.Seq != accepted.Seq {
				t.Fatalf("retained envelope replay differs: %+v %+v", retainedFrame, retainedMessage)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, upload.URL, testHubAdminToken, nil), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, upload.URL, testHubAdminToken, nil), http.StatusForbidden)

			var boundMessage, reference, inline string
			if err := admin.QueryRowContext(t.Context(), `SELECT a.message_id,e.content_attachment_id,e.body_json FROM conversation_attachments a JOIN conversation_events e ON e.organization_id=a.organization_id AND e.conversation_id=a.conversation_id AND e.type='message.accepted' WHERE a.organization_id=$1 AND a.id=$2`, f.org, upload.ID).Scan(&boundMessage, &reference, &inline); err != nil {
				t.Fatal(err)
			}
			if boundMessage != receipt.MessageID || reference == "" || inline != "{}" {
				t.Fatalf("binding/event %s %s %s", boundMessage, reference, inline)
			}
			command.Text = "different"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/conversations/"+f.other+"/commands", testHubAdminToken, command), http.StatusConflict)
			command.Text = "attached"
			command.Key = "cmd_second_claim"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/conversations/"+f.other+"/commands", testHubAdminToken, command), http.StatusUnprocessableEntity)

			candidate := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.other, "claim.txt", "text/plain", []byte("claim payload"))
			claimURL := f.base + "/conversations/" + f.other + "/commands"
			for _, tc := range []struct{ name, column, value string }{
				{"foreign_principal", "principal_id", "another_principal"},
				{"expired", "expires_at", "2000-01-01T00:00:00Z"},
				{"invalid_size", "size", "20971521"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var original string
					if err := admin.QueryRowContext(t.Context(), `SELECT `+tc.column+`::text FROM conversation_attachments WHERE organization_id=$1 AND id=$2`, f.org, candidate.ID).Scan(&original); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_attachments SET `+tc.column+`=$3 WHERE organization_id=$1 AND id=$2`, f.org, candidate.ID, tc.value); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_attachments SET `+tc.column+`=$3 WHERE organization_id=$1 AND id=$2`, f.org, candidate.ID, original); err != nil {
							t.Error(err)
						}
					}()
					response := performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: "cmd_" + tc.name, Kind: conversation.CommandMessage, Text: "candidate", Attachments: []string{candidate.ID}})
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				})
			}
			other := fixtures[0]
			if other.org == f.org {
				other = fixtures[1]
			}
			for _, target := range []string{f.base + "/conversations/" + f.id + "/commands", other.base + "/conversations/" + other.other + "/commands"} {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, target, testHubAdminToken, conversation.Command{Key: "cmd_foreign", Kind: conversation.CommandMessage, Text: "foreign", Attachments: []string{candidate.ID}}), http.StatusUnprocessableEntity)
			}

			for _, ids := range [][]string{{candidate.ID, candidate.ID}, {candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID, candidate.ID}} {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: "cmd_invalid_selection", Kind: conversation.CommandMessage, Attachments: ids}), http.StatusUnprocessableEntity)
			}
			objects.onIO = func() error {
				_, err := admin.ExecContext(t.Context(), `UPDATE conversation_attachments SET name='changed.txt' WHERE organization_id=$1 AND id=$2`, f.org, candidate.ID)
				return err
			}
			changed := performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: "cmd_changed", Kind: conversation.CommandMessage, Text: "source", Attachments: []string{candidate.ID}})
			objects.onIO = nil
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_attachments SET name='claim.txt' WHERE organization_id=$1 AND id=$2`, f.org, candidate.ID); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, changed, http.StatusConflict)

			large := bytes.Repeat([]byte("x"), conversation.MaxAttachmentBytes/2+1)
			largeIDs := []string{}
			for i := range 2 {
				a := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.other, fmt.Sprintf("large%d.txt", i), "text/plain", large)
				largeIDs = append(largeIDs, a.ID)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: "cmd_total", Kind: conversation.CommandMessage, Attachments: largeIDs}), http.StatusUnprocessableEntity)
			for _, id := range largeIDs {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, conversationAttachmentURL(f.org, f.project, f.other, id), testHubAdminToken, nil), http.StatusNoContent)
			}
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 2)
			var workers sync.WaitGroup
			for i := range 2 {
				workers.Go(func() {
					<-start
					results <- performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: fmt.Sprintf("cmd_race_%d", i), Kind: conversation.CommandMessage, Text: "race", Attachments: []string{candidate.ID}})
				})
			}
			close(start)
			workers.Wait()
			close(results)
			winners := 0
			for result := range results {
				if result.Code == http.StatusOK {
					winners++
				} else if result.Code != http.StatusUnprocessableEntity && result.Code != http.StatusConflict {
					t.Fatalf("race status %d: %s", result.Code, result.Body.String())
				}
			}
			if winners != 1 {
				t.Fatalf("claim winners=%d", winners)
			}
			page = performHubAPIRequest(t, service, http.MethodGet, f.base+"/conversations/"+f.other, testHubAdminToken, nil)
			requireNativeStatus(t, page, http.StatusOK)
			decodeHubResponse(t, page, &body)
			bound := 0
			for _, message := range body.Messages {
				for _, a := range message.Attachments {
					if a.ID == candidate.ID {
						bound++
					}
				}
			}
			if bound != 1 || len(body.Messages) != 2 {
				t.Fatalf("race double-bound or leaked message: %+v", body.Messages)
			}
			revokedUpload := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.other, "revoke.txt", "text/plain", []byte("revoke send"))
			objects.onIO = func() error {
				_, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=$2 WHERE id='test_hub_admin' AND organization_id=$1`, f.org, time.Now().UTC())
				return err
			}
			revoked := performHubAPIRequest(t, service, http.MethodPost, claimURL, testHubAdminToken, conversation.Command{Key: "cmd_revoked_send", Kind: conversation.CommandMessage, Text: "revoked", Attachments: []string{revokedUpload.ID}})
			objects.onIO = nil
			if _, err := admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=NULL WHERE id='test_hub_admin' AND organization_id=$1`, f.org); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, revoked, http.StatusForbidden)
		})
	}
	baseUsage := map[tracker.OrganizationID]int64{}
	baseArchive := map[tracker.OrganizationID]int64{}
	for _, f := range fixtures {
		if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			baseUsage[f.org], err = r.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			baseArchive[f.org], err = r.ArchiveBytes(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	usage := func(f fixture, want int64) {
		t.Helper()
		if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.CollaborationBytes(t.Context())
			if err == nil && got != want+baseUsage[f.org] {
				return fmt.Errorf("usage=%d want=%d", got, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range fixtures {
		t.Run(string(f.org), func(t *testing.T) {
			payload := []byte("example attachment")
			response := postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload, "upload")
			requireNativeStatus(t, response, http.StatusCreated)
			var upload conversation.AttachmentUpload
			decodeHubResponse(t, response, &upload)
			usage(f, int64(len(payload)))
			before := objects.publications
			replay := postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload, "upload")
			requireNativeStatus(t, replay, http.StatusCreated)
			if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) || objects.publications != before {
				t.Fatal("replay changed receipt or published content")
			}
			requireNativeStatus(t, postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", []byte("different"), "upload"), http.StatusConflict)
			read := performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil)
			requireNativeStatus(t, read, http.StatusOK)
			if !bytes.Equal(read.Body.Bytes(), payload) {
				t.Fatal("download differs")
			}
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				requireNativeStatus(t, performHubAPIRequest(t, service, method, f.base+"/conversations/"+f.other+"/attachments/"+upload.Attachment.ID, testHubAdminToken, nil), http.StatusNotFound)
				other := fixtures[1]
				if other.org == f.org {
					other = fixtures[0]
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, method, other.base+"/conversations/"+other.id+"/attachments/"+upload.Attachment.ID, testHubAdminToken, nil), http.StatusNotFound)
			}
			objects.onIO = func() error {
				_, err := admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=$3 WHERE organization_id=$1 AND id=$2`, f.org, f.project, time.Now().UTC())
				return err
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil), http.StatusNotFound)
			objects.onIO = nil
			if _, err := admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=NULL WHERE organization_id=$1 AND id=$2`, f.org, f.project); err != nil {
				t.Fatal(err)
			}
			objects.setFail(true)
			failed := postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "failure.txt", "text/plain", []byte("unpublished"), "failure")
			requireNativeStatus(t, failed, http.StatusInternalServerError)
			objects.setFail(false)
			usage(f, int64(len(payload)))
			duplicate := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.id, "duplicate.txt", "text/plain", payload)
			usage(f, int64(2*len(payload)))
			objects.onIO = func() error { return fmt.Errorf("blob unavailable") }
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, duplicate.Attachment.URL, testHubAdminToken, nil), http.StatusInternalServerError)
			objects.onIO = nil
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, duplicate.Attachment.URL, testHubAdminToken, nil), http.StatusNoContent)
			usage(f, int64(len(payload)))
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil), http.StatusOK)
			if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
				n, err := r.ArchiveBytes(t.Context())
				if err == nil && n <= 0 {
					return fmt.Errorf("shared archive lost ownership")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, upload.Attachment.URL, testHubAdminToken, nil), http.StatusNoContent)
			usage(f, 0)
			if err := repository.ReadTenant(t.Context(), f.org, corestore.Serving, func(r tenantstore.Reader) error {
				n, err := r.ArchiveBytes(t.Context())
				if err == nil && n != baseArchive[f.org] {
					return fmt.Errorf("deleted archive bytes=%d", n)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil), http.StatusNotFound)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, upload.Attachment.URL, testHubAdminToken, nil), http.StatusNotFound)
			requireNativeStatus(t, postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload, "upload"), http.StatusConflict)
		})
	}
	for _, f := range fixtures {
		t.Run(string(f.org)+"_revocation", func(t *testing.T) {
			payload := []byte("revocation attachment")
			upload := uploadConversationAttachmentFile(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload)
			fired := false
			objects.onIO = func() error {
				if fired {
					return nil
				}
				fired = true
				return repository.WriteTenant(t.Context(), f.org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessCredential(t.Context(), "test_hub_admin", time.Now().UTC())
				})
			}
			response := performHubAPIRequest(t, service, http.MethodGet, upload.Attachment.URL, testHubAdminToken, nil)
			objects.onIO = nil
			requireNativeStatus(t, response, http.StatusForbidden)
			if bytes.Contains(response.Body.Bytes(), payload) {
				t.Fatal("revoked read disclosed payload")
			}
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				requireNativeStatus(t, performHubAPIRequest(t, service, method, upload.Attachment.URL, testHubAdminToken, nil), http.StatusUnauthorized)
			}
			requireNativeStatus(t, postConversationAttachment(t, service, f.base, testHubAdminToken, f.id, "example.txt", "text/plain", payload, "revoked"), http.StatusUnauthorized)
			usage(f, int64(len(payload)))
			if err := repository.WriteTenant(t.Context(), f.org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.org, ID: "credential_current", Name: "Example", Hash: apikey.HashToken("example-current-token"), Scope: "admin", KeyScope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET owner_principal_id='credential_current' WHERE organization_id=$1`, f.org); err != nil {
				t.Fatal(err)
			}
			fired = false
			objects.onIO = func() error {
				if fired {
					return nil
				}
				fired = true
				return repository.WriteTenant(t.Context(), f.org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessCredential(t.Context(), "credential_current", time.Now().UTC())
				})
			}
			response = postConversationAttachment(t, service, f.base, "example-current-token", f.id, "example.txt", "text/plain", []byte("must not commit"), "in-flight")
			objects.onIO = nil
			requireNativeStatus(t, response, http.StatusForbidden)
			usage(f, int64(len(payload)))
		})
	}

}

type attachmentReadCounter struct {
	tenantstore.Repository
	reads       int
	beforeWrite func() error
}

func (r *attachmentReadCounter) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.beforeWrite != nil {
		before := r.beforeWrite
		r.beforeWrite = nil
		if err := before(); err != nil {
			return err
		}
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func (r *attachmentReadCounter) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	return r.Repository.ReadTenant(ctx, org, workload, read)
}
