package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func withMutableClaimFact(ctx context.Context, tx *sql.Tx, trigger string, write func() error) error {
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?`, trigger).Scan(&definition); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+trigger); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, definition)
	return err
}

func TestTypedAnsweredClaimFacts(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "claim-facts-worker")
	type execution struct {
		issue tracker.NativeIssue
		data  tracker.NativeRunData
	}
	var executions []execution
	for index := range 2 {
		issue := f.create(t, fmt.Sprintf("claim facts %d", index))
		lease := claimNativeAttempt(t, f, worker, fmt.Sprintf("facts-machine-%d", index), fmt.Sprintf("facts-session-%d", index), issue.WorkItemID)
		start := nativeStartedEvent(lease)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		executions = append(executions, execution{issue: issue, data: start.Data})
	}
	for _, test := range []struct {
		name, status, reason, effect, state, want string
		present, human, blockers, acceptance      bool
	}{
		{name: "human", present: true, status: "blocked", human: true, want: "human_action"},
		{name: "permission", present: true, status: "blocked", reason: "permission_wait", want: "permission_wait"},
		{name: "recorded acceptance", acceptance: true, want: "recorded_acceptance"},
		{name: "continuing acceptance", present: true, status: "in_progress", acceptance: true},
		{name: "blocked acceptance", present: true, status: "in_progress", blockers: true, acceptance: true, want: "recorded_acceptance"},
		{name: "legacy push acceptance", effect: "git_push", state: "confirmed", acceptance: true},
		{name: "pending human", present: true, status: "blocked", human: true, effect: "git_push", state: "pending"},
		{name: "ambiguous permission", present: true, status: "blocked", reason: "permission_wait", effect: "pr_create", state: "ambiguous"},
		{name: "confirmed human", present: true, status: "blocked", human: true, effect: "git_push", state: "confirmed", want: "human_action"},
		{name: "plain success"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, execution := range executions {
				data := execution.data
				data.Runtime = &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: test.acceptance}}
				if test.present {
					data.Disposition = &tracker.NativeDisposition{Status: test.status, HumanAction: test.human, Blockers: test.blockers, ReasonCode: test.reason}
				}
				checkpoint := &tracker.NativeCheckpoint{ExternalEffect: test.effect, EffectState: test.state}
				data.Handoff = checkpoint
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				checkpointRaw, err := json.Marshal(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=?,checkpoint_json=?,status='succeeded' WHERE id=?`, string(raw), string(checkpointRaw), data.AttemptID); err != nil {
					t.Fatal(err)
				}
				if err := updateAttemptDisposition(t.Context(), tx, data.AttemptID, data); err != nil {
					t.Fatal(err)
				}
				if err := updateAttemptClaimFacts(t.Context(), tx, data.AttemptID, data); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				var kind string
				query := `SELECT COALESCE((SELECT ` + nativeAnsweredClassification + ` ` + nativeAnsweredAttemptFrom + `),'') FROM issues i WHERE i.native_id=?`
				if err := f.service.database.reader.QueryRowContext(t.Context(), query, execution.issue.WorkItemID).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != test.want {
					t.Fatalf("typed classification=%q want %q", kind, test.want)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json='{}',checkpoint_json=NULL WHERE id=?`, data.AttemptID); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), query, execution.issue.WorkItemID).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != test.want {
					t.Fatalf("typed query depended on opaque payload: %q", kind)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=?,checkpoint_json=?,claim_facts_known=NULL WHERE id=?`, string(raw), string(checkpointRaw), data.AttemptID); err != nil {
					t.Fatal(err)
				}
				var id tracker.WorkItemID
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT id FROM issues WHERE native_id=?`, execution.issue.WorkItemID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				legacy, err := readUnknownAnsweredClaim(t.Context(), f.service.database.reader, id)
				if err != nil {
					t.Fatal(err)
				}
				if legacy.Kind != test.want {
					t.Fatalf("legacy classification=%q want %q", legacy.Kind, test.want)
				}
				if err := f.service.database.populateClaimFacts(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), query, execution.issue.WorkItemID).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != test.want {
					t.Fatalf("backfill classification=%q want %q", kind, test.want)
				}
			}
		})
	}
}

func TestLandingClaimFactsUnknown(t *testing.T) {
	for _, github := range []bool{false, true} {
		t.Run(strconv.FormatBool(github), func(t *testing.T) {
			f := newLandingCapacityFixture(t, nil, "typed-landings", github)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			first, second := f.ready(t, "first"), f.ready(t, "second")
			owner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events)
			owner.enroll(t)
			response := f.claim(t, owner, first, "landing/batch/first", 1)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			var change, version, head string
			if err := f.service.database.reader.QueryRowContext(f.service.runDetailContext(t.Context()), `SELECT c.id,c.claim_version_id,v.claim_head_sha FROM change_requests c JOIN change_versions v ON v.id=c.claim_version_id WHERE c.work_item_id=?`, first.WorkItemID).Scan(&change, &version, &head); err != nil {
				t.Fatal(err)
			}
			event := nativeStartedEvent(lease)
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(), Landing: &tracker.NativeLandingReceipt{ChangeID: change, VersionID: version, HeadSHA: head, Waiting: true}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/events", owner.redemption.Credential, event), http.StatusOK)
			var firstID, secondID tracker.WorkItemID
			for _, pair := range []struct {
				item tracker.NativeWorkItemID
				out  *tracker.WorkItemID
			}{{first.WorkItemID, &firstID}, {second.WorkItemID, &secondID}} {
				if err := f.service.database.reader.QueryRowContext(f.service.runDetailContext(t.Context()), `SELECT id FROM issues WHERE native_id=?`, pair.item).Scan(pair.out); err != nil {
					t.Fatal(err)
				}
			}
			ctx := f.service.runDetailContext(f.service.runDetailContext(t.Context()))
			var versionRaw string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE id=?", version).Scan(&versionRaw); err != nil {
				t.Fatal(err)
			}
			versionJSON, err := resolveStorageJSON(ctx, []byte(versionRaw))
			if err != nil {
				t.Fatal(err)
			}
			runJSON, err := json.Marshal(readReferencedRun(t, f.service, event.Data.AttemptID).NativeRunData)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET data_json=?,payload_object_key='' WHERE id=?", string(runJSON), event.Data.AttemptID); err != nil {
					return err
				}
				return withMutableClaimFact(ctx, tx, "change_versions_immutable", func() error {
					_, err := tx.ExecContext(ctx, "UPDATE change_versions SET record_json=?,payload_object_key='' WHERE id=?", string(versionJSON), version)
					return err
				})
			}); err != nil {
				t.Fatal(err)
			}
			wantAllowed := !github
			for _, unknown := range []bool{false, true} {
				if unknown {
					if _, err := f.service.database.db.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE native_attempts SET claim_facts_known=NULL,landing_waiting=NULL,landing_head_sha=NULL WHERE id=?`, event.Data.AttemptID); err != nil {
						t.Fatal(err)
					}
					tx, err := f.service.database.db.BeginTx(f.service.runDetailContext(t.Context()), nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := withMutableClaimFact(f.service.runDetailContext(t.Context()), tx, "change_versions_immutable", func() error {
						_, err := tx.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE change_versions SET claim_head_sha=NULL,gate_github_pull_request=NULL WHERE change_id IN (SELECT id FROM change_requests WHERE project_id=?)`, f.project.ID)
						return err
					}); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE policy_revisions SET gate_github_pull_request=NULL WHERE scope=?`, string(f.project.OrganizationID)+"/"+string(f.project.ID)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE change_requests SET claim_version_id=NULL,claim_head_sha=NULL,landed_version_id=NULL WHERE project_id=?`, f.project.ID); err != nil {
						t.Fatal(err)
					}
				}
				for _, test := range []struct {
					name, statement    string
					args               []any
					immutable, matches bool
				}{
					{name: "matching receipt", matches: true},
					{name: "not waiting", statement: `UPDATE native_attempts SET landing_waiting=0,data_json=json_set(data_json,'$.runtime.landing.waiting',json('false')) WHERE id=?`, args: []any{event.Data.AttemptID}},
					{name: "other change", statement: `UPDATE native_attempts SET landing_change_id='other-change',data_json=json_set(data_json,'$.runtime.landing.change_id','other-change') WHERE id=?`, args: []any{event.Data.AttemptID}},
					{name: "other version", statement: `UPDATE native_attempts SET landing_version_id='other-version',data_json=json_set(data_json,'$.runtime.landing.version_id','other-version') WHERE id=?`, args: []any{event.Data.AttemptID}},
					{name: "other receipt head", statement: `UPDATE native_attempts SET landing_head_sha='other-head',data_json=json_set(data_json,'$.runtime.landing.head_sha','other-head') WHERE id=?`, args: []any{event.Data.AttemptID}},
					{name: "already landed", statement: `UPDATE change_requests SET landed_version_id=?,record_json=json_set(record_json,'$.landed.version_id',?) WHERE id=?`, args: []any{version, version, change}},
					{name: "no current version", statement: `UPDATE change_requests SET claim_version_id='',record_json=json_set(record_json,'$.current_version_id','') WHERE id=?`, args: []any{change}},
					{name: "frozen gate disabled", statement: `UPDATE policy_revisions SET gate_github_pull_request=0,metadata_json=json_set(metadata_json,'$.gates.github_pull_request',json('false')) WHERE scope=?`, args: []any{string(f.project.OrganizationID) + "/" + string(f.project.ID)}},
					{name: "other version head", statement: `UPDATE change_versions SET claim_head_sha='other-head',record_json=json_set(record_json,'$.head_sha','other-head') WHERE id=?`, args: []any{version}, immutable: true},
					{name: "legacy version policy fallback", statement: `UPDATE change_versions SET policy_revision_id='unrecorded-policy',record_json=json_set(record_json,'$.policy.gates.github_pull_request',json(?)) WHERE id=?`, args: []any{strconv.FormatBool(github), version}, immutable: true, matches: true},
					{name: "version gate disabled", statement: `UPDATE change_versions SET policy_revision_id='unrecorded-policy',gate_github_pull_request=0,record_json=json_set(record_json,'$.policy.gates.github_pull_request',json('false')) WHERE id=?`, args: []any{version}, immutable: true},
				} {
					t.Run(fmt.Sprintf("unknown=%v/%s", unknown, test.name), func(t *testing.T) {
						tx, err := f.service.database.db.BeginTx(f.service.runDetailContext(t.Context()), nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if test.statement != "" {
							write := func() error {
								_, err := tx.ExecContext(f.service.runDetailContext(t.Context()), test.statement, test.args...)
								return err
							}
							if test.immutable {
								err = withMutableClaimFact(f.service.runDetailContext(t.Context()), tx, "change_versions_immutable", write)
							} else {
								err = write()
							}
							if err != nil {
								t.Fatal(err)
							}
						}
						if !unknown {
							if _, err := tx.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE native_attempts SET data_json='{}' WHERE id=?`, event.Data.AttemptID); err != nil {
								t.Fatal(err)
							}
						}
						_, pending, err := readPendingLandingFact(f.service.runDetailContext(t.Context()), tx, firstID)
						if err != nil {
							t.Fatal(err)
						}
						if pending != (github && test.matches) {
							t.Fatalf("pending=%v github=%v matches=%v", pending, github, test.matches)
						}
					})
				}
				if unknown {
					t.Run("transaction projection without resolvable version policy", func(t *testing.T) {
						tx, err := f.service.database.db.BeginTx(f.service.runDetailContext(t.Context()), nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if err := withMutableClaimFact(f.service.runDetailContext(t.Context()), tx, "change_versions_immutable", func() error {
							_, err := tx.ExecContext(f.service.runDetailContext(t.Context()), `UPDATE change_versions SET policy_revision_id='unrecorded-policy',record_json=json_remove(record_json,'$.policy') WHERE id=?`, version)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						if _, _, err := readPendingLandingFact(f.service.runDetailContext(t.Context()), tx, firstID); !errors.Is(err, errClaimHintStale) {
							t.Fatalf("landing fact error = %v, want stale claim hint", err)
						}
						if _, _, err := readNativeIssueProjection(f.service.runDetailContext(t.Context()), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(first.WorkItemID), true); !errors.Is(err, errClaimHintStale) {
							t.Fatalf("execution-facing projection error = %v, want stale claim hint", err)
						}
						issue, _, err := readNativeIssueProjection(withDisplayLandingFacts(f.service.runDetailContext(t.Context())), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(first.WorkItemID), true)
						if err != nil {
							t.Fatalf("board display projection: %v", err)
						}
						if issue.ListChange != nil && issue.ListChange.LandingPending {
							t.Fatal("unresolved landing fact reported pending")
						}
						prepared := context.WithValue(withDisplayLandingFacts(f.service.runDetailContext(t.Context())), claimPreparationKey{}, &claimPreparation{})
						if _, _, err := readNativeIssueProjection(prepared, tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(first.WorkItemID), true); err == nil {
							t.Fatal("claim-prepared projection resolved an unprepared landing fact")
						}
					})
				}
				_, pending, err := readPendingLandingFact(f.service.runDetailContext(t.Context()), f.service.database.reader, firstID)
				if err != nil {
					t.Fatal(err)
				}
				if pending != github {
					t.Fatalf("unknown=%v pending=%v want=%v", unknown, pending, github)
				}
				allowed, _, err := nativeLandingClaimCapacity(f.service.runDetailContext(t.Context()), f.service.database.reader, secondID, 1, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				if allowed != wantAllowed {
					t.Fatalf("unknown=%v allowed=%v want=%v", unknown, allowed, wantAllowed)
				}
				key, err := nativeLandingCapacityKey(f.service.runDetailContext(t.Context()), f.service.database.reader, owner.binding.MachineID, "landing/batch/second", firstID)
				if err != nil {
					t.Fatal(err)
				}
				if (key != "") != !github {
					t.Fatalf("unknown=%v grouped=%q", unknown, key)
				}
				if unknown {
					if err := f.service.database.populateClaimFacts(f.service.runDetailContext(t.Context())); err != nil {
						t.Fatal(err)
					}
					allowed, _, err := nativeLandingClaimCapacity(f.service.runDetailContext(t.Context()), f.service.database.reader, secondID, 1, f.service.config.now())
					if err != nil {
						t.Fatal(err)
					}
					if allowed != wantAllowed {
						t.Fatalf("backfill changed capacity: %v", allowed)
					}
				}
			}
			t.Run("archived legacy policy", func(t *testing.T) {
				objects := &changeObjectFixture{}
				originalContent := f.service.config.Content
				f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
					if !request.Write && f.service.database.db.Stats().InUse != 0 {
						return nil, errors.New("archive read with writer connection held")
					}
					if !request.Write {
						objects.mu.Lock()
						_, found := objects.objects[request.Reference.Key]
						objects.mu.Unlock()
						if !found {
							return originalContent(ctx, request)
						}
					}
					return objects.call(ctx, request)
				}
				ctx := f.service.contentContext(f.service.runDetailContext(t.Context()))
				for _, item := range []tracker.NativeIssue{first, second} {
					var row int64
					var raw string
					if err := f.service.database.reader.QueryRowContext(ctx, `SELECT v.rowid,COALESCE(NULLIF(v.payload_object_key,''),v.record_json) FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE c.work_item_id=?`, item.WorkItemID).Scan(&row, &raw); err != nil {
						t.Fatal(err)
					}
					var legacy tracker.ChangeVersion
					if err := decodeStorageJSON(ctx, []byte(raw), &legacy); err != nil {
						t.Fatal(err)
					}
					legacy.Policy = f.descriptor
					legacy.Validation = &gate.CommandResult{Output: strings.Repeat("retained command output\n", 128)}
					raw, err := marshalNative(legacy)
					if err != nil {
						t.Fatal(err)
					}
					converted, _, err := f.service.prepareStorageConversion(ctx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, storageConversionFamily{name: "versions"}, row, []byte(raw))
					if err != nil || converted == nil {
						t.Fatalf("convert legacy version: converted=%v error=%v", converted != nil, err)
					}
					var envelope map[string]json.RawMessage
					if err := json.Unmarshal(converted, &envelope); err != nil {
						t.Fatal(err)
					}
					if _, present := envelope["policy"]; present {
						t.Fatal("converted version retained inline policy")
					}
					tx, err := f.service.database.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if err := withMutableClaimFact(ctx, tx, "change_versions_immutable", func() error {
						_, err := tx.ExecContext(ctx, `UPDATE change_versions SET record_json=?,payload_object_key='',policy_revision_id='unrecorded-policy',gate_github_pull_request=NULL WHERE rowid=?`, string(converted), row)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				for _, unavailable := range []context.Context{t.Context(), context.WithValue(ctx, explanationReadKey{}, true)} {
					if allowed, _, err := nativeLandingClaimCapacity(unavailable, f.service.database.reader, secondID, 1, f.service.config.now()); err == nil || allowed {
						t.Fatalf("unavailable archive authorized capacity: allowed=%v error=%v", allowed, err)
					}
					if key, err := nativeLandingCapacityKey(unavailable, f.service.database.reader, owner.binding.MachineID, "landing/batch/second", firstID); err == nil || key != "" {
						t.Fatalf("unavailable archive authorized grouping: key=%q error=%v", key, err)
					}
					if err := f.service.database.populateClaimFacts(unavailable); err == nil {
						t.Fatal("backfill accepted unavailable archive")
					}
					var known int
					if err := f.service.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM change_versions WHERE change_id IN (SELECT id FROM change_requests WHERE project_id=?) AND gate_github_pull_request IS NOT NULL`, f.project.ID).Scan(&known); err != nil || known != 0 {
						t.Fatalf("unavailable policy became known: count=%d error=%v", known, err)
					}
				}
				prepared := &claimPreparation{hydrating: true}
				preparedCtx := context.WithValue(ctx, claimPreparationKey{}, prepared)
				if _, err := nativeLandingCapacityKey(preparedCtx, f.service.database.reader, owner.binding.MachineID, "landing/batch/second", firstID); err != nil {
					t.Fatal(err)
				}
				prepared.hydrating = false
				tx, err := f.service.database.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if key, err := nativeLandingCapacityKey(ctx, tx, owner.binding.MachineID, "landing/batch/second", firstID); !errors.Is(err, errClaimHintStale) || key != "" {
					t.Fatalf("unprepared writer accepted archived policy: key=%q error=%v", key, err)
				}
				key, err := nativeLandingCapacityKey(context.WithValue(preparedCtx, claimWriterKey{}, true), tx, owner.binding.MachineID, "landing/batch/second", firstID)
				if err != nil || (key != "") != !github {
					t.Fatalf("prepared archived policy: key=%q error=%v", key, err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				for _, backfilled := range []bool{false, true} {
					readCtx := ctx
					if backfilled {
						if err := f.service.maintainNativeRetention(f.service.runDetailContext(t.Context()), f.service.config.now()); err != nil {
							t.Fatal(err)
						}
						var known int
						if err := f.service.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM change_versions WHERE change_id IN (SELECT id FROM change_requests WHERE project_id=?) AND gate_github_pull_request=?`, f.project.ID, github).Scan(&known); err != nil || known != 2 {
							t.Fatalf("archived gates not backfilled: count=%d error=%v", known, err)
						}
						readCtx = f.service.runDetailContext(t.Context())
					}
					_, pending, err := readPendingLandingFact(readCtx, f.service.database.reader, firstID)
					if err != nil || pending != github {
						t.Fatalf("backfilled=%v pending=%v error=%v", backfilled, pending, err)
					}
					allowed, _, err := nativeLandingClaimCapacity(readCtx, f.service.database.reader, secondID, 1, f.service.config.now())
					if err != nil || allowed != wantAllowed {
						t.Fatalf("backfilled=%v allowed=%v error=%v", backfilled, allowed, err)
					}
					key, err := nativeLandingCapacityKey(readCtx, f.service.database.reader, owner.binding.MachineID, "landing/batch/second", firstID)
					if err != nil || (key != "") != !github {
						t.Fatalf("backfilled=%v grouping=%q error=%v", backfilled, key, err)
					}
				}
			})
		})
	}
}

func TestAdmissionClaimBackfillBound(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	first, second := f.create(t, "first backfill"), f.create(t, "second backfill")
	db := f.service.database
	if err := db.populateClaimFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	for index, item := range []tracker.NativeIssue{first, second} {
		if _, err := db.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,run_attempt_id) SELECT ?||x,?,?,?,x+100,'run.observed',1,'{}',?,?,? FROM n`, claimFactsRowsPerTick+1, fmt.Sprintf("legacy-%d-", index), f.project.OrganizationID, f.project.ID, item.WorkItemID, fmt.Sprintf(`{"run":{"attempt_id":"historical-attempt-%d"}}`, index), testTimestamp, fmt.Sprintf("historical-attempt-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	observationCount := func(item tracker.NativeIssue, attempt string) int {
		t.Helper()
		var count int
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='run.observed' AND claim_attempt_id=?`, f.project.OrganizationID, f.project.ID, item.WorkItemID, attempt).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if count := observationCount(first, "historical-attempt-0"); count != 0 {
		t.Fatalf("unknown attempt fact matched observation lookup: %d", count)
	}
	if _, err := db.db.ExecContext(t.Context(), `CREATE TRIGGER interrupt_claim_backfill BEFORE UPDATE OF claim_attempt_id ON collaboration_events WHEN NEW.id LIKE 'legacy-%' AND (SELECT count(*) FROM collaboration_events WHERE id LIKE 'legacy-%' AND claim_attempt_id IS NOT NULL)=1 BEGIN SELECT RAISE(ABORT,'interrupted claim backfill'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.populateClaimFacts(t.Context()); err == nil {
		t.Fatal("expected backfill interruption")
	}
	var committed int
	if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE id LIKE 'legacy-%' AND claim_attempt_id IS NOT NULL`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Fatalf("backfill did not commit one item: %d", committed)
	}
	if _, err := db.db.ExecContext(t.Context(), `DROP TRIGGER interrupt_claim_backfill`); err != nil {
		t.Fatal(err)
	}
	for tick := 1; tick <= 3; tick++ {
		if err := db.populateClaimFacts(t.Context()); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE claim_attempt_id IS NULL`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		want := max(0, 2*(claimFactsRowsPerTick+1)-1-tick*claimFactsRowsPerTick)
		if remaining != want {
			t.Fatalf("tick=%d remaining=%d want=%d", tick, remaining, want)
		}
	}
	var attempt sql.NullString
	if err := db.reader.QueryRowContext(t.Context(), `SELECT claim_attempt_id FROM collaboration_events WHERE id='legacy-1-1001'`).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if !attempt.Valid || attempt.String != "historical-attempt-1" {
		t.Fatalf("backfill did not preserve attempt: %+v", attempt)
	}
	tx, err := db.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := withMutableClaimFact(t.Context(), tx, "collaboration_events_no_update", func() error {
		_, err := tx.ExecContext(t.Context(), `UPDATE collaboration_events SET data_json='{}' WHERE id LIKE 'legacy-%'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for index, item := range []tracker.NativeIssue{first, second} {
		if count := observationCount(item, fmt.Sprintf("historical-attempt-%d", index)); count != claimFactsRowsPerTick+1 {
			t.Fatalf("typed observation lookup lost rows for item %d: %d", index, count)
		}
		if count := observationCount(item, fmt.Sprintf("historical-attempt-%d", 1-index)); count != 0 {
			t.Fatalf("observation lookup crossed work items: %d", count)
		}
	}
}

func TestTypedRunnerAdmissionFacts(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC()
	var runners [2]runnerFixture
	legacy := make(map[tracker.MachineID]map[string]map[string]tracker.NativeAdmissionObservation)
	for index := range 2 {
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
		runner.enroll(t)
		runners[index] = runner
		legacy[runner.binding.MachineID] = map[string]map[string]tracker.NativeAdmissionObservation{runner.binding.RunnerID: {}}
		for projectIndex := range 2 {
			project := fmt.Sprintf("project-%d", projectIndex)
			observation := tracker.NativeAdmissionObservation{RunnerRevision: int64(index + 1), ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, LastRefreshAt: now, RefreshDuration: time.Millisecond, PolicyID: fmt.Sprintf("policy-%d-%d", index, projectIndex), WorkflowStates: []string{"Todo", "Rework"}, Authors: []string{"author-one", "author-two"}, Assignees: []string{"assignee-one"}, LabelInclude: []string{"include"}, LabelExclude: []string{"exclude"}}}
			legacy[runner.binding.MachineID][runner.binding.RunnerID][project] = observation
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := storeAdmissionFacts(t.Context(), tx, runner.binding.RunnerID, tracker.ProjectID(project), observation); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertObservations := func(stage string) {
		t.Helper()
		for index, runner := range runners {
			for projectIndex := range 2 {
				project := fmt.Sprintf("project-%d", projectIndex)
				scope := nativeScope{organization: f.project.OrganizationID, project: tracker.ProjectID(project)}
				observed, err := readAdmissionFacts(t.Context(), f.service.database.reader, runner.binding.RunnerID, scope.project, runner.binding.MachineID, scope.organization)
				if err != nil {
					t.Fatal(err)
				}
				want := tracker.NativeAdmissionObservation{RunnerRevision: int64(index + 1), ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, LastRefreshAt: now, RefreshDuration: time.Millisecond, PolicyID: fmt.Sprintf("policy-%d-%d", index, projectIndex), WorkflowStates: []string{"Todo", "Rework"}, Authors: []string{"author-one", "author-two"}, Assignees: []string{"assignee-one"}, LabelInclude: []string{"include"}, LabelExclude: []string{"exclude"}}}
				if !reflect.DeepEqual(observed, want) {
					t.Fatalf("stage=%s runner=%d project=%d observed=%+v want=%+v", stage, index, projectIndex, observed, want)
				}
			}
		}
	}
	assertObservations("written")
	if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM runner_admission_observations`); err != nil {
		t.Fatal(err)
	}
	for _, runner := range runners {
		raw, err := json.Marshal(map[string]any{"native_admission": legacy[runner.binding.MachineID]})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=?,admission_facts_ready=NULL WHERE id=?`, string(raw), runner.binding.MachineID); err != nil {
			t.Fatal(err)
		}
	}
	assertObservations("unknown")
	if _, err := readAdmissionFacts(t.Context(), f.service.database.reader, runners[0].binding.RunnerID, "project-0", runners[0].binding.MachineID, "other-organization"); err == nil {
		t.Fatal("cross-organization observation fallback succeeded")
	}
	if err := f.service.database.populateRunnerFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertObservations("backfilled")
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json='{}' WHERE organization_id=?`, f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	assertObservations("opaque removed")
	for _, runner := range runners {
		admissions, err := readHealthAdmissionFacts(t.Context(), f.service.database.reader, f.project.OrganizationID, runner.binding.RunnerID, runner.binding.MachineID)
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{"project-0", "project-1"} {
			observation := admissions[project]
			if !observation.Context.LastRefreshAt.Equal(now) || observation.Context.RefreshDuration != time.Millisecond {
				t.Fatalf("health lost typed refresh facts for %s: %+v", project, observation)
			}
		}
	}
}
