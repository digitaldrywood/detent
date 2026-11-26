package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRecoveryBoundedNonSourceCheckpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		prependSource  bool
		nullCleanFlags bool
		wantSourceID   string
	}{
		{name: "only non-source checkpoints", prependSource: false, wantSourceID: ""},
		{name: "older source checkpoint behind 205 non-source checkpoints", prependSource: true, wantSourceID: "attempt_source_0000"},
		{name: "older source checkpoint behind 205 legacy null non-source checkpoints", prependSource: true, nullCleanFlags: true, wantSourceID: "attempt_source_0000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			details := &memoryRunDetails{}
			f := newChangeFixture(t, newPostgresNativeService(t, Config{RunDetails: details}))
			worker := f.worker(t, "bounded-worker")
			lease := claimNativeAttempt(t, f.nativeFixture, worker, "bounded-machine", "bounded-session", f.issue.WorkItemID)
			started := nativeStartedEvent(lease)
			started.Data.Identity.Role, started.Data.MachineID = "code", lease.MachineID
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, started), http.StatusOK)

			now := f.service.config.Clock().UTC()
			content := eventstore.NewContent(details)
			actor := tracker.Actor{Kind: "runner", PrincipalID: "runner_test"}
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			var issueRowID int64
			if err := admin.QueryRowContext(t.Context(), `SELECT id FROM work_item_identities WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID).Scan(&issueRowID); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3`, now, f.project.OrganizationID, string(lease.ID)); err != nil {
				t.Fatal(err)
			}

			recordCheckpoint := func(attemptID, leaseID, eventID string, ts time.Time, checkpoint *tracker.NativeCheckpoint) {
				t.Helper()
				cpRaw, err := json.Marshal(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				cpRef, err := content.PreparePayload(t.Context(), f.project.OrganizationID, cpRaw)
				if err != nil {
					t.Fatal(err)
				}
				var fencing int64
				if err := admin.QueryRowContext(t.Context(),
					`INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,project_id) VALUES ($1,$2,$3,$4,$5,1,$6,$6,$7,$8) RETURNING fencing_token`,
					f.project.OrganizationID, leaseID, issueRowID, string(lease.MachineID), "session-"+leaseID, ts, ts.Add(time.Hour), f.project.ID,
				).Scan(&fencing); err != nil {
					t.Fatal(err)
				}
				runData := tracker.NativeRunData{
					AttemptID:    attemptID,
					RunID:        started.Data.RunID,
					LeaseID:      tracker.LeaseID(leaseID),
					FencingToken: tracker.FencingToken(fencing),
					Sequence:     1,
					Handoff:      checkpoint,
				}
				raw, err := json.Marshal(tracker.CollaborationEvent{
					ID:             eventID,
					OrganizationID: f.project.OrganizationID,
					ProjectID:      f.project.ID,
					AggregateType:  "issue",
					AggregateID:    f.issue.WorkItemID,
					Type:           "run.checkpointed",
					SchemaVersion:  1,
					Actor:          actor,
					RecordedAt:     ts,
					Data:           tracker.CollaborationData{Run: &runData},
				})
				if err != nil {
					t.Fatal(err)
				}
				ev, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, raw)
				if err != nil {
					t.Fatal(err)
				}
				runRaw, err := json.Marshal(runData)
				if err != nil {
					t.Fatal(err)
				}
				runRef, err := content.PreparePayload(t.Context(), f.project.OrganizationID, runRaw)
				if err != nil {
					t.Fatal(err)
				}
				ev.WorkItemID = f.issue.WorkItemID
				ev.AttemptID = attemptID
				preserves := checkpoint.PreservesSource()
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.AppendEvent(t.Context(), ev); err != nil {
						return fmt.Errorf("append event %s: %w", attemptID, err)
					}
					_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{
						ID:                        attemptID,
						Fence:                     leasestore.Fence{LeaseID: leaseID, FencingToken: fencing},
						RunID:                     started.Data.RunID,
						Sequence:                  1,
						Status:                    "running",
						StartedAt:                 ts,
						PayloadObjectKey:          runRef.Key,
						PayloadReference:          runRef,
						CheckpointObjectKey:       &cpRef.Key,
						CheckpointReference:       cpRef,
						CheckpointPreservesSource: &preserves,
					})
					if err != nil {
						return fmt.Errorf("record attempt %s: %w", attemptID, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3`, ts, f.project.OrganizationID, leaseID); err != nil {
					t.Fatal(err)
				}
			}

			if tc.prependSource {
				sourceCP := nativeTestCheckpoint()
				if !sourceCP.PreservesSource() {
					t.Fatal("source test checkpoint unexpectedly does not preserve source")
				}
				recordCheckpoint("attempt_source_0000", "lease_source_0000", "evt_source_0000", now.Add(time.Second), sourceCP)
			}

			cleanCP := nativeTestCheckpoint()
			cleanCP.WorktreeState = "clean"
			cleanCP.Resume = "fresh_checkout"
			cleanCP.ExternalEffect = ""
			cleanCP.EffectState = ""
			if cleanCP.PreservesSource() {
				t.Fatal("clean test checkpoint unexpectedly preserves source")
			}
			total := claimPreparationLimit + 5
			for i := 1; i <= total; i++ {
				recordCheckpoint(
					fmt.Sprintf("attempt_bounded_%04d", i),
					fmt.Sprintf("lease_bounded_%04d", i),
					fmt.Sprintf("evt_bounded_%04d", i),
					now.Add(time.Duration(i+1)*time.Second),
					cleanCP,
				)
			}
			if tc.nullCleanFlags {
				if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE organization_id=$1 AND issue_id=$2 AND id LIKE 'attempt_bounded_%'`, f.project.OrganizationID, issueRowID); err != nil {
					t.Fatal(err)
				}
			}

			resp := performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", worker, nil)
			requireNativeStatus(t, resp, http.StatusOK)
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, resp, &recovery)
			if recovery.SourceAttemptID != tc.wantSourceID {
				t.Fatalf("SourceAttemptID = %q, want %q", recovery.SourceAttemptID, tc.wantSourceID)
			}
		})
	}
}
