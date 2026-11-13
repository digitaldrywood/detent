package hubserver

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			details := &memoryRunDetails{}
			f := newChangeFixture(t, newPostgresNativeService(t, Config{RunDetails: details}))
			worker := f.worker(t, "source-worker")
			lease := claimNativeAttempt(t, f.nativeFixture, worker, "source-machine", "source-session", f.issue.WorkItemID)
			event := nativeStartedEvent(lease)
			event.Data.Identity.Role, event.Data.MachineID = "code", lease.MachineID
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
				admin := nativeAttemptTestDatabase(t, f.nativeFixture)
				var before string
				if err := admin.QueryRowContext(t.Context(), "SELECT checkpoint_object_key FROM native_attempts WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, event.Data.AttemptID).Scan(&before); err != nil || before == "" {
					t.Fatalf("historical checkpoint key=%q error=%v", before, err)
				}
				result, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, event.Data.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				changed, err := result.RowsAffected()
				if err != nil || changed != 1 {
					t.Fatalf("unknown source fact rows=%d error=%v", changed, err)
				}
				var after string
				var preserves *bool
				if err := admin.QueryRowContext(t.Context(), "SELECT checkpoint_object_key,checkpoint_preserves_source FROM native_attempts WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, event.Data.AttemptID).Scan(&after, &preserves); err != nil || after != before || preserves != nil {
					t.Fatalf("historical checkpoint source fields changed: key=%q known=%t error=%v", after, preserves != nil, err)
				}
			}
			if test.source {
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: event.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+event.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", worker, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-version", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				var version tracker.ChangeVersion
				decodeHubResponse(t, response, &version)
				if test.corrupt || test.missing {
					var key string
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						stored, err := reader.Version(t.Context(), f.change.ID, version.ID)
						if err == nil {
							if stored.SourceObjectKey == "" || stored.SourceObjectKey == stored.PayloadObjectKey {
								t.Fatal("source fault does not name its own bundle")
							}
							key = stored.SourceObjectKey
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					details.mu.Lock()
					original, present := details.data[key]
					original = bytes.Clone(original)
					if test.missing {
						delete(details.data, key)
					} else {
						details.data[key] = []byte{0}
					}
					details.mu.Unlock()
					t.Cleanup(func() { details.mu.Lock(); details.data[key] = original; details.mu.Unlock() })
					if !present || len(original) == 0 {
						t.Fatal("retained source fixture has no bundle bytes")
					}
				}
			}
			var startup tracker.NativeRunEvent
			if test.newerClean {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				nextLease := claimNativeAttempt(t, f.nativeFixture, worker, "source-machine", "startup", f.issue.WorkItemID)
				if nextLease.ID == lease.ID || nextLease.FencingToken <= lease.FencingToken {
					t.Fatal("clean startup did not receive a new fenced lease")
				}
				startup = nativeStartedEvent(nextLease)
				startup.IdempotencyKey = "startup-start"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, startup), http.StatusOK)
				startup.Type, startup.IdempotencyKey, startup.Data.Sequence = "run.checkpointed", "clean-startup-checkpoint", 2
				startup.Data.Handoff = &tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: input.HeadSHA, ExternalEffect: "none", EffectState: "none"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, startup), http.StatusOK)
				if startup.Data.AttemptID == event.Data.AttemptID {
					t.Fatal("clean startup reused the source attempt")
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery&projection=bounded", worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, response, &recovery)
			if !test.initial || test.uncertain {
				if recovery.SourceAttemptID != event.Data.AttemptID {
					t.Fatalf("clean startup hid source recovery: %q", recovery.SourceAttemptID)
				}
			} else if recovery.SourceAttemptID != "" {
				t.Fatalf("initial checkout became completed source: %q", recovery.SourceAttemptID)
			}
			sourceFound, startupFound := false, !test.newerClean
			for _, attempt := range recovery.Attempts {
				if attempt.AttemptID == event.Data.AttemptID {
					sourceFound = true
					checkpoint := attempt.Checkpoint
					if attempt.MachineID != lease.MachineID || checkpoint == nil || checkpoint.HeadSHA != event.Data.Handoff.HeadSHA || checkpoint.Resume != event.Data.Handoff.Resume || checkpoint.WorktreeState != test.state || checkpoint.Storage != event.Data.Handoff.Storage || checkpoint.Availability != event.Data.Handoff.Availability || checkpoint.ExternalEffect != event.Data.Handoff.ExternalEffect || checkpoint.EffectState != event.Data.Handoff.EffectState || checkpoint.EffectID != event.Data.Handoff.EffectID || checkpoint.Ref != event.Data.Handoff.Ref || checkpoint.CommitSHA != event.Data.Handoff.CommitSHA || checkpoint.TreeSHA != event.Data.Handoff.TreeSHA {
						t.Fatalf("recovery lost original source checkpoint: %+v", attempt)
					}
				}
				if test.newerClean && attempt.AttemptID == startup.Data.AttemptID {
					startupFound = true
					if attempt.Checkpoint == nil || attempt.Checkpoint.PreservesSource() || attempt.Checkpoint.WorktreeState != "clean" || attempt.Checkpoint.Resume != "fresh_checkout" {
						t.Fatalf("new startup checkpoint was not clean: %+v", attempt)
					}
				}
			}
			if !sourceFound || !startupFound {
				t.Fatalf("source or independent clean startup omitted: source=%t startup=%t", sourceFound, startupFound)
			}
			t.Logf("legacy upfront source admission expectations: source_runner=%t destination_runner=%t", test.owner, test.destination)
		})
	}
}
