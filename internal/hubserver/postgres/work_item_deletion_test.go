package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemDeletionRetainsHistoryAndReceipts(t *testing.T) {
	var baseline [2]int64
	for _, size := range []int{2, 50} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			admin := template.Database(t)
			repository, pool := repositoryFor(t, admin, smallPool())
			fixtures := map[tracker.OrganizationID]historyFixture{}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedRepository(t, repository, org)
				fixtures[org] = seedHistory(t, repository, org, "retained")
				for i := 1; i < size; i++ {
					event := historyEvent(t, fixtures[org].content, org, fmt.Sprintf("history_%d", i))
					if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
						t.Fatal(err)
					}
				}
			}
			repository, counts := attemptCountedRepository(t, repository, pool)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				f := fixtures[org]
				counts.statements.Store(0)
				counts.exchanges.Store(0)
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", "wi_first_1") })
				if err != nil {
					t.Fatal(err)
				}
				cost := [2]int64{counts.statements.Load(), counts.exchanges.Load()}
				if baseline == [2]int64{} {
					baseline = cost
				} else if cost != baseline {
					t.Fatalf("deletion cost grew: baseline=%v cost=%v", baseline, cost)
				}
				t.Logf("tenant=%s history=%d statements=%d exchanges=%d", org, size, cost[0], cost[1])
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					if _, err := r.WorkItem(t.Context(), "prj_first", "wi_first_1"); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("deleted work item=%v", err)
					}
					events, err := r.Events(t.Context(), "prj_first", "wi_first_1", 0, 100)
					if err != nil {
						return err
					}
					if len(events) != size {
						t.Fatalf("retained events=%d want=%d", len(events), size)
					}
					snapshot, err := r.CollaborationVersion(t.Context(), f.snapshot.RecordID, 2)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(snapshot, f.snapshot) {
						t.Fatal("snapshot changed during deletion")
					}
					receipt, err := r.Receipt(t.Context(), f.receipt.ActorID, f.receipt.Operation, f.receipt.CommandKey)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(receipt, f.receipt) {
						t.Fatal("receipt changed during deletion")
					}
					change, err := r.Change(t.Context(), f.change.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(change, f.change) {
						t.Fatal("change changed during deletion")
					}
					versions, err := r.Versions(t.Context(), f.change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(versions, []eventstore.Version{f.version}) {
						t.Fatal("version changed during deletion")
					}
					evidence, err := r.Evidence(t.Context(), f.change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(evidence, []eventstore.Evidence{f.evidence}) {
						t.Fatal("evidence changed during deletion")
					}
					comments, err := r.Comments(t.Context(), "prj_first", "wi_first_1", 0, 10)
					if err != nil {
						return err
					}
					if len(comments) != 0 {
						t.Fatal("live comments survived deletion")
					}
					_, err = r.WorkItem(t.Context(), "prj_first", "wi_first_2")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				event := historyEvent(t, f.content, org, "after_delete")
				err = repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err })
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("late event=%v", err)
				}
				_, err = admin.ExecContext(t.Context(), `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT organization_id,project_id,work_item_id,'late_event',999,'issue.edited',1,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 LIMIT 1`, org)
				requireSQLState(t, err, "23503")
				err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: "wi_first_1", Number: 3, Title: "Example"})
					return err
				})
				requireSQLState(t, err, "23505")
				_, err = admin.ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1`, org)
				requireSQLState(t, err, "23514")
				_, err = admin.ExecContext(t.Context(), `UPDATE issues SET native_id='changed' WHERE organization_id=$1 AND native_id='wi_first_2'`, org)
				requireSQLState(t, err, "23514")
				if org == "org_first" {
					if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.WorkItem(t.Context(), "prj_first", "wi_first_1")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
