package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationPostgresStreamReauthorizesAgainstTenantProject(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Conversation: &ConversationConfig{Enabled: true},
	})
	for _, orgID := range []tracker.OrganizationID{"org_stream", "org_stream_other"} {
		t.Run(string(orgID), func(t *testing.T) {
			organization := nativeOrganization{ID: orgID, Name: string(orgID), Slug: string(orgID)}
			seedPostgresHubAdmin(t, repository, service, organization)
			projectID := tracker.ProjectID("project_stream")
			now := service.config.now().UTC().Truncate(time.Microsecond)
			conversationID := conversation.NewConversationID()
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
				if _, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{
					Core:   corestore.Project{OrganizationID: organization.ID, ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native", CreatedAt: now},
					States: nativeFixtureStates(),
				}); err != nil {
					return fmt.Errorf("create project record: %w", err)
				}
				_, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
					OrganizationID:   organization.ID,
					ID:               conversationID,
					ProjectID:        projectID,
					OwnerPrincipalID: "test_hub_admin",
					Origin:           conversationOriginUser,
					Title:            "Example conversation",
					Visibility:       conversation.VisibilityShared,
					Status:           conversation.StatusActive,
					Revision:         1,
					Execution:        conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now},
					Preferences:      conversation.DefaultPreferences(),
					CreatedAt:        now,
					UpdatedAt:        now,
				})
				return err
			}); err != nil {
				t.Fatalf("seed PostgreSQL conversation: %v", err)
			}

			database := service.database
			db, reader, authReader, claimReader, postgres := database.db, database.reader, database.authReader, database.claimReader, database.postgres
			database.db, database.reader, database.authReader, database.claimReader, database.postgres = nil, nil, nil, nil, repository
			t.Cleanup(func() {
				database.db, database.reader, database.authReader, database.claimReader, database.postgres = db, reader, authReader, claimReader, postgres
			})
			server := httptest.NewServer(service.Handler())
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			path := fmt.Sprintf("%s/api/v2/organizations/%s/projects/%s/conversations/%s/events?after=0", server.URL, url.PathEscape(string(organization.ID)), url.PathEscape(string(projectID)), url.PathEscape(conversationID))
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
			transport := &http.Transport{}
			client := &http.Client{Transport: transport}
			t.Cleanup(client.CloseIdleConnections)
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("open PostgreSQL event stream: %v", err)
			}
			t.Cleanup(func() { _ = response.Body.Close() })
			if response.StatusCode != http.StatusOK {
				t.Fatalf("stream status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), cancel: cancel}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
				_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{
					OrganizationID: organization.ID,
					ConversationID: conversationID,
					Event:          conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{"status":"active"}`), CreatedAt: service.config.now().UTC().Truncate(time.Microsecond)},
				})
				return err
			}); err != nil {
				t.Fatalf("append PostgreSQL conversation event: %v", err)
			}
			service.conversations.committed(conversationRecord{ID: conversationID})
			frame, err := readSSEFrame(stream.reader)
			if err != nil {
				t.Fatalf("read PostgreSQL conversation event after HTTP 200: %v", err)
			}
			if frame.ID != "1" || frame.Event != string(conversation.EventConversationUpdated) {
				t.Fatalf("first event frame = %#v, want conversation.updated seq 1", frame)
			}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
				return writer.RevokeAccessCredential(t.Context(), "test_hub_admin", service.config.now().UTC().Truncate(time.Microsecond))
			}); err != nil {
				t.Fatalf("revoke stream credential: %v", err)
			}
			service.conversations.broker.notify(conversationID)
			for {
				frame, err = readSSEFrame(stream.reader)
				if err != nil {
					t.Fatalf("read stream close after PostgreSQL credential revocation: %v", err)
				}
				if frame.Event == string(conversation.EventClosed) {
					break
				}
			}
			requireClosed(t, frame, conversationClosedAccessRevoked)

		})
	}
}

type conversationReplayBarrier struct {
	http.ResponseWriter
	ctx     context.Context
	release <-chan struct{}
	pause   bool
}

func (w *conversationReplayBarrier) Write(data []byte) (int, error) {
	w.pause = strings.HasPrefix(string(data), "id: 1\n")
	return w.ResponseWriter.Write(data)
}

func (w *conversationReplayBarrier) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	if w.pause {
		w.pause = false
		select {
		case <-w.release:
		case <-w.ctx.Done():
		}
	}
}

func TestConversationStreamRefreshesEntryKeyAuthorityPostgres(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name   string
		want   string
		replay bool
	}{
		{"revoked", conversationClosedAccessRevoked, false},
		{"narrowed", conversationClosedAccessRevoked, false},
		{"unavailable", conversationClosedServerError, false},
		{"valid", "", false},
		{"replay_revoked", conversationClosedAccessRevoked, true},
		{"replay_narrowed", conversationClosedAccessRevoked, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var services []*Service
			var streams []*sseStream
			var failed atomic.Bool
			release := make(chan struct{})
			releaseReplay := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseReplay)
			conversations := make(map[string]string)
			organizations := []tracker.OrganizationID{tracker.OrganizationID("org_first_" + tc.name), tracker.OrganizationID("org_second_" + tc.name)}
			for _, organization := range organizations {
				f := newConversationTenantFixture(t, repository, now)
				s := f.service
				s.config.Hosted = &HostedConfig{OrganizationID: string(organization), SharedEntry: &HostedSharedEntry{Issuer: "entry", Generation: 1}}
				claims := cloudassert.Claims{Issuer: "entry", Generation: 1, Audience: string(organization), ProviderOrganization: "provider_" + string(organization), Subject: "owner", Role: "owner", Kind: cloudassert.KindKey, IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), Key: &apikey.KeyAuthority{ID: "key_example", CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "selected", Projects: []string{"project_removed", "project_retained"}}}}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
						return fmt.Errorf("create organization: %w", err)
					}
					if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: claims.ProviderOrganization}); err != nil {
						return fmt.Errorf("create tenant: %w", err)
					}
					for _, id := range []string{"principal_owner", "key_example"} {
						if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: string(organization) + id, Fingerprint: string(organization) + id, Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
							return fmt.Errorf("create credential: %w", err)
						}
					}
					if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "owner", Email: "owner@example.test", MembershipID: "membership_owner", Role: "owner", Active: true, PrincipalID: "principal_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
						return fmt.Errorf("create member: %w", err)
					}
					for _, project := range []tracker.ProjectID{"project_removed", "project_retained"} {
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
							return fmt.Errorf("create project: %w", err)
						}
						if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "key_example", ProjectID: project}); err != nil {
							return fmt.Errorf("grant key project: %w", err)
						}
						if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "owner", ProjectID: project}); err != nil {
							return fmt.Errorf("grant member project: %w", err)
						}
						if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "principal_owner", ProjectID: project}); err != nil {
							return fmt.Errorf("grant principal project: %w", err)
						}
						id := conversation.NewConversationID()
						conversations[string(organization)+string(project)] = id
						if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: "principal_owner", Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now}); err != nil {
							return fmt.Errorf("create conversation: %w", err)
						}
						if _, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: organization, ConversationID: id, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{"ready":true}`), CreatedAt: now}}); err != nil {
							return fmt.Errorf("append ready event: %w", err)
						}
						if tc.replay && organization == organizations[0] && project == "project_removed" {
							if _, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: organization, ConversationID: id, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{"protected":true}`), CreatedAt: now}}); err != nil {
								return fmt.Errorf("append backlog event: %w", err)
							}
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				s.config.Hosted.SharedEntry.ResolveKey = func(ctx context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
					if failed.Load() && organization == organizations[0] {
						return cloudassert.Claims{}, operatortool.ErrServiceUnavailable
					}
					current := claims
					key := *claims.Key
					current.Key = &key
					current.Key.Projects = nil
					err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
						stored, err := r.AccessCredential(ctx, origin.KeyID)
						if err != nil {
							return err
						}
						if stored.RevokedAt != nil {
							return operatortool.ErrAccessDenied
						}
						grants, err := r.AccessTokenGrants(ctx, origin.KeyID, "", 10)
						for _, grant := range grants {
							current.Key.Projects = append(current.Key.Projects, string(grant.ProjectID))
						}
						return err
					})
					return current, err
				}
				s.echo = echo.New()
				s.echo.GET("/projects/:project/conversations/:conversation/events", func(c echo.Context) error {
					if tc.replay && organization == organizations[0] && c.Param("project") == "project_removed" {
						c.Response().Writer = &conversationReplayBarrier{ResponseWriter: c.Response().Writer, ctx: c.Request().Context(), release: release}
					}
					c.Set(hostedSharedClaimsKey, claims)
					credential, _, err := s.authenticateAPIRequest(c)
					if err != nil {
						return err
					}
					c.Set("native_scope", nativeScope{organization: organization, project: tracker.ProjectID(c.Param("project")), credential: credential})
					return s.streamConversationEvents(c)
				})
				server := httptest.NewServer(s.echo)
				t.Cleanup(server.Close)
				client := &http.Client{Transport: &http.Transport{}}
				t.Cleanup(client.CloseIdleConnections)
				for _, project := range []string{"project_removed", "project_retained"} {
					request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/projects/"+project+"/conversations/"+conversations[string(organization)+project]+"/events", nil)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(request.Context())
					t.Cleanup(cancel)
					response, err := client.Do(request.WithContext(ctx))
					if err != nil {
						t.Fatal(err)
					}
					stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), cancel: cancel}
					t.Cleanup(stream.close)
					if response.StatusCode != http.StatusOK {
						t.Fatalf("open stream: %s", response.Status)
					}
					frame := stream.nextEvent(t)
					var initial struct{ Ready bool }
					if err := json.Unmarshal([]byte(frame.Data), &initial); err != nil || frame.ID != "1" || !initial.Ready {
						t.Fatalf("initial replay = %+v", frame)
					}
					streams = append(streams, stream)
				}
				services = append(services, s)
			}
			if err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
				switch tc.name {
				case "revoked", "replay_revoked":
					return w.RevokeAccessCredential(t.Context(), "key_example", now)
				case "narrowed", "replay_narrowed":
					return w.DeleteAccessTokenGrant(t.Context(), "key_example", "project_removed")
				default:
					return nil
				}
			}); err != nil {
				t.Fatal(err)
			}
			failed.Store(tc.name == "unavailable")
			releaseReplay()
			for tenant, organization := range organizations {
				for index, project := range []string{"project_removed", "project_retained"} {
					id := conversations[string(organization)+project]
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: organization, ConversationID: id, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{"protected":true}`), CreatedAt: now}})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					services[tenant].conversations.broker.notify(id)
					stream := streams[tenant*2+index]
					frame := stream.nextEvent(t)
					if tenant == 0 && tc.want != "" && (!strings.HasSuffix(tc.name, "narrowed") || index == 0) {
						requireClosed(t, frame, tc.want)
						if trailing, err := io.ReadAll(stream.response.Body); err != nil || len(trailing) != 0 {
							t.Fatalf("stream did not close cleanly: %q, %v", trailing, err)
						}
					} else {
						var delivered struct{ Protected bool }
						if err := json.Unmarshal([]byte(frame.Data), &delivered); err != nil || frame.ID != "2" || !delivered.Protected {
							t.Fatalf("valid stream tenant=%d project=%s: %+v", tenant, project, frame)
						}
					}
				}
			}
		})
	}
}
