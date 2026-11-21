package executionrecords_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
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

func TestProjectGateFailureHistorySurvivesPassingRetry(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	ctx := t.Context()
	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	const org = tracker.OrganizationID("org_example")
	const project = tracker.ProjectID("prj_1")
	const item = tracker.NativeWorkItemID("wi_1")
	const command = "make check"
	const policyID = "policy"
	head, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	pgstoretest.SeedLeaseFamily(t, r, org)
	claim := pgstoretest.LeaseClaim(1)
	writeObservation := func(lease leasestore.Lease, receipt string, code int) {
		t.Helper()
		attempt := pgstoretest.AttemptEvent(lease)
		pgstoretest.RecordLeaseAttempt(t, r, org, attempt)
		when := pgstoretest.HistoryAt()
		check := tracker.ExecutionCheck{Ordinal: 1, Kind: tracker.CheckProjectGate, Text: "Project command gate", CheckDigest: policy.Digest([]byte(command)), Status: tracker.CheckUnverified}
		list := tracker.ExecutionChecklist{AttemptID: attempt.ID, Revision: strings.Repeat("c", 64), PolicyID: policyID, FrozenAt: when, Checks: []tracker.ExecutionCheck{check}}
		status := tracker.CheckPassed
		if code != 0 {
			status = tracker.CheckFailed
		}
		change := tracker.ExecutionCheckUpdate{Ordinal: 1, Status: status, Head: head, Tree: tree, Receipt: receipt}
		event := tracker.CollaborationEvent{ID: "run-" + receipt, OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item, Type: "run.observed", SchemaVersion: 1,
			Actor: pgstoretest.HistoryActor(), RecordedAt: when, Data: tracker.CollaborationData{Run: &tracker.NativeRunData{
				AttemptID: attempt.ID, PolicyID: policyID, Runtime: &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{Command: command, HeadSHA: head, TreeSHA: tree, ReceiptID: receipt, ExitCode: code, Output: "TestReport"}},
			}},
		}
		row, err := content.PrepareEvent(ctx, org, pgstoretest.HistoryJSON(t, event))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.AppendEvent(ctx, row); err != nil {
				return err
			}
			owner := w.(leasestore.ChecklistWriter)
			if err := owner.FreezeExecutionChecklist(ctx, project, list); err != nil {
				return err
			}
			return owner.RecordExecutionCheckChanges(ctx, project, attempt.ID, list.Revision, "evidence:"+event.ID, when, []tracker.ExecutionCheckUpdate{change})
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := pgstoretest.ClaimLease(t, r, org, claim)
	writeObservation(first, "failed", 1)
	if err := r.WriteLeases(ctx, org, corestore.Serving, func(w leasestore.Writer) error {
		_, err := w.Release(ctx, pgstoretest.LeaseFence(first), "finished")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claim.LeaseID, claim.SessionID = "lease_retry", "session_retry"
	writeObservation(pgstoretest.ClaimLease(t, r, org, claim), "passed", 0)
	for _, test := range []struct {
		name, head, tree, command, policy string
		itemID                            int64
		want                              bool
	}{
		{name: "failure from prior fenced attempt", head: head, tree: tree, command: command, policy: policyID, itemID: 1, want: true},
		{name: "different head", head: strings.Repeat("d", 40), tree: tree, command: command, policy: policyID, itemID: 1},
		{name: "different tree", head: head, tree: strings.Repeat("d", 40), command: command, policy: policyID, itemID: 1},
		{name: "different command", head: head, tree: tree, command: "make other", policy: policyID, itemID: 1},
		{name: "different policy", head: head, tree: tree, command: command, policy: "other-policy", itemID: 1},
		{name: "different item", head: head, tree: tree, command: command, policy: policyID, itemID: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var failures []leasestore.ProjectGateFailureEvent
			err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				failures, err = reader.(leasestore.ProjectGateFailureReader).ProjectGateFailureEvents(t.Context(), project, item, test.itemID, test.policy, policy.Digest([]byte(test.command)), test.head, test.tree, leasestore.ProjectGateFailurePosition{})
				return err
			})
			if err != nil || test.want && (len(failures) != 1 || failures[0].ReceiptID != "failed") || !test.want && len(failures) != 0 {
				t.Fatalf("failure events = %+v, error = %v, want failure = %v", failures, err, test.want)
			}
			if test.want {
				event, err := content.Event(t.Context(), failures[0].Event)
				if err != nil || event.Data.Run.Runtime.Validation.Output != "TestReport" {
					t.Fatalf("recorded failure diagnostic = %+v, error = %v", event.Data.Run, err)
				}
			}
		})
	}
}
