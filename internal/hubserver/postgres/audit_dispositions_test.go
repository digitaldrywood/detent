package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAuditDispositionWritesPreserveReadableSession(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, organization)
		t.Run(string(organization), func(t *testing.T) {
			fixture := seedHistory(t, repository, organization, "audit")
			row := eventstore.AuditDisposition{Organization: organization, Project: fixture.change.ProjectID, ChangeID: fixture.change.ID, ReviewSequence: fixture.evidence.Sequence, ActorID: "actor_first", RequestHash: "hash", Reference: eventstore.Reference("example/disposition", "digest", 1, 1), Disposition: securityaudit.Disposition{AuditRunID: 1, FindingID: "finding", Status: securityaudit.DispositionFalsePositive, RecordedAt: historyAt()}}
			write := func(input eventstore.AuditDisposition) (eventstore.AuditDisposition, error) {
				var got eventstore.AuditDisposition
				err := repository.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
					var err error
					got, err = w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), input)
					return err
				})
				return got, err
			}
			var first eventstore.AuditDisposition
			for index := range 200 {
				row.RequestKey = fmt.Sprint(index)
				got, err := write(row)
				if err != nil {
					t.Fatalf("write %d: %v", index, err)
				}
				if index == 0 {
					first = got
				}
			}
			for _, tc := range []struct {
				name    string
				actor   string
				finding string
			}{
				{"same finding", "actor_first", "finding"},
				{"other actor", "actor_second", "finding"},
				{"other finding", "actor_second", "finding_second"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					overflow := row
					overflow.ActorID, overflow.Disposition.FindingID, overflow.RequestKey = tc.actor, tc.finding, tc.name
					if _, err := write(overflow); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Errorf("overflow error = %v, want revision conflict", err)
					}
				})
			}
			replay, err := write(first)
			if err != nil || replay.Disposition.ID != first.Disposition.ID {
				t.Fatalf("replay = %v, %v", replay.Disposition.ID, err)
			}
			first.RequestHash = "changed"
			if _, err := write(first); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Errorf("changed replay error = %v", err)
			}
			row.Disposition.AuditRunID = 2
			row.RequestKey = "other-session"
			if _, err := write(row); err != nil {
				t.Fatalf("independent session: %v", err)
			}
			if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
				reader, ok := r.(tenantstore.ClaimChangeRowsReader)
				if !ok {
					t.Fatal("missing batched claim change snapshot")
				}
				snapshot, err := reader.ClaimChangeRows(t.Context(), row.Project, []string{fixture.change.ID})
				if err != nil {
					return err
				}
				if got := len(snapshot[fixture.change.ID].AuditDispositions[1]); got != 200 {
					t.Fatalf("snapshot dispositions=%d, want 200", got)
				}
				rows, err := r.(eventstore.AuditDispositionReader).AuditDispositions(t.Context(), row.Project, 1)
				if err != nil {
					return err
				}
				batch, err := r.(tenantstore.ClaimAuditDispositionRowsReader).ClaimAuditDispositionRows(t.Context(), row.Project, []int64{1, 2, 404})
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(batch[1], rows) || len(rows) != 200 || len(batch[2]) != 1 {
					t.Fatal("batched disposition metadata differs from scalar")
				}
				if empty, covered := batch[404]; !covered || len(empty) != 0 {
					t.Fatal("empty legacy audit run was not queried")
				}
				return nil
			}); err != nil {
				t.Fatalf("read full session: %v", err)
			}
		})
	}
}

func TestAuditDispositionConcurrentSessionBound(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	organization := tracker.OrganizationID("org_first")
	seedRepository(t, repository, organization)
	first := seedHistory(t, repository, organization, "first_audit")
	second := seedHistory(t, repository, organization, "second_audit")
	row := eventstore.AuditDisposition{Organization: organization, Project: first.change.ProjectID, ChangeID: first.change.ID, ReviewSequence: first.evidence.Sequence, ActorID: "actor_first", RequestHash: "hash", Reference: eventstore.Reference("example/disposition", "digest", 1, 1), Disposition: securityaudit.Disposition{AuditRunID: 1, FindingID: "finding", Status: securityaudit.DispositionFalsePositive, RecordedAt: historyAt()}}
	if err := repository.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
		for index := range 199 {
			row.RequestKey = fmt.Sprint(index)
			if _, err := w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var competing *repositoryTask
	if err := repository.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
		row.RequestKey = "last"
		if _, err := w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), row); err != nil {
			return err
		}
		competing = startRepositoryTask(t, func(ctx context.Context) error {
			return repository.WriteRecords(ctx, organization, corestore.Background, func(other eventstore.Writer) error {
				input := row
				input.ChangeID, input.ReviewSequence, input.ActorID, input.RequestKey = second.change.ID, second.evidence.Sequence, "actor_second", "overflow"
				_, err := other.(eventstore.AuditDispositionWriter).CreateAuditDisposition(ctx, input)
				return err
			})
		})
		waitLeaseLock(t, admin, 1, competing)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := competing.wait(t); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("competing write error = %v, want revision conflict", err)
	}
}
