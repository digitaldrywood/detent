package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOperatorItemReferencesUseCurrentAccessWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, Logger: discardLogger(), now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		credential := apiCredential{ID: "reader", Hash: apikey.HashToken(string(org) + "_reader"), Scope: apiScopeOperator}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential.ID, Hash: credential.Hash, Name: "Example reader", Scope: "operator", KeyScope: "read", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"example", "hidden"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example " + string(project), Slug: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: "example"})
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"example", "hidden"} {
			for number := 1; number <= 2; number++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", project, number))
				actor := "reader"
				if number == 2 {
					actor = "other"
				}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: number, Revision: 1}, Title: string(org) + " example", Body: "Example body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(number), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
						return err
					}
					_, err := w.CreateCollaborationVersion(t.Context(), version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		scope := nativeScope{organization: org, project: "example", credential: credential}
		for _, test := range []struct {
			name, reference, role string
			want                  error
		}{
			{"number", "1", "", nil},
			{"project number", "example#1", "", nil},
			{"native id", "wi_example_1", "", nil},
			{"readable path", "/" + string(org) + "/example/issues/1", "", nil},
			{"reporter own", "1", "reporter", nil},
			{"reporter private", "2", "reporter", explain.ErrNotFound},
			{"missing", "99", "", explain.ErrNotFound},
			{"invalid", "invalid", "", explain.ErrNotFound},
			{"wrong prefix", "hidden#1", "", explain.ErrNotFound},
			{"unauthorized project", "wi_hidden_1", "", operatortool.ErrProjectScopeRequired},
			{"reporter hidden", "wi_hidden_1", "reporter", explain.ErrNotFound},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("operator read panicked with nil legacy database: %v", value)
					}
				}()
				current := scope
				current.credential.HostedRole = test.role
				item, err := service.resolveOperatorNativeReadItem(t.Context(), nil, current, test.reference)
				if err != nil {
					err = safeWorkReadError(err)
				}
				if test.want != nil {
					if !errors.Is(err, test.want) {
						t.Fatalf("error=%v, want %v", err, test.want)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if item.OrganizationID != org || item.ProjectID != "example" || item.WorkItemID != "wi_example_1" || item.Body != "Example body" || item.Title != string(org)+" example" || item.Revision != 1 {
					t.Fatalf("wrong current item identity or content")
				}
			})
		}
		identity := operatorIdentity(credential, string(org))
		ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
			return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, req operatortool.Requirement) error {
				return repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error { return service.authorizeAccessScope(ctx, r, scope, false) })
			}}, nil
		}})
		for _, tool := range []string{operatortool.WorkItem, operatortool.WorkExport} {
			t.Run(string(org)+"/"+tool, func(t *testing.T) {
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("operator tool panicked: %v", value)
					}
				}()
				result, err := (operatorWorkReads{service: service, scope: scope}).ReadWork(ctx, tool, operatortool.WorkReadRequest{ProjectID: "example", Reference: strconv.Itoa(1), Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				var response operatortool.WorkReadResult[operatortool.NativeItem]
				if err := json.Unmarshal(result.Content, &response); err != nil {
					t.Fatal(err)
				}
				if response.ProjectID != "example" || response.Reference != "wi_example_1" || response.Freshness != explain.SourceAvailable || response.Data.OrganizationID != org || response.Data.ProjectID != "example" || response.Data.WorkItemID != "wi_example_1" || response.Data.Number != 1 || response.Data.Revision != 1 || response.Data.Title != string(org)+" example" || response.Data.Body != "Example body" {
					t.Fatal("operator response lost current item identity or body")
				}
				if tool == operatortool.WorkItem && response.Data.Identifier != "example#1" {
					t.Fatal("work item identifier is missing")
				}
			})
		}

		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: "hidden"})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "wi_hidden_1"); !isNativeNotFound(err) {
			t.Fatalf("wrong authorized project error=%v", err)
		}
		updated := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example_1", Number: 1, Revision: 2}, Title: "Current title", Body: "Current body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "reader"}, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(updated)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: updated.WorkItemID, RecordID: string(updated.WorkItemID), Revision: 2}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), "example", updated.WorkItemID)
			if err != nil {
				return err
			}
			row.Title = updated.Title
			if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		current, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "1")
		if err != nil {
			t.Fatal(err)
		}
		if current.Revision != 2 || current.Title != "Current title" || current.Body != "Current body" {
			t.Fatal("operator resolver read stale revision")
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessTokenGrant(t.Context(), credential.ID, "example")
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/revoked grant", func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("revoked read panicked: %v", value)
				}
			}()
			if _, err := service.resolveOperatorNativeReadItem(t.Context(), nil, scope, "1"); err == nil {
				t.Fatal("revoked project grant still reads item")
			}
		})
	}
}
