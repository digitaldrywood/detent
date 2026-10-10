package hubserver

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSourcePlacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, availability              string
		source, initial, newerClean, uncertain bool
		corrupt, missing, wrongHead, pushed    bool
		owner, destination                     bool
	}{
		{name: "legacy local dirty is not pinned", state: "dirty", owner: true, destination: true},
		{name: "legacy unpushed is not pinned", state: "unpushed", owner: true, destination: true},
		{name: "pushed checkpoint restores anywhere", state: "dirty", pushed: true, owner: true, destination: true},
		{name: "pushed checkpoint ignores a missing Hub bundle", state: "unpushed", pushed: true, source: true, missing: true, owner: true, destination: true},
		{name: "missing local source", state: "dirty", availability: "missing"},
		{name: "inaccessible local source", state: "dirty", availability: "inaccessible"},
		{name: "verified immutable source", state: "unpushed", source: true, owner: true, destination: true},
		{name: "corrupt retained bytes", state: "unpushed", source: true, corrupt: true, owner: true},
		{name: "corrupt retained and missing local source", state: "unpushed", source: true, corrupt: true, availability: "missing"},
		{name: "missing retained bytes", state: "unpushed", source: true, missing: true, owner: true},
		{name: "legacy checkpoint ahead of verified source is not pinned", state: "unpushed", source: true, wrongHead: true, owner: true, destination: true},
		{name: "legacy dirty checkpoint after verified source is not pinned", state: "dirty", source: true, owner: true, destination: true},
		{name: "initial checkout is not completed source", state: "clean", initial: true, owner: true, destination: true},
		{name: "clean startup with uncertain publication owns source", state: "clean", initial: true, uncertain: true, owner: true},
		{name: "newer clean startup does not hide source", state: "dirty", newerClean: true, owner: true, destination: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
			worker := f.worker(t, "source-worker")
			lease := claimNativeAttempt(t, f.nativeFixture, worker, "source-machine", "source-session", f.issue.WorkItemID)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "source-checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.HeadSHA = changeTestInput().HeadSHA
			if test.wrongHead {
				event.Data.Handoff.HeadSHA = strings.Repeat("c", 40)
			}
			event.Data.Handoff.WorktreeState = test.state
			if test.initial {
				event.Data.Handoff.Resume = "fresh_checkout"
			}
			if test.uncertain {
				event.Data.Handoff.ExternalEffect, event.Data.Handoff.EffectState = "pr_create", "ambiguous"
				event.Data.Handoff.EffectID = "effect_" + strings.Repeat("e", 32)
			}
			if test.availability != "" {
				event.Data.Handoff.Availability = test.availability
			}
			if test.pushed {
				event.Data.Handoff.Storage, event.Data.Handoff.Ref = "git_ref", tracker.CheckpointRefPrefix+string(f.issue.WorkItemID)
				event.Data.Handoff.CommitSHA, event.Data.Handoff.TreeSHA = strings.Repeat("c", 40), strings.Repeat("e", 40)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			if test.source {
				input := changeTestInput()
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				bundle := []byte("bounded retained source fixture")
				input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-version"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
				if test.corrupt || test.missing {
					statement := "DROP TRIGGER change_sources_no_update"
					if test.missing {
						statement = "DROP TRIGGER change_sources_no_delete"
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
					statement = "UPDATE change_sources SET bundle=x'00'"
					if test.missing {
						statement = "DELETE FROM change_sources"
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.newerClean {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases (lease_id,issue_id,machine_id,session_id,fencing_token,acquired_at,renewed_at,expires_at,updated_at,created_at) SELECT 'new-startup',issue_id,machine_id,'startup',fencing_token+1,acquired_at,renewed_at,expires_at,updated_at,created_at FROM leases WHERE lease_id=?`, lease.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts (id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at) SELECT 'new-startup',organization_id,project_id,work_item_id,'new-startup',fencing_token+1,run_id,sequence,status,data_json,'{"resume":"fresh_checkout","worktree_state":"clean"}',started_at,updated_at FROM native_attempts WHERE id=?`, event.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			if !test.initial || test.uncertain {
				recovery, err := f.service.readNativeRecovery(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				if recovery.SourceAttemptID != event.Data.AttemptID {
					t.Fatalf("clean startup hid source recovery: %q", recovery.SourceAttemptID)
				}
			}

			_, id, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []struct {
				machine tracker.MachineID
				allowed bool
			}{{"source-machine", test.owner}, {"destination-machine", test.destination}} {
				allowed, reason, err := nativeSourceClaimAllowed(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, candidate.machine, id)
				if err != nil || allowed != candidate.allowed {
					t.Fatalf("%s allowed=%v reason=%q error=%v", candidate.machine, allowed, reason, err)
				}
				if !allowed && !strings.Contains(reason, "source runner source-machine") {
					t.Fatalf("refusal lost source owner: %q", reason)
				}
			}
		})
	}
}

func TestNativeSourceOwnerRunnerPlacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, otherReason string
		source, ownerOffline     bool
		otherAllowed             bool
	}{
		{name: "local-only checkpoint is not pinned to its online owner", state: "dirty", otherAllowed: true},
		{name: "local-only checkpoint does not wait for an offline owner", state: "unpushed", ownerOffline: true, otherAllowed: true},
		{name: "verified durable source is not pinned to an online owner", state: "unpushed", source: true, otherAllowed: true},
		{name: "verified durable source moves when the owner is offline", state: "unpushed", source: true, ownerOffline: true, otherAllowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
			owner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			owner.enroll(t)
			other.enroll(t)
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: f.issue.WorkItemID, MachineID: owner.binding.MachineID, SessionID: "source", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", owner.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			event := nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", owner.redemption.Credential, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.WorktreeState, event.Data.Handoff.HeadSHA = test.state, changeTestInput().HeadSHA
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", owner.redemption.Credential, event), http.StatusOK)
			if test.source {
				input := changeTestInput()
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				bundle := []byte("retained source fixture")
				input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
			}
			finish := event
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome, finish.Data.Handoff = "run.finished", "finished", 3, "succeeded", nil
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", owner.redemption.Credential, finish), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			at := now
			if test.ownerOffline {
				at = now.Add(runnerauth.HeartbeatTimeout + time.Minute)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(at), other.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}

			_, id, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []struct {
				runner  runnerFixture
				allowed bool
				reason  string
			}{{owner, true, ""}, {other, test.otherAllowed, test.otherReason}} {
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Runner: candidate.runner.identity}}
				allowed, reason, err := placementClaimAllowed(t.Context(), f.service.database.db, scope, candidate.runner.binding.MachineID, id, at, nil)
				if err != nil || allowed != candidate.allowed {
					t.Fatalf("%s allowed=%v reason=%q error=%v", candidate.runner.binding.RunnerID, allowed, reason, err)
				}
				if !allowed && !strings.Contains(reason, candidate.reason+" "+string(owner.binding.MachineID)) {
					t.Fatalf("refusal lost the waiting owner: %q", reason)
				}
			}
		})
	}
}
