package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardViewReusesPreparedStatement(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repo, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	var firstName string
	var firstPID int
	for call := 1; call <= 2; call++ {
		err := repo.ReadTenant(ctx, "org_example", corestore.Serving, func(reader tenantstore.Reader) error {
			_, err := reader.(interface {
				BoardView(context.Context, viewstore.BoardViewSelection) (viewstore.BoardViewRows, error)
			}).BoardView(ctx, viewstore.BoardViewSelection{ClosedPage: call, LanePreset: viewstore.LanesOpen})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var name string
		var pid, uses int
		if err := pool.QueryRow(ctx, `SELECT pg_backend_pid(), name, generic_plans FROM pg_prepared_statements WHERE statement=$1`, boardviewsql.Statement(false)).Scan(&pid, &name, &uses); err != nil {
			t.Fatal(err)
		}
		if uses != call || (call == 2 && (name != firstName || pid != firstPID)) {
			t.Fatalf("call %d used statement %s on backend %d, executions %d (first %s on %d)", call, name, pid, uses, firstName, firstPID)
		}
		firstName, firstPID = name, pid
	}
}

func TestBoardProjectionPreservesOrderAndSourceFence(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	objects := pgstoretest.NewHistoryObjects()
	content := viewstore.NewContent(objects)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		t.Run(string(org)+"/nested request snapshot", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			nested := make(chan error, 1)
			err := repository.ReadRequest(ctx, org, func(ctx context.Context) error {
				err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					go func() {
						nested <- repository.ReadTenant(ctx, org, corestore.Serving, func(inner tenantstore.Reader) error {
							row, err := inner.AccessOrganization(ctx)
							if err == nil && row.OrganizationID != org {
								return corestore.ErrTenantMismatch
							}
							return err
						})
					}()
					select {
					case err := <-nested:
						return err
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				if ctx.Err() != nil {
					<-nested
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})

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
			record.Display = viewstore.BoardDisplay{ChangePrincipalID: "first", Title: "Example", Number: index + 1, Actor: tracker.Actor{Kind: "human"}, Reason: "Ready", StatusLine: "Running",
				Attempt:   &tracker.NativeBoardAttempt{ID: fmt.Sprintf("attempt_%d", index), Count: 2, Status: "running", RunnerID: "runner_example", MachineID: "machine_example", StartedAt: now, ExpiresAt: now.Add(time.Hour), Identity: &tracker.NativeExecutionIdentity{Role: "implementation", Backend: "example", Model: "example"}},
				Execution: &tracker.NativeBoardExecution{ID: fmt.Sprintf("attempt_%d", index), Running: true, Stage: "implementation", StartedAt: &now, ExpiresAt: now.Add(time.Hour)},
				Change:    &tracker.NativeBoardChange{ID: fmt.Sprintf("change_%d", index), Title: "Example change", Status: "open", External: &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://example.com/change/1"}},
			}
			records = append(records, record)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: record.ProjectID, WorkItemID: id, ExpectedRevision: 1, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
		}
		var initialHead viewstore.BoardSequence
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "prj_example", Limit: 1})
			if err != nil {
				return err
			}
			if len(page) != 1 || page[0].WorkItemID != "wi_example_1" || page[0].OrganizationID != org {
				t.Fatalf("ordered first page=%+v", page)
			}
			if !reflect.DeepEqual(page[0].Display, records[1].Display) {
				t.Fatalf("display roundtrip = %#v, want %#v", page[0].Display, records[1].Display)
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
			if len(deltas) != 3*len(records) {
				t.Fatalf("board deltas=%+v", deltas)
			}
			for i, record := range records {
				notice, action, card := deltas[3*i], deltas[3*i+1], deltas[3*i+2]
				if notice.Topic != "work_item" || notice.Sequence != int64(3*i+1) || notice.EntityID != string(record.WorkItemID) ||
					notice.EntityRevision == nil || *notice.EntityRevision != record.SourceRevision || notice.Previous != nil || notice.Current != nil ||
					card.Topic != "card" || card.Sequence != notice.Sequence+2 || card.WorkItemID != record.WorkItemID || card.Previous != nil || card.Current == nil {
					t.Fatalf("source notice and card do not share the same ordered project stream: notice=%+v card=%+v", notice, card)
				}
				if action.Topic != "human_action" || action.Sequence != notice.Sequence+1 || action.WorkItemID != "" || action.EntityID != string(record.WorkItemID) || action.EntityRevision == nil || *action.EntityRevision != record.SourceRevision || action.Kind != "upsert" || action.Status != "" || action.RelatedID != string(record.WorkItemID) || action.Previous != nil || action.Current != nil {
					t.Fatalf("creation human action notice changed: %+v", action)
				}
			}
			initialHead, err = r.BoardSequence(t.Context(), "prj_example")
			if err == nil && initialHead.Sequence != deltas[len(deltas)-1].Sequence {
				t.Fatalf("committed head=%+v deltas=%+v", initialHead, deltas)
			}
			return err
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
			if err == nil && sequence != initialHead {
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
		var updatedHead viewstore.BoardSequence
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			updatedHead, err = r.BoardSequence(t.Context(), "prj_example")
			if err != nil {
				return err
			}
			notices, err := r.BoardDeltas(t.Context(), "prj_example", initialHead.Sequence, 2)
			if err == nil && (updatedHead.Sequence != initialHead.Sequence+1 || len(notices) != 1 ||
				notices[0].Topic != "work_item" || notices[0].EntityID != "wi_example_0" ||
				notices[0].EntityRevision == nil || *notices[0].EntityRevision != 2) {
				t.Fatalf("source-only title/owner update did not emit one current revision: head=%+v notices=%+v", updatedHead, notices)
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
			if sequence != updatedHead {
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
	t.Run("retention work stays bounded per project flush", func(t *testing.T) {
		organizations := []tracker.OrganizationID{"org_first", "org_second"}
		projects := []tracker.ProjectID{"prj_retention_a", "prj_retention_b", "prj_retention_boundary"}
		type retained struct{ count, min, max, tail, head, floor int64 }
		read := func(org tracker.OrganizationID, project tracker.ProjectID) retained {
			t.Helper()
			var value retained
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*),coalesce(min(d.sequence),0),coalesce(max(d.sequence),0),count(*) FILTER(WHERE d.sequence>s.sequence-2048),s.sequence,s.replay_floor FROM native_board_sequences s LEFT JOIN native_board_deltas d USING(organization_id,project_id) WHERE s.organization_id=$1 AND s.project_id=$2 GROUP BY s.sequence,s.replay_floor`, org, project).Scan(&value.count, &value.min, &value.max, &value.tail, &value.head, &value.floor); err != nil {
				t.Fatal(err)
			}
			return value
		}
		for _, org := range organizations {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, project := range projects {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i, project := range projects {
				head, floor := int64(3000), int64(7)
				if i == 2 {
					head, floor = 2048, 0
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=excluded.sequence,replay_floor=excluded.replay_floor`, org, project, head, floor); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,topic,entity_id,kind) SELECT $1,$2,n,'','activity','retained_'||n,'upsert' FROM generate_series(1,$3::bigint)n`, org, project, head); err != nil {
					t.Fatal(err)
				}
			}
		}
		for orgIndex, org := range organizations {
			untouched := map[tracker.ProjectID]retained{}
			for _, project := range projects {
				untouched[project] = read(organizations[1-orgIndex], project)
			}
			var unrelated viewstore.BoardSequence
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				unrelated, err = r.BoardSequence(t.Context(), "prj_example")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, project := range projects[:2] {
					for i := range 40 {
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("%s_item_%d", project, i)), Number: int64(i + 1), Title: "Example retained item", WorkflowStateName: new("Todo")}); err != nil {
							return err
						}
						if project == projects[0] && i == 19 {
							first := pgstoretest.PolicyTestRevision(org, project, "retention_first", now)
							second := pgstoretest.PolicyTestRevision(org, project, "retention_second", now.Add(time.Second))
							if err := w.ApproveProjectPolicy(t.Context(), first, ""); err != nil {
								return err
							}
							if err := w.ApproveProjectPolicy(t.Context(), second, first.PolicyID); err != nil {
								return err
							}
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i, project := range projects[:2] {
				expected := retained{count: 3048, min: 33, max: 3080, tail: 2048, head: 3080, floor: 32}
				if i == 0 {
					expected.count++
					expected.max++
					expected.head++
					expected.floor = 3041
				}
				if got := read(org, project); got != expected {
					t.Fatalf("retention tenant=%s project=%s got=%+v want=%+v", org, project, got, expected)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					head, err := r.BoardSequence(t.Context(), project)
					if err != nil {
						return err
					}
					if head.Sequence != expected.head || head.ReplayFloor != expected.floor {
						t.Fatalf("retention public watermark=%+v want=%+v", head, expected)
					}
					tail, err := r.BoardDeltas(t.Context(), project, head.ReplayFloor, 1)
					if err != nil {
						return err
					}
					if len(tail) != 1 || tail[0].Sequence != head.ReplayFloor+1 {
						t.Fatalf("inclusive floor replay=%+v head=%+v", tail, head)
					}
					if i == 0 {
						reset, err := r.BoardDeltas(t.Context(), project, 3040, 1)
						if err != nil {
							return err
						}
						if len(reset) != 1 || reset[0].Sequence != 3041 || reset[0].Topic != "board" || reset[0].Kind != "invalidate" || reset[0].EntityID != string(project) || reset[0].Previous != nil || reset[0].Current != nil {
							t.Fatalf("last policy invalidation lost: %+v", reset)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(org, projects[2]); got != (retained{count: 2048, min: 1, max: 2048, tail: 2048, head: 2048, floor: 0}) {
				t.Fatalf("untouched boundary project changed: %+v", got)
			}
			for _, project := range projects {
				if got := read(organizations[1-orgIndex], project); got != untouched[project] {
					t.Fatalf("retention crossed tenant: project=%s got=%+v want=%+v", project, got, untouched[project])
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.BoardSequence(t.Context(), "prj_example")
				if err == nil && got != unrelated {
					t.Fatalf("retention changed unrelated project: got=%+v want=%+v", got, unrelated)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			beforeRollback := read(org, projects[1])
			rollbackTrim := errors.New("rollback retention")
			err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				next, err := w.TrimBoardDeltas(t.Context(), projects[0])
				if err != nil {
					return err
				}
				if next != projects[1] {
					t.Fatalf("rollback trim project=%s want=%s", next, projects[1])
				}
				return rollbackTrim
			})
			if !errors.Is(err, rollbackTrim) || read(org, projects[1]) != beforeRollback {
				t.Fatalf("retention rollback changed rows or replay floor: error=%v before=%+v after=%+v", err, beforeRollback, read(org, projects[1]))
			}
			cursor := tracker.ProjectID("prj_retention_")
			for i, project := range projects {
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					next, err := w.TrimBoardDeltas(t.Context(), cursor)
					if err == nil && next != project {
						t.Fatalf("retention continuation=%s want=%s", next, project)
					}
					cursor = next
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if i < 2 {
					expected := retained{count: 3016, min: 65, max: 3080, tail: 2048, head: 3080, floor: 64}
					if i == 0 {
						expected.count++
						expected.max++
						expected.head++
						expected.floor = 3041
					}
					if got := read(org, project); got != expected {
						t.Fatalf("maintenance continuation=%+v want=%+v", got, expected)
					}
				} else if got := read(org, project); got != (retained{count: 2048, min: 1, max: 2048, tail: 2048, head: 2048, floor: 0}) {
					t.Fatalf("head2048 was trimmed: %+v", got)
				}
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: projects[2], NativeID: "boundary_item", Number: 1, Title: "Boundary item", WorkflowStateName: new("Todo")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := read(org, projects[2]); got != (retained{count: 2048, min: 3, max: 2050, tail: 2048, head: 2050, floor: 2}) {
				t.Fatalf("inclusive head-2048 boundary changed: %+v", got)
			}
		}
	})
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
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	organization := tracker.OrganizationID("org_first")
	pgstoretest.SeedTenantAccess(t, repository, organization)
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
			if _, err := r.AccessProjectGrant(t.Context(), "first", project, accessstore.NoRoleCeiling); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("concurrent grants were not both committed: %v", err)
	}
}
