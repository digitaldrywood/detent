package hubserver

import (
	"errors"
	"github.com/digitaldrywood/detent/internal/artifact"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/claimsource"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecoveryAndClaimWithoutRunEvents(t *testing.T) {
	t.Parallel()
	for _, stored := range []bool{false, true} {
		name := "base"
		if stored {
			name = "stored_change"
		}
		t.Run(name, func(t *testing.T) {
			service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
			f := newChangeFixture(t, service)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				for i := range project.States {
					project.States[i].Transitions = append(project.States[i].Transitions, "Merging")
				}
				project.States = append(project.States, tracker.NativeState{Name: "Merging", Dispatchable: true})
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			runner.enroll(t)
			lease := nativeAttemptFixtureLease(t, runner, f.issue, "old")
			start := nativeStartedEvent(lease)
			start.Data.Identity.Role, start.Data.MachineID = "code", lease.MachineID
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			worker := runner.redemption.Credential
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
			checkpoint := start
			checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
			checkpoint.Data.Handoff = nativeTestCheckpoint()
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
			if stored {
				input := changeTestInput()
				input.AttemptID, input.RunID = start.Data.AttemptID, start.Data.RunID
				bundle := changeTestSource(t, &input)
				diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: start.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.path+"/versions", worker, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			db := nativeAttemptTestDatabase(t, f.nativeFixture)
			tamperPostgresRows(t, db, "DELETE FROM collaboration_events WHERE organization_id=$1 AND attempt_id=$2", f.project.OrganizationID, start.Data.AttemptID)
			state := "Todo"
			if stored {
				state = "Merging"
			}
			currentResponse := performHubAPIRequest(t, service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, currentResponse, http.StatusOK)
			var current tracker.NativeIssue
			decodeHubResponse(t, currentResponse, &current)
			if current.State != state {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "lane"}, ExpectedRevision: current.Revision, State: state, Reason: "user_requested"}), http.StatusOK)
			}
			var item corestore.WorkItem
			if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				item, err = r.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			facts, err := claimsource.Read(t.Context(), claimsource.Deps{Repository: service.config.TenantRepository, Details: service.runDetailStore(), Checkpoint: service.readNativeAttemptCheckpointTenant}, f.project.OrganizationID, f.project.ID, item)
			if err != nil {
				t.Fatal(err)
			}
			if allowed, reason := claimsource.Allowed(facts, "other_machine", "other_runner"); !allowed {
				t.Fatalf("claim source refused: %s", reason)
			}
			if _, err := service.readNativeRecoveryFactsTenant(t.Context(), nativeAttemptListingScope(t, f.nativeFixture), string(f.issue.WorkItemID), false); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, service, http.MethodGet, path+"?view=recovery", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if facts.Checkpoint != nil || facts.CheckpointUnreadable {
				t.Fatalf("missing events retained checkpoint: %+v", facts)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				reason, err := claimsource.Refusal(t.Context(), r, f.project.ID, item, facts, "other_machine", "other_runner")
				if reason != "" {
					t.Fatalf("claim commit refused: %s", reason)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, response, &recovery)
			for _, attempt := range recovery.Attempts {
				if attempt.Checkpoint != nil {
					t.Fatalf("missing events exposed checkpoint: %+v", attempt.Checkpoint)
				}
			}
			if stored {
				tamperPostgresRows(t, db, "UPDATE change_versions SET head_sha=$1 WHERE organization_id=$2 AND id=$3", strings.Repeat("f", 40), f.project.OrganizationID, facts.VersionID)
				tampered, err := claimsource.Read(t.Context(), claimsource.Deps{Repository: service.config.TenantRepository, Details: service.runDetailStore(), Checkpoint: service.readNativeAttemptCheckpointTenant}, f.project.OrganizationID, f.project.ID, item)
				if err == nil {
					if allowed, _ := claimsource.Allowed(tampered, "other_machine", "other_runner"); allowed {
						t.Fatal("tampered Change source allowed")
					}
				} else if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("tampered source error: %v", err)
				}
			}
			if !stored {
				claimNativeAttempt(t, f.nativeFixture, worker, "example", "fresh", f.issue.WorkItemID)
			}
		})
	}
}
