package postgres_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardProjectionPreservesOrderAndSourceFence(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	objects := newHistoryObjects()
	content := viewstore.NewContent(objects)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		records := make([]viewstore.BoardRecord, 0, 2)
		for index := 0; index < 2; index++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%d", index))
			card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{
				NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id, Revision: 1, Number: index + 1},
				Title:           "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "first"}, Type: "bug",
				Verification: &tracker.NativeVerification{State: "pending"}, Labels: []string{fmt.Sprintf("label%d", index)}, CreatedAt: now, UpdatedAt: now,
			}}
			ref, err := content.PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if index == 0 {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}); err != nil {
						return err
					}
				}
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: id, Number: int64(index + 1), Title: "Example", ActorKind: new("human"), ActorPrincipalID: new("first"), WorkflowStateName: new("Todo")})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			record := viewstore.BoardRecord{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id, SourceRevision: 1, Position: viewstore.BoardPosition{Lane: 1, State: "Todo", Priority: 1 - index, Activity: &now, Identifier: fmt.Sprintf("prj_example#%d", index+1)}, ActorPrincipalID: "first", IssueType: "bug", VerificationState: "pending", CreatedAt: now, UpdatedAt: now, Payload: ref, Labels: []string{fmt.Sprintf("label%d", index)}}
			records = append(records, record)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: record.ProjectID, WorkItemID: id, ExpectedRevision: 1, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", Limit: 1})
			if err != nil {
				return err
			}
			if len(page) != 1 || page[0].WorkItemID != "wi_example_1" || page[0].OrganizationID != org {
				t.Fatalf("ordered first page=%+v", page)
			}
			next, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", After: &page[0].Position, Limit: 1})
			if err != nil {
				return err
			}
			if len(next) != 1 || next[0].WorkItemID != "wi_example_0" {
				t.Fatalf("ordered continuation=%+v", next)
			}
			filtered, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", Labels: []string{"label0"}, Limit: 10})
			if err != nil {
				return err
			}
			if len(filtered) != 1 || filtered[0].WorkItemID != "wi_example_0" {
				t.Fatalf("label filter=%+v", filtered)
			}
			selected, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", WorkItemIDs: []tracker.NativeWorkItemID{"wi_example_0"}, Limit: 10})
			if err != nil {
				return err
			}
			if len(selected) != 1 || selected[0].WorkItemID != "wi_example_0" || selected[0].OrganizationID != org {
				t.Fatalf("search candidate filter=%+v", selected)
			}
			lanes, err := r.BoardLanes(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example"}, now)
			if err != nil {
				return err
			}
			if len(lanes) != 1 || lanes[0].Total != 2 || lanes[0].Running != 0 {
				t.Fatalf("lane counts=%+v", lanes)
			}
			for _, selection := range []struct {
				query viewstore.BoardSelection
				total int64
			}{
				{query: viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(false)}, total: 2},
				{query: viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(true)}},
				{query: viewstore.BoardSelection{ProjectID: "prj_example", Labels: []string{"label0"}}, total: 1},
				{query: viewstore.BoardSelection{ProjectID: "prj_example", WorkItemIDs: []tracker.NativeWorkItemID{"wi_example_0"}}, total: 1},
				{query: viewstore.BoardSelection{ProjectID: "prj_example", WorkItemIDs: []tracker.NativeWorkItemID{}}, total: 0},
			} {
				result, err := r.BoardLanes(t.Context(), selection.query, now)
				if err != nil {
					return err
				}
				var total int64
				for _, lane := range result {
					total += lane.Total
				}
				if total != selection.total {
					t.Fatalf("filtered lane total=%d want=%d", total, selection.total)
				}
			}
			deltas, err := r.BoardDeltas(t.Context(), "prj_example", 0, 10)
			if err != nil {
				return err
			}
			if len(deltas) != 2 || deltas[0].Previous != nil || deltas[0].Current == nil || deltas[1].Sequence != 2 {
				t.Fatalf("board deltas=%+v", deltas)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			card, err := content.BoardRecord(t.Context(), record)
			if err != nil || card.Issue.Title != "Example" || card.Issue.WorkItemID != record.WorkItemID {
				t.Fatalf("hydrate board card: %+v, %v", card, err)
			}
			for _, field := range []string{"item", "revision", "actor", "type", "verification", "priority", "labels", "assignees"} {
				invalid := record
				switch field {
				case "item":
					invalid.WorkItemID = "wi_other"
				case "revision":
					invalid.SourceRevision++
				case "actor":
					invalid.ActorPrincipalID = "other"
				case "type":
					invalid.IssueType = "other"
				case "verification":
					invalid.VerificationState = "verified"
				case "priority":
					invalid.Priority = new(1)
				case "labels":
					invalid.Labels = []string{"other"}
				case "assignees":
					invalid.Assignees = []string{"other"}
				}
				if _, err := content.BoardRecord(t.Context(), invalid); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("mismatched %s accepted: %v", field, err)
				}
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ExpectedRevision: 1, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			sequence, err := r.BoardSequence(t.Context(), "prj_example")
			if err == nil && sequence.Sequence != 2 {
				t.Fatalf("unchanged refresh emitted deltas: %+v", sequence)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), "prj_example", "wi_example_0")
			if err != nil {
				return err
			}
			item.Title = "Changed"
			item.ActorPrincipalID = new("second")
			_, err = w.UpdateWorkItem(t.Context(), item)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "prj_example", WorkItemID: "wi_example_0", ExpectedRevision: 1, Current: &records[0]})
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale projection error=%v", err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", Reporter: true, ReporterPrincipalID: "first", Labels: []string{"label0"}, Limit: 10})
			if err == nil && len(page) != 0 {
				t.Fatal("stale board ownership exposed a reassigned issue")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		rollback := errors.New("rollback projection")
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "prj_example", WorkItemID: "wi_example_1", ExpectedRevision: 1}); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			sequence, err := r.BoardSequence(t.Context(), "prj_example")
			if err != nil {
				return err
			}
			if sequence.Sequence != 2 {
				t.Fatalf("rollback retained delta: %+v", sequence)
			}
			_, err = r.BoardItem(t.Context(), "prj_example", "wi_example_1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index, age := range []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 0} {
			closed := now.Add(-age)
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_closed_%d", index))
			card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id, Number: 100 + index, Revision: 1}, Title: "Closed", State: "Done", Terminal: true, Archived: index == 0, ClosedAt: &closed}}
			ref, err := content.PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: id, Number: int64(100 + index), Title: "Closed", Archived: index == 0, WorkflowStateName: new("Done")})
				if err != nil {
					return err
				}
				projection := viewstore.BoardRecord{OrganizationID: org, ProjectID: row.ProjectID, WorkItemID: id, SourceRevision: row.Revision, Position: viewstore.BoardPosition{State: "Done", Identifier: fmt.Sprintf("prj_example#%d", 100+index), ActivityMissing: true}, Terminal: true, Archived: index == 0, ClosedAt: &closed, CreatedAt: now, UpdatedAt: now, Payload: ref}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: id, ExpectedRevision: row.Revision, Current: &projection})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			windows, err := r.BoardClosedWindows(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(false), States: []string{"Todo"}}, []time.Time{now.Add(-time.Hour), now.Add(-24 * time.Hour), now.Add(-7 * 24 * time.Hour)}, now)
			if err != nil {
				return err
			}
			if len(windows) != 3 {
				t.Fatalf("closed windows=%+v", windows)
			}
			for index, window := range windows {
				if window.State != "Done" || window.Window != index || window.Total != int64(index+1) {
					t.Fatalf("closed boundary/archive count=%+v", windows)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

	}
	t.Run("access revisions follow committed grants without crossing tenants", func(t *testing.T) {
		read := func(org tracker.OrganizationID, user string) [3]int64 {
			t.Helper()
			var value [3]int64
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				value, err = r.BoardAccessRevisions(t.Context(), user, user)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return value
		}
		first, second, other := read("org_first", "first"), read("org_second", "first"), read("org_first", "second")
		rollback := errors.New("rollback grant")
		err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_first", UserID: "first", ProjectID: "prj_example"}); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) || read("org_first", "first") != first {
			t.Fatalf("rolled back grant changed access revision: %v", err)
		}
		if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_first", UserID: "first", ProjectID: "prj_example"}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_first", TokenID: "first", ProjectID: "prj_example"})
		}); err != nil {
			t.Fatal(err)
		}
		after := read("org_first", "first")
		if after[0] != first[0] || after[1] <= first[1] || after[2] <= first[2] || read("org_second", "first") != second || read("org_first", "second") != other {
			t.Fatalf("grant revision isolation: before=%v after=%v", first, after)
		}
		if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.DeleteAccessProjectGrant(t.Context(), "first", "prj_example"); err != nil {
				return err
			}
			return w.DeleteAccessTokenGrant(t.Context(), "first", "prj_example")
		}); err != nil {
			t.Fatal(err)
		}
		revoked := read("org_first", "first")
		if revoked[1] <= after[1] || revoked[2] <= after[2] {
			t.Fatalf("revocation did not advance version: before=%v after=%v", after, revoked)
		}
	})

}

func TestConcurrentProjectGrantsDoNotDeadlockRevisionUpdates(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	organization := tracker.OrganizationID("org_first")
	seedTenantAccess(t, repository, organization)
	for _, project := range []tracker.ProjectID{"prj_example", "prj_other"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	var startOnce sync.Once
	continueWorkers := func() { startOnce.Do(func() { close(start) }) }
	t.Cleanup(func() {
		continueWorkers()
		workers.Wait()
	})
	for _, project := range []tracker.ProjectID{"prj_example", "prj_other"} {
		project := project
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.AccessMember(t.Context(), "first"); err != nil {
					return err
				}
				ready <- struct{}{}
				<-start
				return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "first", ProjectID: project})
			})
		}()
	}
	readyCount := 0
	for readyCount < 2 {
		select {
		case <-ready:
			readyCount++
		case err := <-results:
			t.Fatalf("grant writer failed before lock barrier: %v", err)
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	continueWorkers()
	firstErr, secondErr := <-results, <-results
	workers.Wait()
	if firstErr != nil || secondErr != nil {
		t.Fatalf("concurrent grant writes: first=%v second=%v", firstErr, secondErr)
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		for _, project := range []tracker.ProjectID{"prj_example", "prj_other"} {
			if _, err := r.AccessProjectGrant(t.Context(), "first", project); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("concurrent grants were not both committed: %v", err)
	}
}
