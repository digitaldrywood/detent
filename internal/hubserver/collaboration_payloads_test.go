package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func appendLegacyNativeHistory(ctx context.Context, tx *sql.Tx, scope nativeScope, workItemID string, eventType string, data tracker.CollaborationData, now time.Time) error {
	var sequence int64
	if err := tx.QueryRowContext(ctx, "UPDATE issues SET event_sequence = event_sequence + 1 WHERE organization_id = ? AND project_id = ? AND native_id = ? RETURNING event_sequence", scope.organization, scope.project, workItemID).Scan(&sequence); err != nil {
		return err
	}
	actor, err := marshalNative(scope.actor())
	if err != nil {
		return err
	}
	payload, err := marshalNative(data)
	if data.Run != nil {
		run, encodeErr := marshalNative(*data.Run)
		if encodeErr != nil {
			return encodeErr
		}
		payload, err = marshalNative(struct {
			tracker.CollaborationData
			Run json.RawMessage `json:"run"`
		}{CollaborationData: data, Run: json.RawMessage(run)})
	}
	if err != nil {
		return err
	}
	id := newNativeID("evt")
	decision := tracker.NativeSchedulerDecision{}
	if data.Decision != nil {
		decision = *data.Decision
	}
	key := analyticsDimension(decision.Source, decision.Reason)
	attempt := ""
	if data.Run != nil {
		attempt = data.Run.AttemptID
	}
	metadata, err := activityQueryMetadata([]byte(payload))
	if err != nil {
		return err
	}
	archiveValues, err := archiveEventValues(payload)
	if err != nil {
		return err
	}
	args := []any{id, scope.organization, scope.project, workItemID, sequence, eventType, actor, payload, formatHubTime(now), decision.Outcome, decision.Reason, decision.Source, decision.RunnerID, nullString(data.FromState), nullString(data.ToState), attempt, key, now.UnixNano(), metadata.values[0], metadata.values[1]}
	args = append(args, eventClaimFacts(data.Run)...)
	args = append(args, archiveValues...)
	_, err = tx.ExecContext(ctx, "INSERT INTO collaboration_events (id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,scheduler_outcome,scheduler_reason,scheduler_source,scheduler_runner_id,analytics_from_state,analytics_to_state,analytics_attempt_id,analytics_skip_key,analytics_recorded_ns,analytics_projection_version,query_metadata_version,run_attempt_id,activity_spans_present,claim_attempt_id,run_id,run_lease_id,run_fencing_token,run_sequence,run_policy_id,push_change_id,push_version_id,push_head_sha,push_external_effect,push_effect_state,archive_operation,archive_from_state,archive_to_state,archive_attempt_id,archive_outcome,archive_landed_commit) VALUES(?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,1,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", args...)
	if err != nil {
		return err
	}

	if err := updateEventPageFacts(ctx, tx, id, scope.actor(), data); err != nil {
		return err
	}
	if err := updateHealthEventProjection(ctx, tx, id, scope.actor(), data); err != nil {
		return err
	}
	if err := writeChangeColumns(ctx, tx, changeColumnFamily{table: "collaboration_events", key: "id", payload: "data_json"}, id, payload, changeEventColumns(data)); err != nil {
		return err
	}
	landed := data.Run != nil && data.Run.Runtime != nil && data.Run.Runtime.Landing != nil && data.Run.Runtime.Landing.Landed
	return insertHealthEventTime(ctx, tx, id, string(scope.organization), eventType, landed, now)
}

func TestCollaborationPayloadInlineBytes(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }})
	rowBytes := collaborationInlineBytesQuery(t, f.service.database.reader)
	for _, kind := range []string{"run.started", "run.checkpointed", "run.observed", "run.finished", "scheduler.decision"} {
		t.Run(kind, func(t *testing.T) {
			var sizes []int
			payloadSizes := []int{16, 256 << 10}
			if kind == "scheduler.decision" {
				payloadSizes = []int{0}
			}
			for _, size := range payloadSizes {
				issue := f.create(t, fmt.Sprintf("object payload %s %d", kind, size))
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				scope.credential.ID = defaultHubOperatorID
				ctx := f.service.runDetailContext(t.Context())
				owner := ctx.Value(runDetailContextKey{}).(*runDetailContext)
				data := tracker.CollaborationData{Revision: issue.Revision}
				if kind == "scheduler.decision" {
					data.Decision = &tracker.NativeSchedulerDecision{RunnerID: "runner", Source: "native_claim", Outcome: "skipped", Reason: "capacity", At: f.service.config.now(), WorkItemRevision: issue.Revision}
					data.Change = &tracker.NativeChangeReference{ChangeID: "change", VersionID: "version", HeadSHA: "head"}
					data.ReasonDetail = "retain scheduler audit context"
					prepared := &claimPreparation{decisions: []preparedClaimDecision{{scope: scope, item: string(issue.WorkItemID), data: data}}}
					ctx = context.WithValue(ctx, claimPreparationKey{}, prepared)
					if err := prepareClaimDecisionPayloads(ctx, prepared); err != nil {
						t.Fatal(err)
					}
				} else {
					run := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: "lease", PolicyID: "policy", FencingToken: 1, Sequence: 1, CompletionBody: strings.Repeat("x", size)}
					data.Run = &run
					owner.organization = string(scope.organization)
					owner.runs = map[string]tracker.NativeRunData{runDetailEntry(run): run}
					raw, err := json.Marshal(owner.runs)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := owner.store.StoreDetail(ctx, owner.organization, raw)
					if err != nil {
						t.Fatal(err)
					}
					owner.reference = &ref
				}
				err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
					if data.Run != nil {
						if err := recordRunDetailReference(ctx, tx, scope, issue.WorkItemID, *data.Run, formatHubTime(now)); err != nil {
							return err
						}
					}
					return appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), kind, data, now)
				})
				if err != nil {
					t.Fatal(err)
				}
				var inline int
				if err := f.service.database.reader.QueryRowContext(ctx, `SELECT length(CAST(data_json AS BLOB)) FROM collaboration_events WHERE work_item_id=? AND type=?`, issue.WorkItemID, kind).Scan(&inline); err != nil {
					t.Fatal(err)
				}
				if inline != 2 {
					t.Fatalf("%d payload bytes stored inline for %d-byte payload, want 2", inline, size)
				}
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT "+rowBytes+" FROM collaboration_events WHERE work_item_id=? AND type=?", issue.WorkItemID, kind).Scan(&inline); err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, inline)
				page, err := f.service.readHistory(f.service.freshRunDetailContext(t.Context()), scope, string(issue.WorkItemID), url.Values{"limit": {"100"}})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) != 2 {
					t.Fatalf("history items=%d, want 2", len(page.Items))
				}
				if kind == "scheduler.decision" {
					for _, lightweight := range []bool{false, true} {
						ctx := context.WithValue(t.Context(), explanationReadKey{}, lightweight)
						runtime, err := f.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
						if err != nil {
							t.Fatal(err)
						}
						want := page.Items[1].Data
						if lightweight {
							want.ReasonDetail = ""
						}
						for name, event := range map[string]*tracker.CollaborationEvent{"latest decision": runtime.LatestDecision, "candidate exclusion": runtime.NativeCandidateExclusion} {
							if event == nil || !reflect.DeepEqual(event.Data, want) {
								t.Errorf("%s payload differs from history (lightweight=%v): got %+v, want %+v", name, lightweight, event, want)
							}
						}
					}
				}
				if size < 1024 || kind == "scheduler.decision" {
					if !reflect.DeepEqual(page.Items[1].Data, data) {
						t.Fatal("history differs from original payload")
					}
				} else if page.Items[1].DataOmission == nil {
					t.Fatal("large history payload was not bounded")
				}
				var locator string
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT "+collaborationPayloadColumn("")+" FROM collaboration_events WHERE work_item_id=? AND type=?", issue.WorkItemID, kind).Scan(&locator); err != nil {
					t.Fatal(err)
				}
				var decoded tracker.CollaborationData
				if err := decodeRunHistory(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, locator, &decoded); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded, data) {
					t.Fatal("object payload differs from input")
				}
				var queued int
				if err := f.service.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM storage_conversion_progress WHERE family='runs' AND aggregate_id=?`, issue.WorkItemID).Scan(&queued); err != nil || queued != 0 {
					t.Fatalf("new event queued for conversion: %d, %v", queued, err)
				}
			}
			if len(sizes) > 1 && sizes[0] != sizes[1] {
				t.Fatalf("inline payload grows: %v", sizes)
			}
		})
	}
}

func collaborationInlineBytesQuery(t *testing.T, q nativeQueryer) string {
	t.Helper()
	rows, err := q.QueryContext(t.Context(), "SELECT name FROM pragma_table_info('collaboration_events')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, `coalesce(length(CAST("`+column+`" AS BLOB)),0)`)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(columns, "+")
}

func restoreLegacyCollaborationPayloads(t *testing.T, service *Service, scope nativeScope) {
	t.Helper()
	ctx := service.runDetailContext(t.Context())
	rows, err := service.database.reader.QueryContext(ctx, "SELECT id,"+collaborationPayloadColumn("")+" FROM collaboration_events WHERE organization_id=? AND project_id=?", scope.organization, scope.project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type record struct{ id, raw string }
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for i := range records {
		var data tracker.CollaborationData
		if err := decodeRunHistory(ctx, service.database.reader, records[i].raw, &data); err != nil {
			t.Fatal(err)
		}
		records[i].raw, err = marshalNative(data)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		var trigger string
		if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE name='collaboration_events_no_update'").Scan(&trigger); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER collaboration_events_no_update"); err != nil {
			return err
		}
		for _, r := range records {
			if _, err := tx.ExecContext(ctx, "UPDATE collaboration_events SET data_json=?,payload_version=0,payload_attachment_id='',payload_entry='',payload_object_key='',payload_sha256='',payload_bytes=0,payload_content_bytes=0 WHERE id=?", r.raw, r.id); err != nil {
				return err
			}
			metadata, err := activityQueryMetadata([]byte(r.raw))
			if err != nil {
				return err
			}
			if err := metadata.write(ctx, tx, "collaboration_events", "id=?", r.id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, trigger)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollaborationPayloadConversion(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }})
	objects := &changeObjectFixture{}
	f.service.config.Content = objects.call
	f.service.database.reader.SetMaxOpenConns(1)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	ctx := f.service.runDetailContext(t.Context())
	type expectedEvent struct {
		id   string
		data tracker.CollaborationData
	}
	var events []expectedEvent
	for _, kind := range []string{"run.started", "run.checkpointed", "run.observed", "run.finished", "scheduler.decision"} {
		for _, size := range []int{16, 64 << 10} {
			issue := f.create(t, fmt.Sprintf("legacy payload %s %d", kind, size))
			data := tracker.CollaborationData{Revision: issue.Revision}
			if kind == "scheduler.decision" {
				data.Decision = &tracker.NativeSchedulerDecision{RunnerID: "runner", Source: "native_provider_capacity", Outcome: "skipped", Reason: "full", At: f.service.config.now(), WorkItemRevision: issue.Revision}
				data.Change = &tracker.NativeChangeReference{ChangeID: "change", VersionID: "version", HeadSHA: "head"}
				data.ReasonDetail = strings.Repeat("scheduler context", size)
			} else {
				data.Run = &tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), Sequence: 1, CompletionBody: strings.Repeat("x", size)}
				data.ReasonDetail = strings.Repeat("legacy context", size)
			}
			if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
				return appendLegacyNativeHistory(ctx, tx, scope, string(issue.WorkItemID), kind, data, now)
			}); err != nil {
				t.Fatal(err)
			}
			var id, original string
			var row int64
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT id,rowid,data_json FROM collaboration_events WHERE work_item_id=? AND type=?", issue.WorkItemID, kind).Scan(&id, &row, &original); err != nil {
				t.Fatal(err)
			}
			if data.Run != nil && size > 16 {
				payload, err := json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "runs", Row: row, Data: json.RawMessage(original)})
				if err != nil {
					t.Fatal(err)
				}
				ref, err := prepareChangeContent(ctx, string(scope.organization), payload)
				if err != nil {
					t.Fatal(err)
				}
				compact, err := compactStorageJSON([]byte(original), storageJSONReference{Organization: string(scope.organization), Project: string(scope.project), Family: "runs", Row: row, Reference: ref})
				if err != nil {
					t.Fatal(err)
				}
				err = f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
					before := storageConversionDigest(original, int64(0), "", "", "", "", int64(0), int64(0))
					after := storageConversionDigest(compact, int64(0), "", "", "", "", int64(0), int64(0))
					if _, err := tx.ExecContext(ctx, "UPDATE storage_conversion_progress SET conversion_rowid=?,conversion_before_sha256=?,conversion_after_sha256=? WHERE family='runs' AND aggregate_id=?", row, before, after, issue.WorkItemID); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, "UPDATE collaboration_events SET data_json=? WHERE id=?", string(compact), id); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, "UPDATE storage_conversion_progress SET conversion_rowid=0,conversion_before_sha256='',conversion_after_sha256='' WHERE family='runs' AND aggregate_id=?", issue.WorkItemID)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			events = append(events, expectedEvent{id: id, data: data})
		}
	}
	if _, err := f.service.database.db.ExecContext(ctx, "UPDATE collaboration_events SET conversion_state=2 WHERE type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(ctx, "UPDATE storage_conversion_progress SET cursor_rowid=through_rowid,complete=1 WHERE family='runs'"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.discoverStorageFamily(ctx, storageConversionFamilies[0], f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "archive owner")
	for i := range 4 {
		if _, err := f.service.database.db.ExecContext(ctx, "INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at) VALUES(?,?,'issue_archives',?,1,0,'')", other.project.OrganizationID, other.project.ID, fmt.Sprintf("example-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.database.db.ExecContext(ctx, "UPDATE storage_conversion_progress SET updated_at='1' WHERE family='runs'"); err != nil {
		t.Fatal(err)
	}
	scopes, err := f.service.database.reader.QueryContext(ctx, storageConversionScopeDiscovery)
	if err != nil {
		t.Fatal(err)
	}
	defer scopes.Close()
	selected := 0
	for scopes.Next() {
		var org, project string
		if err := scopes.Scan(&org, &project); err != nil {
			t.Fatal(err)
		}
		if org != string(scope.organization) || project != string(scope.project) {
			t.Fatal("another archive family crowded out run conversion")
		}
		selected++
	}
	if err := errors.Join(scopes.Err(), scopes.Close()); err != nil {
		t.Fatal(err)
	}
	if selected != 4 {
		t.Fatalf("pending scope entries=%d, want bounded batch of 4", selected)
	}
	plan := explainQueryPlan(t, f.service.database.reader, storageConversionScopeDiscovery, nil)
	if !strings.Contains(strings.Join(plan, " "), "storage_conversion_pending_scopes") {
		t.Fatalf("scope discovery missed sparse index: %v", plan)
	}
	for range events {
		if err := f.service.convertStorageFamily(ctx, scope, storageConversionFamilies[0], f.service.config.now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range events {
		t.Run(expected.id, func(t *testing.T) {
			var raw string
			var inline, version int
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT "+collaborationPayloadColumn("")+",length(CAST(data_json AS BLOB)),payload_version FROM collaboration_events WHERE id=?", expected.id).Scan(&raw, &inline, &version); err != nil {
				t.Fatal(err)
			}
			if inline != 2 || version != 1 {
				t.Fatalf("inline bytes=%d version=%d, want 2 and 1", inline, version)
			}
			ref := contentReference()
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE id=?", expected.id).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize); err != nil {
				t.Fatal(err)
			}
			if _, err := readChangeContent(ctx, string(scope.organization), ref); err != nil {
				t.Fatalf("converted payload has no readable archive: %v", err)
			}
			var got tracker.CollaborationData
			if err := decodeRunHistory(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected.data) {
				t.Fatal("converted payload differs from legacy data")
			}
		})
	}
	before := objects.writes
	if err := f.service.discoverStorageFamily(ctx, storageConversionFamilies[0], f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertStorageFamily(ctx, scope, storageConversionFamilies[0], f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if objects.writes != before {
		t.Fatal("completed conversion uploaded duplicate objects")
	}
}

func TestSchedulerPayloadRequiresArchive(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "scheduler archive required")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "scheduler-archive")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "scheduler-machine", "hostname": "scheduler-machine", "version": "test", "capacity": 1}), http.StatusOK)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	data := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "capacity"}, ReasonDetail: "retain complete scheduler context"}
	t.Run("write without prepared archive", func(t *testing.T) {
		err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
			return appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", data, now)
		})
		if err == nil {
			t.Fatal("scheduler write succeeded without an archive")
		}
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'", issue.WorkItemID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed write committed %d scheduler events: %v", count, err)
		}
	})
	ctx := f.service.runDetailContext(t.Context())
	if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		return appendLegacyNativeHistory(ctx, tx, scope, string(issue.WorkItemID), "scheduler.decision", data, now)
	}); err != nil {
		t.Fatal(err)
	}
	var row int64
	var raw string
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT rowid,data_json FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'", issue.WorkItemID).Scan(&row, &raw); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"missing storage", "upload rejected", "verification failed"} {
		t.Run(failure, func(t *testing.T) {
			objects := &changeObjectFixture{}
			original := f.service.config.Content
			t.Cleanup(func() { f.service.config.Content = original })
			f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				if failure == "upload rejected" && request.Write {
					return nil, errors.New("upload rejected")
				}
				if failure == "verification failed" && !request.Write {
					return []byte("invalid archive"), nil
				}
				return objects.call(ctx, request)
			}
			ctx := t.Context()
			if failure != "missing storage" {
				ctx = f.service.contentContext(ctx)
			} else {
				f.service.config.Content = nil
			}
			if _, _, err := f.service.prepareCollaborationConversion(ctx, scope, row, []byte(raw)); err == nil {
				t.Fatal("scheduler conversion succeeded without a verified archive")
			}
			prepared := &claimPreparation{decisions: []preparedClaimDecision{{scope: scope, item: string(issue.WorkItemID), data: data}}}
			if err := prepareClaimDecisionPayloads(ctx, prepared); err == nil || prepared.decisions[0].payload.Key != "" {
				t.Fatalf("scheduler write preparation accepted an unverified archive: %v", err)
			}
			var retained string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT data_json FROM collaboration_events WHERE rowid=?", row).Scan(&retained); err != nil || retained != raw {
				t.Fatalf("failed conversion changed original payload: %v", err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
				PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: "scheduler-machine", SessionID: "scheduler-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
			})
			requireNativeStatus(t, response, http.StatusInternalServerError)
			var leases, events int
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM leases WHERE session_id='scheduler-session'").Scan(&leases); err != nil || leases != 0 {
				t.Fatalf("unverified scheduler payload committed %d leases: %v", leases, err)
			}
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'", issue.WorkItemID).Scan(&events); err != nil || events != 1 {
				t.Fatalf("unverified scheduler payload changed event count to %d: %v", events, err)
			}
		})
	}
}
