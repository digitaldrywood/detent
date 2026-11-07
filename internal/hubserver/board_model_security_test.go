package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardModelRechecksCurrentRelationshipAuthority(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	repository := service.config.TenantRepository
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			details.beforeRead = nil
			source := newNativeFixture(t, service, org, "Example source")
			target := newNativeFixture(t, service, org, "Example target")
			credential, status, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(source.token), "", "")
			if err != nil {
				t.Fatalf("credential status=%d: %v", status, err)
			}
			scope := nativeScope{organization: org, project: source.project.ID, credential: credential}
			now := service.config.now().UTC().Truncate(time.Microsecond)
			sourceID, sameID, crossID := tracker.NativeWorkItemID("wi_source"), tracker.NativeWorkItemID("wi_same"), tracker.NativeWorkItemID("wi_cross")
			links := []tracker.NativeDependency{
				{ID: sameID, ProjectID: source.project.ID, Number: 2, Title: "Same project target", State: "Todo"},
				{ID: crossID, ProjectID: target.project.ID, Number: 1, Title: "Private target " + string(org), State: "Todo"},
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: source.project.ID, WorkItemID: sourceID, Number: 1, Revision: 1, Profile: "native"}, Title: "Source " + string(org), Body: "Public source body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{sameID, crossID}, Blockers: links, Dependents: slices.Clone(links), Related: slices.Clone(links), DependencyReady: true, BlockedReason: tracker.DependencyReason{Text: "private dependency reason"}, UnblocksReason: tracker.DependencyReason{Text: "private unblock reason"}}
			payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			change := tracker.ChangeRequest{ID: "change_shared", OrganizationID: org, ProjectID: source.project.ID, WorkItemID: sameID, LinkedIssues: []tracker.NativeWorkItemID{sameID, sourceID}, Title: "Shared change", Revision: 1, CreatedAt: now, UpdatedAt: now}
			changeRaw, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			changeRow, err := eventstore.NewContent(details).PrepareChange(t.Context(), org, changeRaw)
			if err != nil {
				t.Fatal(err)
			}
			createSource := func(w tenantstore.Writer) (corestore.WorkItem, error) {
				return w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: source.project.ID, NativeID: sourceID, Number: 1, Title: issue.Title, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")})
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := createSource(w)
				if err != nil {
					return err
				}
				for _, link := range links {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: link.ProjectID, NativeID: link.ID, Number: int64(link.Number), Title: link.Title, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")}); err != nil {
						return err
					}
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return err
				}
				project, err := w.Project(t.Context(), source.project.ID)
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: source.project.ID, WorkItemID: sourceID, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision,
					Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#1", source.project.ID)}, Dispatchable: true, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 4096, ProjectedAt: now,
					ReporterRelations: &viewstore.BoardReporterRelations{ReporterStatus: "reported", Dependencies: slices.Clone(issue.Dependencies), Blockers: slices.Clone(links), Dependents: slices.Clone(links), Related: slices.Clone(links), DependencyReady: true, BlockedReason: issue.BlockedReason, UnblocksReason: issue.UnblocksReason},
					Display:           viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: 1, Actor: issue.Actor, ChangePrincipalID: "owner", Change: &tracker.NativeBoardChange{ID: change.ID, Title: change.Title, Status: "open"}}}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: source.project.ID, WorkItemID: sourceID, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &row})
			})
			if err != nil {
				t.Fatal(err)
			}
			details.beforeRead = func(context.Context) error { return errors.New("serving read accessed an object") }
			check := func(label string, scope nativeScope, want []tracker.NativeWorkItemID, wantChange bool) {
				t.Helper()
				for _, include := range []string{"board", "work"} {
					page, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {include}, "limit": {"10"}})
					if err != nil {
						t.Fatalf("%s/%s: %v", label, include, err)
					}
					if len(page.Items) != 1 || page.Items[0].OrganizationID != org || page.Items[0].WorkItemID != sourceID {
						t.Fatalf("%s/%s source page=%+v", label, include, page.Items)
					}
					if include == "board" && (len(page.Cards) != 1 || (page.Cards[0].Change != nil) != wantChange) {
						t.Errorf("%s current Change visibility=%+v want visible=%v", label, page.Cards, wantChange)
					}
					items := slices.Clone(page.Items)
					if page.Work != nil {
						items = append(items, page.Work.Items...)
					}
					for _, got := range items {
						if !slices.Equal(got.Dependencies, want) {
							t.Errorf("%s/%s dependencies=%v want=%v", label, include, got.Dependencies, want)
						}
						for _, actual := range [][]tracker.NativeDependency{got.Blockers, got.Dependents, got.Related} {
							ids := make([]tracker.NativeWorkItemID, len(actual))
							for i, link := range actual {
								ids[i] = link.ID
							}
							if !slices.Equal(ids, want) {
								t.Errorf("%s/%s links=%v want=%v", label, include, ids, want)
							}
						}
						if len(want) < len(links) && (got.DependencyReady || got.BlockedReason.Text != "" || got.UnblocksReason.Text != "") {
							t.Errorf("%s/%s hidden relationship exposed derived facts", label, include)
						}
					}
					if !slices.Contains(want, crossID) {
						raw, err := json.Marshal(page)
						if err != nil {
							t.Fatal(err)
						}
						for _, hidden := range []string{string(crossID), string(target.project.ID), links[1].Title, "private dependency reason", "private unblock reason"} {
							if strings.Contains(string(raw), hidden) {
								t.Errorf("%s/%s leaked hidden relationship %q", label, include, hidden)
							}
						}
					}
				}
			}
			check("source only", scope, []tracker.NativeWorkItemID{sameID}, true)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: target.project.ID})
			}); err != nil {
				t.Fatal(err)
			}
			check("target granted", scope, []tracker.NativeWorkItemID{sameID, crossID}, true)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), credential.ID, target.project.ID)
			}); err != nil {
				t.Fatal(err)
			}
			check("target revoked", scope, []tracker.NativeWorkItemID{sameID}, true)
			reporter := scope
			reporter.credential.HostedRole, reporter.credential.HostedPrincipal = "reporter", "owner"
			check("reporter owns target", reporter, []tracker.NativeWorkItemID{sameID}, true)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(t.Context(), source.project.ID, sameID)
				if err != nil {
					return err
				}
				item.ActorPrincipalID = new("another-owner")
				_, err = w.UpdateWorkItem(t.Context(), item)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check("reporter target reassigned", reporter, []tracker.NativeWorkItemID{}, false)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.DeleteWorkItem(t.Context(), source.project.ID, sourceID)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {"board"}}); !errors.Is(err, accessstore.ErrDenied) {
				t.Errorf("deleted source error=%v, want denied", err)
			}
			movedID := tracker.NativeWorkItemID("wi_source_elsewhere")
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: target.project.ID, NativeID: movedID, Number: 99, Title: "Example", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: target.project.ID})
			}); err != nil {
				t.Fatal(err)
			}
			raw, ok := projectRecordDatabases.Load(repository)
			if !ok {
				t.Fatal("missing isolated fixture database")
			}
			result, err := raw.(*sql.DB).ExecContext(t.Context(), "UPDATE native_board_items SET work_item_id=$3 WHERE organization_id=$1 AND project_id=$4 AND work_item_id=$2", org, sourceID, movedID, source.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("stale projection rekey count=%d error=%v", count, err)
			}
			if _, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {"board"}}); !errors.Is(err, accessstore.ErrDenied) {
				t.Errorf("moved source error=%v, want denied", err)
			}
		})
	}
}
