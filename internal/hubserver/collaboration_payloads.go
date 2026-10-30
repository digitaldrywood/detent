package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func collaborationPayloadColumn(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "CASE WHEN " + alias + "payload_version=1 THEN 'event:' || " + alias + "id ELSE " + alias + "data_json END"
}

func schedulerPayloadArchive(scope nativeScope, raw string) ([]byte, error) {
	return json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "scheduler.decision", Data: json.RawMessage(raw)})
}

func collaborationPayloadValues(ctx context.Context, tx *sql.Tx, scope nativeScope, item, kind, id string, data tracker.CollaborationData, raw string, now time.Time) (string, []any, error) {
	version := 0
	attachmentID, entry := "", ""
	ref := contentReference()
	if kind == "scheduler.decision" {
		if prepared := claimPrepared(ctx); prepared != nil {
			for _, value := range prepared.decisions {
				if value.scope.organization == scope.organization && value.scope.project == scope.project && value.item == item && reflect.DeepEqual(value.data, data) {
					ref = value.payload
					break
				}
			}
		}
		if ref.Validate(string(scope.organization)) != nil {
			if claimPrepared(ctx) != nil {
				return "", nil, errors.New("scheduler payload archive was not prepared")
			}
			payload, err := schedulerPayloadArchive(scope, raw)
			if err != nil {
				return "", nil, err
			}
			key, err := storedPayloadKey(ctx, string(scope.organization), payload)
			if err != nil {
				return "", nil, err
			}
			ref, err = readPayloadReference(ctx, tx, string(scope.organization), key)
			if err != nil {
				return "", nil, err
			}
		}
		version, raw = 1, "{}"
	}
	if strings.HasPrefix(kind, "run.") {
		payload, err := json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: kind + ":" + id, Data: json.RawMessage(raw)})
		if err != nil {
			return "", nil, err
		}
		key, err := storedPayloadKey(ctx, string(scope.organization), payload)
		if err != nil {
			return "", nil, err
		}
		ref, err = readPayloadReference(ctx, tx, string(scope.organization), key)
		if err != nil {
			return "", nil, err
		}
		if scope.credential.ID != "" {
			if _, err := recordPayloadAttachment(ctx, tx, scope, tracker.NativeWorkItemID(item), ref, formatHubTime(now)); err != nil {
				return "", nil, err
			}
		}
		version, raw = 1, "{}"
	}
	change := tracker.NativeChangeReference{}
	if data.Change != nil {
		change = *data.Change
	}
	run := tracker.NativeRunData{}
	if data.Run != nil {
		run = *data.Run
	}
	checkpoint := tracker.NativeCheckpoint{}
	if run.Handoff != nil {
		checkpoint = *run.Handoff
	}
	return raw, []any{version, attachmentID, entry, change.ChangeID, change.VersionID, change.HeadSHA, data.Change != nil, run.Outcome, run.MachineID, run.SessionID, run.RunnerID, checkpoint.EffectID, checkpoint.HeadSHA, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize}, nil
}

func resolveCollaborationPayload(ctx context.Context, q nativeQueryer, raw string) (string, error) {
	id, referenced := strings.CutPrefix(raw, "event:")
	if !referenced {
		resolved, err := resolveStorageJSON(ctx, []byte(raw))
		return string(resolved), err
	}
	var organization, project, kind, attachmentID, entry, outcome, machine, session, runner string
	var scheduler tracker.NativeSchedulerDecision
	var change tracker.NativeChangeReference
	var schedulerPresent, changePresent bool
	var schedulerAt string
	var revision, sequence int64
	var row int64
	var expected tracker.NativeRunData
	ref := contentReference()
	err := q.QueryRowContext(ctx, `SELECT organization_id,project_id,type,payload_attachment_id,payload_entry,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,
coalesce(event_revision,0),coalesce(run_sequence,0),run_outcome,run_machine_id,run_session_id,run_runner_id,
coalesce(scheduler_present,0),coalesce(scheduler_outcome,''),coalesce(scheduler_reason,''),coalesce(scheduler_source,''),coalesce(scheduler_runner_id,''),coalesce(scheduler_at,''),coalesce(scheduler_revision,0),scheduler_change_id,scheduler_version_id,scheduler_head_sha,scheduler_change_present,
rowid,coalesce(claim_attempt_id,''),coalesce(run_id,''),coalesce(run_lease_id,''),coalesce(run_fencing_token,0),coalesce(run_policy_id,'')
FROM collaboration_events WHERE id=? AND payload_version=1`, id).Scan(&organization, &project, &kind, &attachmentID, &entry, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize,
		&revision, &sequence, &outcome, &machine, &session, &runner,
		&schedulerPresent, &scheduler.Outcome, &scheduler.Reason, &scheduler.Source, &scheduler.RunnerID, &schedulerAt, &scheduler.WorkItemRevision, &change.ChangeID, &change.VersionID, &change.HeadSHA, &changePresent,
		&row, &expected.AttemptID, &expected.RunID, &expected.LeaseID, &expected.FencingToken, &expected.PolicyID)
	if err != nil {
		return "", err
	}
	if ref.Key != "" && !explanationRead(ctx) && (kind != "scheduler.decision" || claimPrepared(ctx) == nil) {
		payload, err := readChangeContent(ctx, organization, ref)
		if err != nil {
			return "", err
		}
		var archived storageJSONPayload
		if err := json.Unmarshal(payload, &archived); err != nil {
			return "", err
		}
		bound := archived.Family == "runs" && archived.Row == row || kind == "scheduler.decision" && archived.Family == kind && archived.Row == 0 || strings.HasPrefix(kind, "run.") && archived.Family == kind+":"+id && archived.Row == 0
		if archived.Organization != organization || archived.Project != project || !bound || !json.Valid(archived.Data) {
			return "", artifact.ErrIntegrity
		}
		if strings.HasPrefix(kind, "run.") && archived.Row == 0 {
			var data tracker.CollaborationData
			if err := json.Unmarshal(archived.Data, &data); err != nil {
				return "", err
			}
			if data.Run == nil || data.Run.AttemptID != expected.AttemptID || data.Run.RunID != expected.RunID || data.Run.LeaseID != expected.LeaseID || data.Run.FencingToken != expected.FencingToken || data.Run.PolicyID != expected.PolicyID {
				return "", artifact.ErrIntegrity
			}
		}
		return string(archived.Data), nil
	}
	data := tracker.CollaborationData{Revision: tracker.Revision(revision)}
	if kind == "scheduler.decision" {
		if ref.Key == "" {
			return "", artifact.ErrIntegrity
		}
		if schedulerPresent {
			if schedulerAt != "" {
				scheduler.At, err = parseTimeValue(schedulerAt)
				if err != nil {
					return "", err
				}
			}
			data.Decision = &scheduler
		}
		if changePresent {
			data.Change = &change
		}
	} else if ref.Key == "" && attachmentID != "" && entry != "" {
		if !explanationRead(ctx) {
			runs, err := readRunDetailSegment(ctx, q, &runDetailReference{AttachmentID: attachmentID, Organization: organization, Entry: entry})
			if err != nil {
				return "", err
			}
			run, found := runs[entry]
			if !found || runDetailEntry(run) != entry || run.AttemptID != expected.AttemptID || run.RunID != expected.RunID || run.LeaseID != expected.LeaseID || run.FencingToken != expected.FencingToken || run.PolicyID != expected.PolicyID {
				return "", artifact.ErrIntegrity
			}
			run.Sequence, run.Outcome = sequence, outcome
			run.MachineID, run.SessionID, run.RunnerID = tracker.MachineID(machine), session, runner
			data.Run = &run
		}
	} else if ref.Key == "" {
		return "", errors.Join(artifact.ErrIntegrity, sql.ErrNoRows)
	}
	return marshalNative(data)
}

func (s *Service) prepareCollaborationConversion(ctx context.Context, scope nativeScope, row int64, raw []byte) ([]byte, attachment.ArchiveReference, error) {
	payload, err := resolveStorageJSON(ctx, raw)
	if err != nil {
		return nil, attachment.ArchiveReference{}, err
	}
	encoded, err := json.Marshal(storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "runs", Row: row, Data: payload})
	if err != nil {
		return nil, attachment.ArchiveReference{}, err
	}
	if err := expandStorageConversion(ctx, len(encoded)); err != nil {
		return nil, attachment.ArchiveReference{}, err
	}
	ref, err := prepareChangeContent(ctx, string(scope.organization), encoded)
	if err != nil {
		return nil, ref, err
	}
	verified, err := readChangeContent(ctx, string(scope.organization), ref)
	if err != nil {
		return nil, ref, err
	}
	if !bytes.Equal(encoded, verified) {
		return nil, ref, artifact.ErrIntegrity
	}
	return []byte("{}"), ref, nil
}

func (s *Service) prepareCollaborationPayloadMetadata(ctx context.Context, row int64, raw []byte) (conversationQueryMetadata, error) {
	var data tracker.CollaborationData
	var m conversationQueryMetadata
	if err := decodeRunHistory(ctx, s.database.reader, string(raw), &data); err != nil {
		return m, err
	}
	var id, actorRaw, at, kind string
	if err := s.database.reader.QueryRowContext(ctx, "SELECT id,actor_json,recorded_at,type FROM collaboration_events WHERE rowid=?", row).Scan(&id, &actorRaw, &at, &kind); err != nil {
		return m, err
	}
	var actor tracker.Actor
	if err := json.Unmarshal([]byte(actorRaw), &actor); err != nil {
		return m, err
	}
	recorded, err := parseTimeValue(at)
	if err != nil {
		return m, err
	}
	encoded, err := resolveCollaborationPayload(ctx, s.database.reader, string(raw))
	if err != nil {
		return m, err
	}
	archive, err := archiveEventValues(encoded)
	if err != nil {
		return m, err
	}
	columns := changeEventColumns(data)
	for i, column := range columns.columns {
		m.add(column, columns.values[i])
	}
	for i, column := range []string{"archive_operation", "archive_from_state", "archive_to_state", "archive_attempt_id", "archive_outcome", "archive_landed_commit"} {
		m.add(column, archive[i])
	}
	decision := tracker.NativeSchedulerDecision{}
	if data.Decision != nil {
		decision = *data.Decision
	}
	change := tracker.NativeChangeReference{}
	if data.Change != nil {
		change = *data.Change
	}
	run := tracker.NativeRunData{}
	if data.Run != nil {
		run = *data.Run
	}
	for i, column := range []string{"scheduler_change_id", "scheduler_version_id", "scheduler_head_sha", "scheduler_change_present", "run_outcome", "run_machine_id", "run_session_id", "run_runner_id", "run_attempt_id", "activity_spans_present", "analytics_from_state", "analytics_to_state", "analytics_attempt_id", "analytics_skip_key", "analytics_recorded_ns", "analytics_projection_version"} {
		values := []any{change.ChangeID, change.VersionID, change.HeadSHA, data.Change != nil, run.Outcome, run.MachineID, run.SessionID, run.RunnerID, nullString(run.AttemptID), false, nullString(data.FromState), nullString(data.ToState), run.AttemptID, analyticsDimension(decision.Source, decision.Reason), recorded.UnixNano(), 1}
		m.add(column, values[i])
	}
	checkpoint := tracker.NativeCheckpoint{}
	if run.Handoff != nil {
		checkpoint = *run.Handoff
	}
	m.add("push_effect_id", checkpoint.EffectID)
	m.add("push_checkpoint_head_sha", checkpoint.HeadSHA)
	projected := data
	if kind != "workflow.transitioned" {
		projected.ReasonDetail = ""
	}
	m.project = func(ctx context.Context, tx *sql.Tx) error {
		if err := updateHealthEventProjection(ctx, tx, id, actor, projected); err != nil {
			return err
		}
		return updateEventPageFacts(ctx, tx, id, actor, data)
	}
	return m, nil
}
