package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
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

func TestNativeSourceOwnerRunnerPlacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, otherReason         string
		invalidate                       string
		source, ownerOffline             bool
		objects, landing, readerFallback bool
		otherAllowed                     bool
		saturated, unknown               bool
	}{
		{name: "local-only checkpoint is not pinned to its online owner", state: "dirty", otherAllowed: true},
		{name: "local-only checkpoint does not wait for an offline owner", state: "unpushed", ownerOffline: true, otherAllowed: true},
		{name: "verified durable source is not pinned to an online owner", state: "unpushed", source: true, otherAllowed: true},
		{name: "verified durable source moves when the owner is offline", state: "unpushed", source: true, ownerOffline: true, otherAllowed: true},
		{name: "claim prepares stored source and archived checkpoint", state: "unpushed", source: true, objects: true, otherAllowed: true},
		{name: "claim prepares landing evidence", state: "unpushed", source: true, objects: true, landing: true, otherAllowed: true},
		{name: "claim revalidates checkpoint sequence", state: "unpushed", source: true, objects: true, otherAllowed: true, invalidate: "checkpoint"},
		{name: "claim revalidates review sequence", state: "unpushed", source: true, objects: true, landing: true, otherAllowed: true, invalidate: "review"},
		{name: "claim progresses with saturated reader pool", state: "unpushed", source: true, objects: true, readerFallback: true, saturated: true, otherAllowed: true},
		{name: "claim prepares unknown source and version identities", state: "unpushed", source: true, objects: true, unknown: true, otherAllowed: true},
		{name: "claim uses general reader when claim pool is absent", state: "unpushed", source: true, objects: true, readerFallback: true, otherAllowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			objects := &changeObjectFixture{}
			details := &memoryRunDetails{}
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
			if test.objects {
				cfg.Content, cfg.RunDetails = objects.call, details
			}
			f := newChangeFixture(t, openTestService(t, cfg))
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
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.WorktreeState, event.Data.Handoff.HeadSHA = test.state, input.HeadSHA
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", owner.redemption.Credential, event), http.StatusOK)
			var published tracker.ChangeVersion
			if test.source {
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				if test.objects {
					input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: "stored command output"}
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &published)
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
				allowed, reason, err := placementClaimAllowed(f.service.runDetailContext(t.Context()), f.service.database.db, scope, candidate.runner.binding.MachineID, id, at, nil)
				if err != nil || allowed != candidate.allowed {
					t.Fatalf("%s allowed=%v reason=%q error=%v", candidate.runner.binding.RunnerID, allowed, reason, err)
				}
				if !allowed && !strings.Contains(reason, candidate.reason+" "+string(owner.binding.MachineID)) {
					t.Fatalf("refusal lost the waiting owner: %q", reason)
				}
			}
			if test.objects {
				if test.landing {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+published.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+published.ID+"/checks", f.token, changeTestResult(published)), http.StatusOK)
					states := append(f.project.States, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}})
					raw, err := json.Marshal(states)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET states_json=? WHERE id=?`, string(raw), f.project.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states(project_id,source_name,detent_state,dispatchable,created_at,updated_at) VALUES(?,'Merging','Merging',1,?,?)`, f.project.ID, formatHubTime(now), formatHubTime(now)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND source_name='Merging'),revision=revision+1 WHERE id=?`, f.project.ID, id); err != nil {
						t.Fatal(err)
					}

				}
				var outputKey string
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT object_key FROM change_command_outputs WHERE version_id=?`, published.ID).Scan(&outputKey); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				delete(objects.objects, outputKey)
				objects.reads = 0
				objects.mu.Unlock()
				details.mu.Lock()
				details.reads = 0
				details.mu.Unlock()
				if test.invalidate != "" {
					credential, _, err := f.service.authenticateAPIToken(t.Context(), other.redemption.Credential, "", "")
					if err != nil {
						t.Fatal(err)
					}
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
					prepared := &claimPreparation{versions: make(map[string]preparedClaimVersion), checkpoints: make(map[string]preparedClaimCheckpoint), details: make(map[string]preparedClaimDetail)}
					ctx := f.service.runDetailContext(context.WithValue(t.Context(), claimPreparationKey{}, prepared))
					request := tracker.ClaimRequest{WorkItemID: id, MachineID: other.binding.MachineID, SessionID: "prepared-stale", TTL: time.Minute}
					query := claimCandidateQuery{PolicyID: hubTestPolicy().ID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
					_, err = f.service.database.prepareClaim(ctx, request, query, time.Hour)
					var selected *claimSelected
					if !errors.As(err, &selected) {
						t.Fatalf("prepare claim: %v", err)
					}
					if test.invalidate == "checkpoint" {
						_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET sequence=sequence+1 WHERE id=?`, event.Data.AttemptID)
					} else {
						raw, marshalErr := json.Marshal(tracker.ChangeReview{ID: "new-review", VersionID: published.ID, Decision: "changes_requested", CreatedAt: now})
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, f.change.ID, published.ID, string(raw))
					}
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.service.database.claimNextOn(ctx, f.service.database.db, nil, claimMode{hint: selected}, request, query, time.Hour)
					if !errors.Is(err, errClaimHintStale) {
						t.Fatalf("changed %s granted stale claim: %v", test.invalidate, err)
					}
					var granted int
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM leases WHERE session_id=?`, request.SessionID).Scan(&granted); err != nil || granted != 0 {
						t.Fatalf("stale claim leases=%d error=%v", granted, err)
					}
					return
				}
				if test.readerFallback {
					reader := f.service.database.claimReader
					f.service.database.claimReader = nil
					defer func() { f.service.database.claimReader = reader }()
				}
				if test.unknown {
					if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE id=?`, event.Data.AttemptID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_requests SET claim_version_id=NULL,claim_head_sha=NULL WHERE id=?`, f.change.ID); err != nil {
						t.Fatal(err)
					}
				}
				if test.unknown {
					db := f.service.database
					credential, _, err := f.service.authenticateAPIToken(t.Context(), other.redemption.Credential, "", "")
					if err != nil {
						t.Fatal(err)
					}
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
					prepared := &claimPreparation{versions: make(map[string]preparedClaimVersion), checkpoints: make(map[string]preparedClaimCheckpoint), details: make(map[string]preparedClaimDetail)}
					ctx := f.service.runDetailContext(f.service.contentContext(context.WithValue(t.Context(), claimPreparationKey{}, prepared)))
					request := tracker.ClaimRequest{WorkItemID: id, MachineID: other.binding.MachineID, SessionID: "unknown", TTL: time.Minute}
					query := claimCandidateQuery{PolicyID: hubTestPolicy().ID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
					_, err = db.prepareClaim(ctx, request, query, time.Hour)
					var selected *claimSelected
					if !errors.As(err, &selected) {
						t.Fatalf("unknown preparation: %v", err)
					}
					if err := prepareClaimDecisionPayloads(ctx, prepared); err != nil {
						t.Fatal(err)
					}
					restored, err := db.claimNextOn(ctx, db.db, nil, claimMode{hint: selected}, request, query, time.Hour)
					if err != nil {
						t.Fatalf("unknown writer: %v", err)
					}
					if restored.WorkItemID != id || restored.FencingToken <= lease.FencingToken {
						t.Fatalf("unknown claim: %+v", restored)
					}
					return
				}
				if test.saturated {
					db := f.service.database
					credential, _, err := f.service.authenticateAPIToken(t.Context(), other.redemption.Credential, "", "")
					if err != nil {
						t.Fatal(err)
					}
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
					db.reader.SetMaxOpenConns(1)
					entered := make(chan struct{}, 1)
					released := make(chan struct{})
					var first sync.Once
					objects.beforeRead = func(ctx context.Context) error {
						if claimPrepared(ctx) == nil {
							return nil
						}
						first.Do(func() {
							entered <- struct{}{}
							select {
							case <-released:
							case <-ctx.Done():
							}
						})
						var value int
						return db.reader.QueryRowContext(ctx, "SELECT 1").Scan(&value)
					}
					request := tracker.ClaimRequest{WorkItemID: id, MachineID: other.binding.MachineID, SessionID: "saturated", TTL: time.Minute}
					query := claimCandidateQuery{PolicyID: hubTestPolicy().ID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
					ctx, cancel := context.WithCancel(f.service.runDetailContext(f.service.contentContext(testenv.WaitContext(t, t.Context()))))
					done := make(chan error, 1)
					joined := make(chan struct{})
					var release sync.Once
					releaseRead := func() { release.Do(func() { close(released) }) }
					defer func() {
						cancel()
						releaseRead()
						<-joined
					}()
					go func() {
						defer close(joined)
						_, err := db.claimNativePrepared(ctx, request, query, time.Hour)
						done <- err
					}()
					select {
					case <-entered:
					case err := <-done:
						t.Fatalf("claim did not reach archive fetch: %v", err)
					case <-ctx.Done():
						t.Fatal("claim retained a reader before archive fetch")
					}
					held, err := db.reader.Conn(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if db.reader.Stats().InUse != 1 {
						t.Fatal("reader pool is not saturated")
					}
					if err := held.Close(); err != nil {
						t.Fatal(err)
					}
					releaseRead()
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal("claim waited for its own reader connection")
					}
					var leases int
					if err := db.reader.QueryRowContext(ctx, `SELECT count(*) FROM leases WHERE session_id='saturated'`).Scan(&leases); err != nil || leases != 1 {
						t.Fatalf("leases=%d: %v", leases, err)
					}
					return
				}
				claim.MachineID, claim.SessionID = other.binding.MachineID, "restored"
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", other.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var restored tracker.NativeLease
				decodeHubResponse(t, response, &restored)
				if restored.WorkItemID != f.issue.WorkItemID || restored.FencingToken <= lease.FencingToken {
					t.Fatalf("restored claim = %+v", restored)
				}
				objects.mu.Lock()
				contentReads, contentWriterReads := objects.reads, objects.writerReads
				objects.mu.Unlock()
				details.mu.Lock()
				detailReads, detailWriterReads := details.reads, details.writerReads
				details.mu.Unlock()
				if contentReads == 0 || detailReads != 0 || contentWriterReads != 0 || detailWriterReads != 0 {
					t.Fatalf("content reads=%d writer=%d; detail reads=%d writer=%d", contentReads, contentWriterReads, detailReads, detailWriterReads)
				}
			}

		})
	}
}
