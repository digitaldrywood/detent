package executionrecords_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptVerdictPreservesTenantFailures(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(id))
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, org, event)
			event.Sequence, event.Status, event.PayloadObjectKey = 2, "failed", "example/failed"
			event.Failure = &leasestore.AttemptFailure{Example: string(org) + " original failure", Class: "attempt"}
			pgstoretest.RecordLeaseAttempt(t, r, org, event)
			verdict := leasestore.ProgressVerdict{OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", id)), AttemptID: event.ID, AttemptSequence: 2, EvidenceRevision: fmt.Sprintf("%s/%d", org, id), AttemptedStep: "implement", Success: true, Outcome: "progressing", Reason: string(org) + " model reason", NextStep: new("implement")}
			write := func(v leasestore.ProgressVerdict) error {
				return r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					return w.(leasestore.ProgressVerdictWriter).RecordProgressVerdict(t.Context(), v)
				})
			}
			wrong := verdict
			wrong.OrganizationID = "org_foreign"
			if err := write(wrong); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("cross-tenant publication: %v", err)
			}
			if err := write(verdict); err != nil {
				t.Fatal(err)
			}
			if err := write(verdict); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("verdict overwritten: %v", err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, id := range []int64{1, 2} {
			var status, payload, failure, revision, reason, next string
			var success bool
			err := admin.QueryRowContext(t.Context(), `SELECT status,payload_object_key,failure_example,verdict_evidence_revision,verdict_reason,verdict_success,verdict_next_step FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, fmt.Sprintf("attempt_lease_%d", id)).Scan(&status, &payload, &failure, &revision, &reason, &success, &next)
			if err != nil {
				t.Fatal(err)
			}
			if status != "failed" || payload != "example/failed" || failure != string(org)+" original failure" || revision != fmt.Sprintf("%s/%d", org, id) || reason != string(org)+" model reason" || !success || next != "implement" {
				t.Fatalf("tenant %s attempt %d mixed or replaced terminal evidence: %s %s %s %s %s %v %s", org, id, status, payload, failure, revision, reason, success, next)
			}
		}
	}
}
