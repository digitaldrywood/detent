package hubserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantVersionRoutesWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	service.registerNativeRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), Local: true, CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"project", "hidden", "visible"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				id := tracker.NativeWorkItemID("item_" + string(project))
				state, kind, principal := "Todo", "human", "example-owner"
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: 1, Title: "Current", WorkflowStateName: &state, ActorKind: &kind, ActorPrincipalID: &principal}); err != nil {
					return err
				}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: 1, Revision: 1, Profile: "native"}, Title: string(org) + " historical", State: state, Actor: tracker.Actor{Kind: kind, PrincipalID: principal}, Blockers: []tracker.NativeDependency{{ID: "item_hidden", ProjectID: "hidden"}, {ID: "item_visible", ProjectID: "visible", Title: "Historical dependency"}}, BlockedReason: tracker.DependencyReason{Text: "private reason"}}
				comment := tracker.NativeComment{ID: "comment_" + string(project), OrganizationID: org, ProjectID: project, WorkItemID: id, Revision: 1, Body: string(org) + " original comment"}
				for _, value := range []struct {
					id   string
					body any
				}{{string(id), issue}, {comment.ID, comment}} {
					raw, err := json.Marshal(value.body)
					if err != nil {
						return err
					}
					version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: value.id, Revision: 1}, raw)
					if err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
				}
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "reader", Name: "Example reader", Hash: apikey.HashToken(string(org) + "_token"), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"project", "visible"} {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "reader", ProjectID: project}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			base := "/api/v2/organizations/" + org + "/projects/project/work-items/item_project"
			for _, tc := range []struct {
				name, suffix string
				status       int
			}{
				{"issue", "/versions/1", 200}, {"comment", "/comments/comment_project/versions/1", 200},
				{"missing", "/versions/2", 404}, {"foreign comment", "/comments/comment_hidden/versions/1", 404}, {"invalid", "/versions/0", 422},
			} {
				t.Run(tc.name, func(t *testing.T) {
					response := performHubAPIRequest(t, service, http.MethodGet, base+tc.suffix, org+"_token", nil)
					requireNativeStatus(t, response, tc.status)
					if tc.name == "issue" {
						var issue tracker.NativeIssue
						decodeHubResponse(t, response, &issue)
						if issue.Title != org+" historical" || len(issue.Blockers) != 1 || issue.Blockers[0].ID != "item_visible" || issue.Blockers[0].Title != "Historical dependency" || len(issue.Dependencies) != 1 || issue.Dependencies[0] != "item_visible" || issue.BlockedReason.Text != "" || issue.WebURL != "" {
							t.Fatalf("historical issue visibility: %+v", issue)
						}
					}
					if tc.name == "comment" {
						var comment tracker.NativeComment
						decodeHubResponse(t, response, &comment)
						if comment.Body != org+" original comment" || string(comment.OrganizationID) != org {
							t.Fatalf("historical comment: %+v", comment)
						}
					}
				})
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"/versions/1", map[string]string{"org_first": "org_second_token", "org_second": "org_first_token"}[org], nil), 401)
			credential, _, err := service.authenticateTenantAPIHash(t.Context(), tracker.OrganizationID(org), apikey.HashToken(org+"_token"), "", "")
			if err != nil {
				t.Fatal(err)
			}
			service.config.Hosted = &HostedConfig{OrganizationID: org, PublicURL: "https://example.test"}
			raw, err := service.readVersion(t.Context(), nativeScope{organization: tracker.OrganizationID(org), project: "project", credential: credential}, "item_project", "", 1)
			if err != nil {
				t.Fatal(err)
			}
			var issue tracker.NativeIssue
			if err := json.Unmarshal(raw, &issue); err != nil {
				t.Fatal(err)
			}
			if issue.WebURL != "https://example.test/"+strings.ReplaceAll(org, "_", "-")+"/project/issues/1" {
				t.Fatalf("canonical URL = %q", issue.WebURL)
			}
			credential.HostedRole = "reporter"
			for _, principal := range []string{"example-owner", "another-owner"} {
				credential.HostedPrincipal = principal
				scope := nativeScope{organization: tracker.OrganizationID(org), project: "project", credential: credential}
				for _, comment := range []string{"", "comment_project"} {
					_, err := service.readVersion(t.Context(), scope, "item_project", comment, 1)
					if principal == "example-owner" && err != nil || principal != "example-owner" && !isNativeNotFound(err) {
						t.Fatalf("reporter %s comment %q: %v", principal, comment, err)
					}
				}
			}
			service.config.Hosted = nil
		})
	}
}
