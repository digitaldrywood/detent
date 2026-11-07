package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresBrowserWorkListConcurrentRequests(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 7)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			service := accountPreferencesPostgresFixture(t, repository).service
			service.config.Hosted.OrganizationID = string(org)
			service.config.Hosted.SharedEntry = &HostedSharedEntry{}
			service.config.SecretKeys = secretTestKeys(t, "1", "1")
			service.database.hostedOrganization = org
			service.config.now = func() time.Time { return now }
			content := eventstore.NewContent(service.config.RunDetails)
			descriptor := hubTestPolicy()
			policy, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = "project_example", "principal", now
			var project projectstore.Record
			claims := cloudassert.Claims{Kind: cloudassert.KindBrowser, Audience: string(org), Subject: "user_example", ProviderOrganization: "provider_" + string(org), ProviderSession: "session_example", SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), AccessExpiresAt: now.Add(time.Hour), Email: "member@example.test", Role: "admin", Binding: "binding_example"}
			service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					c.Set(hostedSharedClaimsKey, claims)
					return next(c)
				}
			})
			envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", Slug: "example-" + string(org), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: claims.ProviderOrganization}); err != nil {
					return err
				}
				var err error
				project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project_example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
				if err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "principal", Name: "Example", Hash: "principal_hash", Scope: "operator", NativeOnly: true, ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: claims.Subject, Email: claims.Email, MembershipID: "membership_example", Role: "admin", Active: true, PrincipalID: "principal", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: org, Hash: claims.Binding, Email: claims.Email, Subject: claims.Subject, ProviderOrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession, IdentityCreatedAt: now, IdentityExpiresAt: claims.SessionExpiresAt, CreatedAt: now, ExpiresAt: claims.SessionExpiresAt}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: claims.Subject, ProjectID: "project_example", CanWrite: true}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "principal", ProjectID: "project_example"}); err != nil {
					return err
				}
				if err := w.ApproveProjectPolicy(t.Context(), policy, ""); err != nil {
					return err
				}
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			for index := range 2 {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
				changeID, versionID := fmt.Sprintf("change_%03d", index), fmt.Sprintf("version_%03d", index)
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project.Core.ID, WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "principal"}, CreatedAt: now, UpdatedAt: now}
				raw, _ := json.Marshal(issue)
				collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project.Core.ID, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ = json.Marshal(tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: project.Core.ID, WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now})
				change, err := content.PrepareChange(t.Context(), org, raw)
				if err != nil {
					t.Fatal(err)
				}
				input := changeTestInput()
				input.PolicyID = descriptor.ID
				raw, _ = json.Marshal(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: descriptor, Actor: issue.Actor, CreatedAt: now})
				version, err := content.PrepareVersion(t.Context(), org, raw)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ = json.Marshal(tracker.ChangeReview{ID: "review_" + changeID, VersionID: versionID, Decision: "approved", Actor: issue.Actor, CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{Run: securityaudit.Run{ID: int64(index + 1)}}})
				evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, raw)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := viewstore.NewContent(service.config.RunDetails).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue, Change: &tracker.NativeBoardChange{ID: changeID, Title: "Example"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project.Core.ID, NativeID: id, Number: int64(index + 1), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
						return err
					}
					if _, err := w.CreateChange(t.Context(), change); err != nil {
						return err
					}
					if _, err := w.CreateVersion(t.Context(), version); err != nil {
						return err
					}
					if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: project.Core.ID, WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
						return err
					}
					if _, err := w.AppendEvidence(t.Context(), evidence); err != nil {
						return err
					}
					board := viewstore.BoardRecord{OrganizationID: org, ProjectID: project.Core.ID, WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.Core.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%03d", index)}, ActorPrincipalID: issue.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now, Payload: payload}
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: project.Core.ID, WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.Core.ContentSettingsRevision, Current: &board})
				}); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 20)
			var workers sync.WaitGroup
			for range 20 {
				workers.Go(func() {
					<-start
					request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v2/organizations/%s/projects/project_example/work-items?include=board", org), nil)
					response := httptest.NewRecorder()
					service.Handler().ServeHTTP(response, request)
					results <- response
				})
			}
			close(start)
			workers.Wait()
			close(results)
			for response := range results {
				if response.Code != http.StatusOK {
					t.Errorf("browser work list status=%d body=%s", response.Code, response.Body.String())
					continue
				}
				var page tracker.NativeIssuePage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Cards) != 2 {
					t.Fatalf("board cards=%d, want 2", len(page.Cards))
				}
				for _, card := range page.Cards {
					if card.Issue.OrganizationID != org || card.Change == nil || card.Change.Status == "" {
						t.Fatalf("incomplete or wrong-tenant card: %+v", card)
					}
				}
			}
		})
	}
}
