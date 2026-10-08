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
		name, state, availability   string
		source, initial, newerClean bool
		corrupt, missing, wrongHead bool
		owner, destination          bool
	}{
		{name: "local dirty", state: "dirty", owner: true},
		{name: "legacy unpushed", state: "unpushed", owner: true},
		{name: "missing local source", state: "dirty", availability: "missing"},
		{name: "inaccessible local source", state: "dirty", availability: "inaccessible"},
		{name: "verified immutable source", state: "unpushed", source: true, owner: true, destination: true},
		{name: "corrupt retained bytes", state: "unpushed", source: true, corrupt: true},
		{name: "missing retained bytes", state: "unpushed", source: true, missing: true},
		{name: "retained older head cannot replace checkpoint", state: "unpushed", source: true, wrongHead: true, owner: true},
		{name: "durable commit does not contain dirty changes", state: "dirty", source: true, owner: true},
		{name: "initial checkout is not completed source", state: "clean", initial: true, owner: true, destination: true},
		{name: "newer clean startup does not hide source", state: "dirty", newerClean: true, owner: true},
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
			if test.availability != "" {
				event.Data.Handoff.Availability = test.availability
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			if test.source {
				input := changeTestInput()
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				bundle := []byte("bounded retained source fixture")
				input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-version"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
				if test.corrupt || test.missing {
					// Simulate lost/corrupt storage, beyond normal immutable writes.
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
				// Model a historic destination startup before owner affinity existed.
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases (lease_id,issue_id,machine_id,session_id,fencing_token,acquired_at,renewed_at,expires_at,updated_at,created_at) SELECT 'new-startup',issue_id,machine_id,'startup',fencing_token+1,acquired_at,renewed_at,expires_at,updated_at,created_at FROM leases WHERE lease_id=?`, lease.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts (id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at) SELECT 'new-startup',organization_id,project_id,work_item_id,'new-startup',fencing_token+1,run_id,sequence,status,data_json,'{"resume":"fresh_checkout","worktree_state":"clean"}',started_at,updated_at FROM native_attempts WHERE id=?`, event.Data.AttemptID); err != nil {
					t.Fatal(err)
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
