package executionrecords_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChecklistKeepsAttemptsAndReplayedStateDistinct(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	ctx := t.Context()
	now := time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(id))
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, org, event)
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			list := tracker.ExecutionChecklist{AttemptID: event.ID, Revision: strings.Repeat("d", 64), PolicyID: "policy", FrozenAt: now,
				Checks: []tracker.ExecutionCheck{{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: string(org) + " " + event.ID, Status: tracker.CheckNeedsReview}}}
			change := tracker.ExecutionCheckUpdate{Ordinal: 1, Status: tracker.CheckPassed, Head: strings.Repeat("c", 40), Tree: strings.Repeat("a", 40), Receipt: "command-receipt"}
			intervention := tracker.ExecutionLoopIntervention{Revision: list.Revision, ThreadID: "thread", TurnID: "turn", Action: "steer", Fingerprint: strings.Repeat("b", 64), Period: 1, Repeats: 3, Sequence: 3, ObservedAt: now}
			if err := r.WriteLeases(ctx, org, corestore.Serving, func(w leasestore.Writer) error {
				owner := w.(leasestore.ChecklistWriter)
				if err := owner.FreezeExecutionChecklist(ctx, project, list); err != nil {
					return err
				}
				if err := owner.RecordExecutionCheckChanges(ctx, project, event.ID, list.Revision, "round-1", now, []tracker.ExecutionCheckUpdate{change}); err != nil {
					return err
				}
				if err := owner.RecordLoopIntervention(ctx, project, event.ID, intervention); err != nil {
					return err
				}
				if err := owner.RecordLoopIntervention(ctx, project, event.ID, intervention); err != nil {
					return err
				}
				return owner.FreezeExecutionChecklist(ctx, project, list)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, id := range []int64{1, 2} {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			attempt := fmt.Sprintf("attempt_lease_%d", id)
			if err := r.ReadLeases(ctx, org, corestore.Serving, func(r leasestore.Reader) error {
				got, err := r.(leasestore.ChecklistReader).ExecutionChecklist(ctx, project, attempt)
				if err != nil {
					return err
				}
				if got == nil || len(got.Checks) != 1 || got.Checks[0].Text != string(org)+" "+attempt || got.Checks[0].Status != tracker.CheckPassed || got.Checks[0].Head != strings.Repeat("c", 40) || got.Checks[0].Receipt != "command-receipt" {
					t.Fatalf("tenant %s attempt %s: %+v", org, attempt, got)
				}
				if len(got.Interventions) != 1 || got.Interventions[0].Action != "steer" || got.Interventions[0].Sequence != 3 {
					t.Fatalf("tenant %s attempt %s intervention: %+v", org, attempt, got.Interventions)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
