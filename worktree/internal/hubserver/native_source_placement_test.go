package hubserver

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSourcePlacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, availability              string
		source, initial, newerClean, uncertain bool
		corrupt, missing, wrongHead, pushed    bool
		owner, destination                     bool
		unknown                                bool
	}{
		{name: "legacy local dirty is not pinned", state: "dirty", owner: true, destination: true},
		{name: "legacy unpushed is not pinned", state: "unpushed", owner: true, destination: true},
		{name: "pushed checkpoint restores anywhere", state: "dirty", pushed: true, owner: true, destination: true},
		{name: "pushed checkpoint ignores a missing Hub bundle", state: "unpushed", pushed: true, source: true, missing: true, owner: true, destination: true},
		{name: "unknown missing local source", state: "dirty", availability: "missing", unknown: true},
		{name: "unknown clean checkpoint", state: "clean", initial: true, unknown: true, owner: true, destination: true},
		{name: "missing local source", state: "dirty", availability: "missing"},
		{name: "inaccessible local source", state: "dirty", availability: "inaccessible"},
		{name: "verified immutable source", state: "unpushed", source: true, owner: true, destination: true},
		{name: "corrupt retained bytes", state: "unpushed", source: true, corrupt: true, owner: true},
		{name: "corrupt retained and missing local source", state: "unpushed", source: true, corrupt: true, availability: "missing"},
		{name: "missing retained bytes", state: "unpushed", source: true, missing: true, owner: true},
		{name: "legacy checkpoint ahead of verified source is not pinned", state: "unpushed", source: true, wrongHead: true, owner: true, destination: true},
		{name: "legacy dirty checkpoint after verified source is not pinned", state: "dirty", source: true, owner: true, destination: true},
		{name: "initial checkout is not completed source", state: "clean", initial: true, owner: true, destination: true},
		{name: "uncertain publication reconciles on any runner", state: "clean", initial: true, uncertain: true, owner: true, destination: true},
		{name: "uncertain publication of verified source reconciles on any runner", state: "unpushed", source: true, uncertain: true, owner: true, destination: true},
		{name: "newer clean startup does not hide source", state: "dirty", newerClean: true, owner: true, destination: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &changeObjectFixture{}
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call}))
			worker := f.worker(t, "source-worker")
			lease := claimNativeAttempt(t, f.nativeFixture, worker, "source-machine", "source-session", f.issue.WorkItemID)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "source-checkpoint", 2
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.HeadSHA = input.HeadSHA
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
			if test.unknown {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE id=?`, event.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			if test.source {
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-version"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
				if test.corrupt {
					var key string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT object_key FROM change_sources ORDER BY rowid DESC LIMIT 1").Scan(&key); err != nil {
						t.Fatal(err)
					}
					objects.mu.Lock()
					objects.objects[key] = []byte{0}
					objects.mu.Unlock()
				}
				if test.missing {
					if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER change_sources_no_delete"); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM change_sources"); err != nil {
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
				allowed, reason, err := nativeSourceClaimAllowed(f.service.runDetailContext(t.Context()), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, candidate.machine, id)
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
