package itemread_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantVersionRoutesWithoutLegacyDatabase(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: hubtest.NewDetails()})
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	tokens := make(map[tracker.OrganizationID]string, len(organizations))
	for _, org := range organizations {
		h.EnsureOrganization(t, org)
		seedVersionProjects(t, h, org)
		seedVersionRecords(t, h, org, "example-owner")
		tokens[org] = hubtest.Credential(t, h.Repository, org, hubtest.CredentialOptions{KeyScope: "read", Projects: []tracker.ProjectID{"project", "visible"}}).Token
	}
	for _, org := range organizations {
		t.Run(string(org), func(t *testing.T) {
			base := hubfixture.ProjectBase(org, "project") + "/work-items/item_project"
			response := h.Request(t, http.MethodGet, base+"/versions/1", tokens[org], nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			hubfixture.Decode(t, response, &issue)
			blockers := []tracker.NativeDependency{{ID: "item_visible", ProjectID: "visible", Title: "Historical dependency"}}
			if issue.Title != string(org)+" historical" || issue.OrganizationID != org || !slices.EqualFunc(issue.Blockers, blockers, func(actual, want tracker.NativeDependency) bool {
				return actual.ID == want.ID && actual.ProjectID == want.ProjectID && actual.Title == want.Title
			}) || !slices.Equal(issue.Dependencies, []tracker.NativeWorkItemID{"item_visible"}) || issue.BlockedReason.Text != "" || issue.WebURL != "" {
				t.Fatalf("historical issue changed identity or relationship visibility: %+v", issue)
			}

			response = h.Request(t, http.MethodGet, base+"/comments/comment_project/versions/1", tokens[org], nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var comment tracker.NativeComment
			hubfixture.Decode(t, response, &comment)
			if comment.Body != string(org)+" original comment" || comment.OrganizationID != org {
				t.Fatalf("historical comment = %+v", comment)
			}
		})
	}

	t.Run("denials", func(t *testing.T) {
		base := hubfixture.ProjectBase("org_first", "project") + "/work-items/item_project"
		for _, test := range []struct {
			name, suffix string
			status       int
		}{
			{"missing revision", "/versions/2", http.StatusNotFound},
			{"foreign comment", "/comments/comment_hidden/versions/1", http.StatusNotFound},
			{"invalid revision", "/versions/0", http.StatusUnprocessableEntity},
		} {
			t.Run(test.name, func(t *testing.T) {
				hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, base+test.suffix, tokens["org_first"], nil), test.status)
			})
		}
		hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, base+"/versions/1", tokens["org_second"], nil), http.StatusUnauthorized)
	})

	t.Run("hosted reporter visibility", func(t *testing.T) {
		cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
		cfg.Hosted.PublicURL = "https://example.test"
		cfg.RunDetails = hubtest.NewDetails()
		hosted := hubfixture.OpenHosted(t, cfg)
		hosted.Project = "project"
		seedVersionProjects(t, hosted.Hub, hosted.Primary)
		owner := hosted.User(t, "owner-reporter", "reporter", "owner-reporter@example.test", "read", "")
		other := hosted.User(t, "other-reporter", "reporter", "other-reporter@example.test", "read", "")
		var principal string
		if err := hosted.Repository.ReadTenant(t.Context(), hosted.Primary, corestore.Serving, func(r tenantstore.Reader) error {
			member, err := r.AccessMember(t.Context(), owner.Identity.Subject)
			principal = member.PrincipalID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		seedVersionRecords(t, hosted.Hub, hosted.Primary, principal)
		base := hubfixture.ProjectBase(hosted.Primary, "project") + "/work-items/item_project"
		response := hosted.Request(t, owner, http.MethodGet, base+"/versions/1", nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var version tracker.NativeIssue
		hubfixture.Decode(t, response, &version)
		if version.WebURL != "https://example.test/local-organization/project/issues/1" {
			t.Fatalf("canonical URL = %q", version.WebURL)
		}
		hubfixture.RequireStatus(t, hosted.Request(t, owner, http.MethodGet, base+"/comments/comment_project/versions/1", nil), http.StatusOK)
		for _, suffix := range []string{"/versions/1", "/comments/comment_project/versions/1"} {
			hubfixture.RequireStatus(t, hosted.Request(t, other, http.MethodGet, base+suffix, nil), http.StatusNotFound)
		}
	})
}

func seedVersionProjects(t *testing.T, h *hubfixture.Hub, org tracker.OrganizationID) {
	t.Helper()
	if err := h.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, project := range []tracker.ProjectID{"project", "hidden", "visible"} {
			record := projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: string(project), Profile: "native"}, States: hubtest.States()}
			if _, err := w.CreateProjectRecord(t.Context(), record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func seedVersionRecords(t *testing.T, h *hubfixture.Hub, org tracker.OrganizationID, principal string) {
	t.Helper()
	content := eventstore.NewContent(h.Config.RunDetails)
	if err := h.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, project := range []tracker.ProjectID{"project", "hidden", "visible"} {
			item := tracker.NativeWorkItemID("item_" + string(project))
			state, kind := "Todo", "human"
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: 1, Title: "Current", WorkflowStateName: &state, ActorKind: &kind, ActorPrincipalID: &principal}); err != nil {
				return err
			}
			issue := tracker.NativeIssue{
				NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 1, Revision: 1, Profile: "native"},
				Title:           string(org) + " historical", State: state, Actor: tracker.Actor{Kind: kind, PrincipalID: principal},
				Blockers:      []tracker.NativeDependency{{ID: "item_hidden", ProjectID: "hidden"}, {ID: "item_visible", ProjectID: "visible", Title: "Historical dependency"}},
				BlockedReason: tracker.DependencyReason{Text: "private reason"},
			}
			comment := tracker.NativeComment{ID: "comment_" + string(project), OrganizationID: org, ProjectID: project, WorkItemID: item, Revision: 1, Body: string(org) + " original comment"}
			for _, value := range []struct {
				id   string
				body any
			}{{string(item), issue}, {comment.ID, comment}} {
				raw, err := json.Marshal(value.body)
				if err != nil {
					return err
				}
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: value.id, Revision: 1}, raw)
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
