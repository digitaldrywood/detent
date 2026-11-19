package boardprojection_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardProjectionBoundsReporterRelationshipsAcrossTenants(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	content := viewstore.NewContent(hubtest.NewDetails())
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: org, Now: now})
		projectFixture := hubtest.Project(t, repository, org, hubtest.ProjectOptions{Name: "Example bounded projection"})
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: projectFixture.ID, WorkItemID: "wi_bounds", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, DependencyReady: true,
			Provenance: &tracker.Provenance{Provider: "external", ExternalID: "example", AuthorID: "owner", ObservedAt: now}, BlockedReason: tracker.DependencyReason{Text: strings.Repeat("reason", 4096)}}
		for index := range 20 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_relation_%02d", index))
			issue.Dependencies = append(issue.Dependencies, id)
			issue.Blockers = append(issue.Blockers, tracker.NativeDependency{ID: id, ProjectID: projectFixture.ID, Number: index + 2, Title: strings.Repeat("x", 8*1024), State: "Todo"})
		}
		issue.Dependents, issue.Related = slices.Clone(issue.Blockers), slices.Clone(issue.Blockers)
		reporter := viewstore.BoardReporterRelations{ReporterStatus: "reported", Dependencies: slices.Clone(issue.Dependencies), Blockers: slices.Clone(issue.Blockers), Dependents: slices.Clone(issue.Dependents), Related: slices.Clone(issue.Related), DependencyReady: true, BlockedReason: issue.BlockedReason}
		board, list, boundedReporter, listBytes, err := boardview.BoundProjection(issue, reporter)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"blockers", "dependents", "related"} {
			if !slices.Contains(board.OmittedFields, field) {
				t.Errorf("large %s was not marked omitted", field)
			}
		}
		if !reflect.DeepEqual(board.Provenance, issue.Provenance) {
			t.Error("bounding removed board provenance")
		}
		payload, err := content.PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: board})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: projectFixture.ID, NativeID: issue.WorkItemID, Number: 1, Title: issue.Title, WorkflowStateName: &issue.State, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID})
			if err != nil {
				return err
			}
			project, err := w.Project(t.Context(), projectFixture.ID)
			if err != nil {
				return err
			}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: projectFixture.ID, WorkItemID: issue.WorkItemID, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision,
				Position: viewstore.BoardPosition{State: "Todo", Identifier: "example#1"}, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &board, ListIssue: &list, ReporterRelations: &boundedReporter, ListBytes: listBytes, ProjectedAt: now,
				Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: 1, Actor: issue.Actor}}
			return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &row})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.BoardItem(t.Context(), projectFixture.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			for _, role := range []string{"", "reporter"} {
				scope := hubscope.NativeScope{Organization: org, Project: projectFixture.ID, Credential: hubapi.APICredential{HostedRole: role, HostedPrincipal: "owner"}}
				card, err := boardview.ModelCard(scope, row)
				if err != nil {
					return err
				}
				if card.Issue.OrganizationID != org || card.Issue.ProjectID != projectFixture.ID || card.Issue.WorkItemID != issue.WorkItemID || !reflect.DeepEqual(card.Issue.Provenance, issue.Provenance) {
					t.Fatalf("projection lost tenant, item, or provenance: %+v", card.Issue)
				}
				if role == "reporter" {
					if len(card.Issue.Blockers)+len(card.Issue.Dependents)+len(card.Issue.Related) != 0 {
						t.Error("reporter restored omitted relationship arrays")
					}
					for _, field := range []string{"blockers", "dependents", "related"} {
						if !slices.Contains(card.Issue.OmittedFields, field) {
							t.Errorf("reporter lost omitted marker %s", field)
						}
					}
				}

			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
