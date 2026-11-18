package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardModelRechecksCurrentRelationshipAuthority(t *testing.T) {
	t.Parallel()
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
			scope := hubscope.NativeScope{Organization: org, Project: source.project.ID, Credential: credential}
			now := service.config.Clock().UTC().Truncate(time.Microsecond)
			sourceID, sameID, crossID := tracker.NativeWorkItemID("wi_source"), tracker.NativeWorkItemID("wi_same"), tracker.NativeWorkItemID("wi_cross")
			links := []tracker.NativeDependency{
				{ID: sameID, ProjectID: source.project.ID, Number: 2, Title: "Same project target", State: "Todo"},
				{ID: crossID, ProjectID: target.project.ID, Number: 1, Title: "Private target " + string(org), State: "Todo"},
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: source.project.ID, WorkItemID: sourceID, Number: 1, Revision: 1, Profile: "native"}, Title: "Source " + string(org), Body: "Public source body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{sameID, crossID}, Blockers: links, Dependents: slices.Clone(links), Related: slices.Clone(links), DependencyReady: true, BlockedReason: tracker.DependencyReason{Text: "private dependency reason"}, UnblocksReason: tracker.DependencyReason{Text: "private unblock reason"}}
			issue.Change = &tracker.NativeIssueChange{ChangeID: "change_shared", HeadSHA: "private-source-head"}
			issue.ListChange = &tracker.NativeListChange{HeadSHA: "private-list-head", Branch: "private-list-branch"}
			issue.PullRequest = &tracker.NativePullRequest{Number: 42, URL: "https://example.test/pull/42"}
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
			check := func(label string, scope hubscope.NativeScope, want []tracker.NativeWorkItemID, wantChange bool) {
				t.Helper()
				for _, include := range []string{"", "board", "work"} {
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
						if !wantChange && (got.Change != nil || got.ListChange != nil || got.PullRequest != nil) {
							t.Errorf("%s/%s exposed hidden change fields: change=%+v list=%+v PR=%+v", label, include, got.Change, got.ListChange, got.PullRequest)
						}
						if wantChange && (got.ListChange == nil || got.ListChange.HeadSHA != "private-list-head" || got.ListChange.Branch != "private-list-branch" || got.PullRequest == nil || got.PullRequest.Number != 42 || got.PullRequest.URL != "https://example.test/pull/42") {
							t.Errorf("%s/%s lost permitted change fields: list=%+v PR=%+v", label, include, got.ListChange, got.PullRequest)
						}
						if wantChange && include == "board" && (got.Change == nil || got.Change.ChangeID != "change_shared" || got.Change.HeadSHA != "private-source-head") {
							t.Errorf("%s/%s lost permitted Change: %+v", label, include, got.Change)
						}
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
						if got.DependencyReady != (len(want) == 0) {
							t.Errorf("%s/%s readiness=%v not computed from visible blockers %v", label, include, got.DependencyReady, want)
						}
						if len(want) < len(links) && (got.BlockedReason.Text != "" || got.UnblocksReason.Text != "" || slices.Contains(got.OmittedFields, "blocked_reason") || slices.Contains(got.OmittedFields, "unblocks_reason")) {
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
			reporter.Credential.HostedRole, reporter.Credential.HostedPrincipal = "reporter", "owner"
			check("reporter owns target", reporter, []tracker.NativeWorkItemID{sameID}, true)
			for _, test := range []struct {
				principal string
				visible   bool
			}{{"another-owner", false}, {"owner", true}} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					row, err := w.BoardItem(t.Context(), source.project.ID, sourceID)
					if err != nil {
						return err
					}
					row.Display.ChangePrincipalID = test.principal
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: source.project.ID, WorkItemID: sourceID, ExpectedRevision: row.SourceRevision, ExpectedEventSequence: row.SourceEventSequence, ExpectedProjectRevision: row.SourceProjectRevision, Current: &row})
				}); err != nil {
					t.Fatal(err)
				}
				check("stored Change owner "+test.principal, reporter, []tracker.NativeWorkItemID{sameID}, test.visible)
				check("operator stored Change owner "+test.principal, scope, []tracker.NativeWorkItemID{sameID}, true)
			}
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

func TestBoardModelHiddenBlockerLeavesNoTrace(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	repository := service.config.TenantRepository
	org := tracker.OrganizationID("org_first")
	source := newNativeFixture(t, service, org, "Example source")
	target := newNativeFixture(t, service, org, "Example target")
	partial, status, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(source.token), "", "")
	if err != nil {
		t.Fatalf("partial credential status=%d: %v", status, err)
	}
	full, status, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(target.token), "", "")
	if err != nil {
		t.Fatalf("full credential status=%d: %v", status, err)
	}
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	same := tracker.NativeDependency{ID: "wi_same", ProjectID: source.project.ID, Number: 5, Title: "Same project blocker", State: "Todo"}
	hidden := tracker.NativeDependency{ID: "wi_hidden", ProjectID: target.project.ID, Number: 1, Title: "Private target blocker", State: "Todo"}
	type item struct {
		id     tracker.NativeWorkItemID
		number int
		links  []tracker.NativeDependency
		basis  []string
	}
	pairs := []struct {
		name          string
		with, without item
		ready         bool
	}{
		{"visible blocker", item{"wi_with_visible", 1, []tracker.NativeDependency{same, hidden}, []string{"wi_same"}}, item{"wi_without_visible", 2, []tracker.NativeDependency{same}, []string{"wi_same"}}, false},
		{"only hidden blocker", item{"wi_with_hidden", 3, []tracker.NativeDependency{hidden}, nil}, item{"wi_without_any", 4, nil, nil}, true},
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: full.ID, ProjectID: source.project.ID}); err != nil {
			return err
		}
		for _, link := range []tracker.NativeDependency{same, hidden} {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: link.ProjectID, NativeID: link.ID, Number: int64(link.Number), Title: link.Title, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")}); err != nil {
				return err
			}
		}
		project, err := w.Project(t.Context(), source.project.ID)
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			for _, entry := range []item{pair.with, pair.without} {
				stored, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: source.project.ID, NativeID: entry.id, Number: int64(entry.number), Title: "Example item", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")})
				if err != nil {
					return err
				}
				ids := make([]tracker.NativeWorkItemID, len(entry.links))
				for i, link := range entry.links {
					ids[i] = link.ID
				}
				reason := tracker.DependencyReason{Text: "Waiting on visible work", Basis: workitemread.DependencyBasis(slices.Clone(entry.basis)), Revision: 1}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: source.project.ID, WorkItemID: entry.id, Number: entry.number, Revision: 1, Profile: "native"}, Title: "Example item", Body: "Example body", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: ids, Blockers: slices.Clone(entry.links), Dependents: slices.Clone(entry.links), Related: slices.Clone(entry.links), DependencyReady: len(entry.links) == 0, BlockedReason: reason, UnblocksReason: reason}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: source.project.ID, WorkItemID: entry.id, SourceRevision: stored.Revision, SourceEventSequence: stored.EventSequence, SourceProjectRevision: project.ContentSettingsRevision,
					Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%d", source.project.ID, entry.number)}, Dispatchable: true, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 4096, ProjectedAt: now,
					ReporterRelations: &viewstore.BoardReporterRelations{ReporterStatus: "reported", Dependencies: slices.Clone(ids), Blockers: slices.Clone(entry.links), Dependents: slices.Clone(entry.links), Related: slices.Clone(entry.links), DependencyReady: issue.DependencyReady, BlockedReason: reason, UnblocksReason: reason},
					Display:           viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: entry.number, Actor: issue.Actor}}
				if err := w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: source.project.ID, WorkItemID: entry.id, ExpectedRevision: stored.Revision, ExpectedEventSequence: stored.EventSequence, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &row}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	normalized := func(t *testing.T, issue tracker.NativeIssue, card *tracker.NativeBoardCard) string {
		t.Helper()
		issue.WorkItemID, issue.Number, issue.WebURL = "", 0, ""
		var value any = issue
		if card != nil {
			copied := *card
			copied.Issue = issue
			value = copied
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, include := range []string{"", "board", "work"} {
		t.Run("include "+include, func(t *testing.T) {
			read := func(t *testing.T, credential hubapi.APICredential) (tracker.NativeIssuePage, map[tracker.NativeWorkItemID]tracker.NativeIssue, map[tracker.NativeWorkItemID]tracker.NativeBoardCard) {
				t.Helper()
				page, err := service.readBoardRecords(t.Context(), hubscope.NativeScope{Organization: org, Project: source.project.ID, Credential: credential}, url.Values{"include": {include}, "limit": {"10"}})
				if err != nil {
					t.Fatal(err)
				}
				items, cards := map[tracker.NativeWorkItemID]tracker.NativeIssue{}, map[tracker.NativeWorkItemID]tracker.NativeBoardCard{}
				all := slices.Clone(page.Items)
				if page.Work != nil {
					all = append(all, page.Work.Items...)
				}
				for _, issue := range all {
					items[issue.WorkItemID] = issue
				}
				for _, card := range page.Cards {
					cards[card.Issue.WorkItemID] = card
				}
				return page, items, cards
			}
			page, items, cards := read(t, partial)
			raw, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{string(hidden.ID), string(target.project.ID), hidden.Title} {
				if strings.Contains(string(raw), secret) {
					t.Errorf("page leaked hidden blocker %q", secret)
				}
			}
			for _, pair := range pairs {
				with, okWith := items[pair.with.id]
				without, okWithout := items[pair.without.id]
				if !okWith || !okWithout {
					t.Fatalf("%s: page items=%v", pair.name, slices.Collect(maps.Keys(items)))
				}
				if with.DependencyReady != pair.ready || without.DependencyReady != pair.ready {
					t.Errorf("%s readiness with=%v without=%v want %v", pair.name, with.DependencyReady, without.DependencyReady, pair.ready)
				}
				if with.BlockedReason.Text == "" || with.BlockedReason.CurrentBasis == "" {
					t.Errorf("%s visible reason withheld: %+v", pair.name, with.BlockedReason)
				}
				var withCard, withoutCard *tracker.NativeBoardCard
				if include == "board" {
					first, second := cards[pair.with.id], cards[pair.without.id]
					withCard, withoutCard = &first, &second
				}
				if got, want := normalized(t, with, withCard), normalized(t, without, withoutCard); got != want {
					t.Errorf("%s: hidden blocker changed the response\nwith:    %s\nwithout: %s", pair.name, got, want)
				}
			}
			_, items, _ = read(t, full)
			got := items[pairs[0].with.id]
			if !slices.ContainsFunc(got.Blockers, func(link tracker.NativeDependency) bool { return link.ID == hidden.ID }) || got.DependencyReady || got.BlockedReason.Text != "" {
				t.Errorf("full viewer lost the cross-project blocker or its reason rule: %+v", got)
			}
		})
	}
}
