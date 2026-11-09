package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func actionRunFromRecord(row postgres.ActionRun) actionRunRecord {
	r := actionRunRecord{ID: row.ID, ActionID: row.ActionID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkspaceID: row.WorkspaceID, Status: row.Status, Reason: row.Reason, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, OutputBytes: row.OutputContentBytes, Truncated: row.Truncated, CreatedBy: row.CreatedBy, ClaimedBy: row.ClaimedBy, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.ExitCode.Valid {
		value := int(row.ExitCode.Int32)
		r.ExitCode = &value
	}
	if row.ClaimedStreamID != "" {
		r.ClaimedBy = row.ClaimedStreamID
	} else if row.ClaimedBy != "" {
		r.ClaimedBy = "runner:" + row.ClaimedBy
	}
	r.OutputArtifact = actionRunOutputPath(r)
	return r
}

func workspaceArchive(key, digest string, size, content int64) attachment.ArchiveReference {
	return attachment.ArchiveReference{Key: key, SHA256: digest, CompressedSize: size, UncompressedSize: content, Codec: "gzip", Kind: "archive", Retention: attachment.ArchiveRetention, SchemaVersion: 1}
}
func (s *Service) hydrateActionRunRecord(ctx context.Context, row postgres.ActionRun, output bool) (actionRunRecord, error) {
	record := actionRunFromRecord(row)
	command, err := s.runDetailStore().ReadDetail(ctx, string(row.OrganizationID), workspaceArchive(row.CommandObjectKey, row.CommandSha256, row.CommandBytes, row.CommandContentBytes))
	if err != nil {
		return record, err
	}
	record.Command = string(command)
	if output && row.OutputObjectKey != nil {
		raw, err := s.runDetailStore().ReadDetail(ctx, string(row.OrganizationID), workspaceArchive(*row.OutputObjectKey, row.OutputSha256, row.OutputBytes, row.OutputContentBytes))
		if err != nil {
			return record, err
		}
		record.Output = string(raw)
	}
	return record, nil
}

func (s *Service) createActionRunRecord(ctx context.Context, scope nativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {
	if _, err := s.requireWorkspaces(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, http.MethodPost, "/actions/"+url.PathEscape(id)+"/runs")
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		if err != nil {
			return nil, err
		}
		return content.Receipt(ctx, receipt)
	}
	var workspace workspaceRecord
	var command string
	var actionRevision int64
	err = s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		action, err := r.ProjectAction(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if request.ExpectedActionRevision != 0 && int64(request.ExpectedActionRevision) != action.Action.Revision {
			return nativeConflict(tracker.Revision(action.Action.Revision))
		}
		actionRevision = action.Action.Revision
		workspace, err = workspaceForActor(ctx, r, scope, request.WorkspaceID)
		if err != nil {
			return err
		}
		return validateActionWorkspace(workspace)
	})
	if err != nil {
		return nil, err
	}
	records, err := s.actionRecords(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Action.ID == id {
			a, err := s.hydrateActionRecord(ctx, scope, record)
			if err != nil {
				return nil, err
			}
			command = a.Command
		}
	}
	if command == "" {
		return nil, nativeNotFound()
	}
	now := s.config.now().UTC()
	run := actionRunRecord{ID: newNativeID("actionrun"), OrganizationID: scope.organization, ProjectID: scope.project, ActionID: id, WorkspaceID: workspace.ID, Command: command, Status: "queued", CreatedBy: scope.actor().PrincipalID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(projectActionRunReceipt{RunID: run.ID})
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.workspaceWrite(ctx, scope, func(w workspaceWriter, _ time.Time) error {
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		previous, e := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if e == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &previous
			return nil
		}
		if !errors.Is(e, corestore.ErrNotFound) {
			return e
		}
		current, e := workspaceForActor(ctx, w, scope, workspace.ID)
		if e != nil {
			return e
		}
		if e = validateActionWorkspace(current); e != nil {
			return e
		}
		action, e := w.ProjectAction(ctx, scope.project, id)
		if e != nil {
			return e
		}
		if action.Action.Revision != actionRevision {
			return nativeConflict(tracker.Revision(action.Action.Revision))
		}
		created, e := w.CreateActionRun(ctx, postgres.CreateActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: run.ID, ActionID: id, WorkspaceID: workspace.ID, CreatedBy: run.CreatedBy, CreatedAt: now})
		if e != nil {
			return e
		}
		if e = keepWorkspaceArchive(ctx, w, scope, workspaceArchive(created.CommandObjectKey, created.CommandSha256, created.CommandBytes, created.CommandContentBytes), "project-action-run", created.ID, now); e != nil {
			return e
		}
		_, e = w.PutReceipt(ctx, receipt)
		return e
	})
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}

func validateActionWorkspace(workspace workspaceRecord) error {
	if !workspacesession.Bound(workspace.State) {
		return nativeStaleExecution("The workspace is not bound to a runner")
	}
	if workspace.ReadOnly {
		return actionRunReadOnly()
	}
	if workspace.Capabilities == nil || !workspace.Capabilities.Has(workspacesession.CapabilityExec) {
		return actionRunCapabilityMissing()
	}
	return nil
}

func actionRunForActor(ctx context.Context, r workspaceReader, scope nativeScope, id string) (postgres.ActionRun, error) {
	row, err := r.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: id})
	if err != nil {
		return row, err
	}
	if _, err = r.ProjectAction(ctx, scope.project, row.ActionID); err != nil {
		return row, err
	}
	if _, err = workspaceForActor(ctx, r, scope, row.WorkspaceID); err != nil {
		return row, err
	}
	return row, nil
}

func (s *Service) readActionRunRecordByID(ctx context.Context, scope nativeScope, id string) (postgres.ActionRun, error) {
	var row postgres.ActionRun
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		var err error
		row, err = actionRunForActor(ctx, r, scope, id)
		return err
	})
	return row, err
}

func (s *Service) readActionRunRecords(ctx context.Context, scope nativeScope, action, id string, limit int) ([]postgres.ActionRun, error) {
	var records []postgres.ActionRun
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		if id != "" {
			row, err := actionRunForActor(ctx, r, scope, id)
			if err != nil {
				return err
			}
			if row.ActionID != action {
				return nativeNotFound()
			}
			records = []postgres.ActionRun{row}
			return nil
		}
		if _, err := r.ProjectAction(ctx, scope.project, action); err != nil {
			return err
		}
		rows, err := r.RecentActionRuns(ctx, postgres.ActionRunsInput{OrganizationID: scope.organization, ProjectID: scope.project, ActionID: action, Limit: int32(min(max(limit, 1), workspaceListLimit))})
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.WorkspaceID)
		}
		sessions, err := r.WorkspaceSessionsByIDs(ctx, ids)
		if err != nil {
			return err
		}
		visible, err := workspaceVisibleSubjects(ctx, r, scope, sessions)
		if err != nil {
			return err
		}
		allowed := make(map[string]bool, len(sessions))
		for _, session := range sessions {
			allowed[session.ID] = session.ProjectID == scope.project && (visible == nil || visible[session.SubjectWorkItemID])
		}
		for _, row := range rows {
			if allowed[row.WorkspaceID] {
				records = append(records, row)
			}
		}
		return nil
	})
	return records, err
}

func (s *Service) listActionRunRecords(ctx context.Context, scope nativeScope, action string, limit int) (json.RawMessage, error) {
	rows, err := s.readActionRunRecords(ctx, scope, action, "", limit)
	if err != nil {
		return nil, err
	}
	page := projectActionRunList{Items: []workspacesession.Run{}}
	for _, row := range rows {
		record, err := s.hydrateActionRunRecord(ctx, row, false)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, record.resource())
	}
	return json.Marshal(page)
}

func (s *Service) getActionRunRecord(ctx context.Context, scope nativeScope, action, id string) (actionRunRecord, error) {
	rows, err := s.readActionRunRecords(ctx, scope, action, id, 1)
	if err != nil {
		return actionRunRecord{}, err
	}
	return s.hydrateActionRunRecord(ctx, rows[0], true)
}

func (w *workspaceService) finishActionRunRecord(ctx context.Context, state *execRunState, status string, exit *int, reason string) error {
	var row postgres.ActionRun
	err := w.server.config.TenantRepository.ReadTenant(ctx, state.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner := r.(workspaceReader)
		var err error
		row, err = owner.ActionRunByID(ctx, state.runID)
		return err
	})
	if err != nil {
		return err
	}
	if workspacesession.TerminalRunStatus(row.Status) {
		return nil
	}
	output := state.output
	truncated := state.truncated
	var ref attachment.ArchiveReference
	if output == nil && row.OutputObjectKey != nil {
		ref = workspaceArchive(*row.OutputObjectKey, row.OutputSha256, row.OutputBytes, row.OutputContentBytes)
		truncated = truncated || row.Truncated
	} else if len(output) > 0 {
		if len(output) > workspacesession.MaxExecOutputBytes {
			output = output[:workspacesession.MaxExecOutputBytes]
			truncated = true
		}
		var err error
		ref, err = w.server.runDetailStore().StoreDetail(ctx, string(row.OrganizationID), output)
		if err != nil {
			return err
		}
	}
	var outputKey *string
	if ref.Key != "" {
		outputKey = &ref.Key
	}

	var code pgtype.Int4
	if exit != nil {
		code = pgtype.Int4{Int32: int32(*exit), Valid: true}
	}
	return w.server.workspaceWrite(ctx, nativeScope{organization: row.OrganizationID, project: row.ProjectID}, func(writer workspaceWriter, now time.Time) error {
		if err := keepActionOutputArchive(ctx, writer, row, ref, now); err != nil {
			return err
		}
		_, err := writer.FinishActionRun(ctx, postgres.FinishActionRunInput{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, Status: status, ExitCode: code, Reason: reason, FinishedAt: &now, OutputObjectKey: outputKey, OutputSha256: ref.SHA256, OutputBytes: ref.CompressedSize, OutputContentBytes: ref.UncompressedSize, Truncated: truncated})
		return err
	})
}

func keepWorkspaceArchive(ctx context.Context, w workspaceWriter, scope nativeScope, ref attachment.ArchiveReference, kind, id string, now time.Time) error {
	object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: newNativeID("att"), Reference: ref, CreatedAt: now})
	if err != nil {
		return err
	}
	return w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.organization, ArchiveID: object.ID, Kind: kind, ID: actionstore.ArchiveOwnerID(scope.project, id), CreatedAt: now})
}

func keepActionOutputArchive(ctx context.Context, w workspaceWriter, row postgres.ActionRun, ref attachment.ArchiveReference, now time.Time) error {
	scope := nativeScope{organization: row.OrganizationID, project: row.ProjectID}
	if ref.Key != "" {
		if err := keepWorkspaceArchive(ctx, w, scope, ref, "project-action-output", row.ID, now); err != nil {
			return err
		}
	}
	if row.OutputObjectKey == nil || *row.OutputObjectKey == ref.Key {
		return nil
	}
	old, err := w.ArchiveObjectByReference(ctx, workspaceArchive(*row.OutputObjectKey, row.OutputSha256, row.OutputBytes, row.OutputContentBytes))
	if err != nil {
		return err
	}
	return w.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: row.OrganizationID, ArchiveID: old.ID, Kind: "project-action-output", ID: actionstore.ArchiveOwnerID(row.ProjectID, row.ID), CreatedAt: now})
}
