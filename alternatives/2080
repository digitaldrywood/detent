package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublishedReviewCheckpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		checkpoint any
		wantReview bool
	}{
		{name: "missing checkpoint"},
		{name: "null checkpoint", checkpoint: "null"},
		{name: "valid checkpoint", wantReview: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newChangeFixture(t, nil)
			version := f.publish(t, "current", "")
			runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
			ctx := f.service.runDetailContext(t.Context())
			now := f.service.config.now()
			tx, err := f.service.database.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", tracker.NativeRunData{
				Disposition:  &tracker.NativeDisposition{Status: "complete"},
				Finalization: &tracker.NativeFinalization{Changed: true, ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA},
			}, now, -1)
			checkpoint := test.checkpoint
			if test.wantReview {
				checkpoint, err = marshalNative(tracker.NativeCheckpoint{WorktreeState: "clean", HeadSHA: version.HeadSHA})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_json=?,work_item_revision=(SELECT revision FROM issues WHERE native_id=?),dispatch_generation=(SELECT dispatch_generation FROM issues WHERE native_id=?) WHERE id=?", checkpoint, f.issue.WorkItemID, f.issue.WorkItemID, attempt); err != nil {
				t.Fatal(err)
			}
			var id tracker.WorkItemID
			if err := tx.QueryRowContext(ctx, "SELECT id FROM issues WHERE native_id=?", f.issue.WorkItemID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			review, gotAttempt, err := readPublishedReview(ctx, tx, id, now)
			if err != nil {
				t.Fatal(err)
			}
			want := connector.ChangeReview{}
			wantAttempt := ""
			if test.wantReview {
				want.Current = true
				wantAttempt = attempt
			}
			if review != want || gotAttempt != wantAttempt {
				t.Fatalf("published review = %+v, attempt = %q; want %+v, %q", review, gotAttempt, want, wantAttempt)
			}
		})
	}
}
