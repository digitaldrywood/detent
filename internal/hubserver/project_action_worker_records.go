package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) reportWorkspaceActionRunRecord(c echo.Context) error {
	var request workspaceActionRunReport
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	if _, err := s.requireWorkspaces(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Status != "running" && request.Status != "succeeded" && request.Status != "failed" || request.Reason != "" && !validActionRunReason(request.Reason) || request.OutputSequence < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid action run report"))
	}
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	output := []byte(request.Output)
	truncated := request.Truncated
	if len(output) > workspacesession.MaxExecOutputBytes {
		output = output[:workspacesession.MaxExecOutputBytes]
		truncated = true
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := authorizeRunnerRepositoryReader(ctx, r, scope, s.config.now())
		return err
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	var ref attachment.ArchiveReference
	var err error
	if len(output) > 0 {
		ref, err = s.runDetailStore().StoreDetail(ctx, string(scope.organization), output)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	runID := strings.TrimSpace(request.RunID)
	if runID == "" {
		runID = newNativeID("actionrun")
		if request.IdempotencyKey != "" {
			digest := sha256.Sum256([]byte(c.Param("workspace") + ":" + request.ActionID + ":" + string(request.WorkspaceIdentity.LeaseID) + ":" + strconv.FormatInt(int64(request.WorkspaceIdentity.FencingToken), 10) + ":" + request.IdempotencyKey))
			runID = "actionrun_" + hex.EncodeToString(digest[:16])
		}
	}
	var reported postgres.ActionRun
	err = s.workspaceWorkerWrite(ctx, scope, func(w workspaceWriter, now time.Time) error {
		workspace, _, err := loadWorkspaceWorkerRecord(ctx, w, scope, c.Param("workspace"), request.WorkspaceIdentity, now)
		if err != nil {
			return err
		}
		if workspace.LeaseID != request.WorkspaceIdentity.LeaseID || workspace.FencingToken != request.WorkspaceIdentity.FencingToken || workspace.RunnerID != scope.credential.Runner.RunnerID {
			return nativeStaleExecution("The workspace owner changed")
		}
		if err = validateActionWorkspace(workspace); err != nil {
			return err
		}
		action, err := w.ProjectAction(ctx, scope.project, request.ActionID)
		if err != nil {
			return err
		}
		row, err := w.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: runID})
		if errors.Is(err, corestore.ErrNotFound) {
			if request.RunID != "" {
				return nativeNotFound()
			}
			if !action.Action.RunOnWorktreeCreation {
				return nativePublicInvalid("Only a run-on-worktree-creation action may be reported without a run id")
			}
			sequence := request.OutputSequence
			if sequence == 0 {
				sequence = 1
			}
			started := request.StartedAt
			if started == nil {
				started = &now
			}
			finished := request.FinishedAt
			if request.Status != "running" && finished == nil {
				finished = &now
			}
			var exit pgtype.Int4
			if request.ExitCode != nil {
				exit = pgtype.Int4{Int32: int32(*request.ExitCode), Valid: true}
			}
			reported, err = w.CreateReportedActionRun(ctx, postgres.CreateReportedActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: runID, ActionID: request.ActionID, WorkspaceID: workspace.ID, Status: request.Status, ExitCode: exit, Reason: request.Reason, StartedAt: *started, FinishedAt: finished, OutputObjectKey: ref.Key, OutputSha256: ref.SHA256, OutputBytes: ref.CompressedSize, OutputContentBytes: ref.UncompressedSize, OutputSequence: sequence, Truncated: truncated, RunnerID: workspace.RunnerID, LeaseID: string(workspace.LeaseID), FencingToken: int64(workspace.FencingToken), CreatedAt: now})
			if err != nil {
				return err
			}
			if err = keepWorkspaceArchive(ctx, w, scope, workspaceArchive(reported.CommandObjectKey, reported.CommandSha256, reported.CommandBytes, reported.CommandContentBytes), "project-action-run", reported.ID, now); err != nil {
				return err
			}
			return keepActionOutputArchive(ctx, w, reported, ref, now)

		}
		if err != nil {
			return err
		}
		if request.Output == "" && row.OutputObjectKey != nil {
			ref = workspaceArchive(*row.OutputObjectKey, row.OutputSha256, row.OutputBytes, row.OutputContentBytes)
			truncated = truncated || row.Truncated
		}
		if row.ActionID != request.ActionID || row.WorkspaceID != workspace.ID {
			return nativeNotFound()
		}
		if workspacesession.TerminalRunStatus(row.Status) {
			if row.Status == request.Status && row.OutputSha256 == ref.SHA256 && row.Reason == request.Reason && row.Truncated == truncated && (!row.ExitCode.Valid && request.ExitCode == nil || row.ExitCode.Valid && request.ExitCode != nil && int(row.ExitCode.Int32) == *request.ExitCode) {
				reported = row
				return nil
			}
			return nativeStaleExecution("The action run already ended")
		}
		sequence := request.OutputSequence
		if sequence == 0 {
			sequence = row.OutputSequence + 1
		}
		if sequence <= row.OutputSequence {
			return nativeStaleExecution("The action run output sequence is stale")
		}
		var exit pgtype.Int4
		if request.ExitCode != nil {
			exit = pgtype.Int4{Int32: int32(*request.ExitCode), Valid: true}
		}
		finished := request.FinishedAt
		if request.Status != "running" && finished == nil {
			finished = &now
		}
		if err := keepActionOutputArchive(ctx, w, row, ref, now); err != nil {
			return err
		}
		var outputKey *string
		if ref.Key != "" {
			outputKey = &ref.Key
		}
		reported, err = w.ReportActionRun(ctx, postgres.ReportActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: row.ID, RunnerID: workspace.RunnerID, LeaseID: string(workspace.LeaseID), FencingToken: int64(workspace.FencingToken), Status: request.Status, ExitCode: exit, Reason: request.Reason, FinishedAt: finished, OutputObjectKey: outputKey, OutputSha256: ref.SHA256, OutputBytes: ref.CompressedSize, OutputContentBytes: ref.UncompressedSize, OutputSequence: sequence, Truncated: truncated, UpdatedAt: now})
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record, err := s.hydrateActionRunRecord(ctx, reported, false)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record.resource())
}

func (w *workspaceService) claimActionRunRecord(ctx context.Context, scope nativeScope, id, stream string, runner *relayConnection) (actionRunRecord, error) {
	var row postgres.ActionRun
	err := w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, now time.Time) error {
		current, err := writer.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: id})
		if err != nil {
			return err
		}
		workspace, err := writer.WorkspaceSession(ctx, scope.project, current.WorkspaceID)
		if err != nil {
			return err
		}
		if err = validateActionWorkspace(workspaceFromRecord(workspace)); err != nil {
			return err
		}
		if runner == nil || runner.owner != workspaceFromRecord(workspace).owner() {
			return nativeStaleExecution("The runner connection belongs to an earlier workspace owner")
		}
		if _, _, err = loadWorkspaceWorkerRecord(ctx, writer, runner.scope, workspace.ID, workspaceWorkerIdentity{LeaseID: tracker.LeaseID(workspace.LeaseID), FencingToken: tracker.FencingToken(workspace.FencingToken)}, now); err != nil {
			return err
		}
		row, err = writer.ClaimActionRun(ctx, postgres.ClaimActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: id, RunnerID: workspace.RunnerID, LeaseID: workspace.LeaseID, FencingToken: workspace.FencingToken, StartedAt: &now, ClaimedStreamID: stream})
		return err
	})
	if err != nil {
		return actionRunRecord{}, err
	}
	return w.server.hydrateActionRunRecord(ctx, row, false)
}

func (w *workspaceService) dispatchActionRunRecords(ctx context.Context) {
	var runs []postgres.ActionRun
	err := w.server.config.TenantRepository.ReadTenant(ctx, w.organization(), corestore.Background, func(r tenantstore.Reader) error {
		var err error
		runs, err = r.(workspaceReader).PendingActionRuns(ctx, w.now().Add(-actionRunDispatchGrace), actionRunDispatchBatch)
		return err
	})
	if err != nil {
		w.logger.Warn("action_run.dispatch_failed", "error", err)
		return
	}
	for _, row := range runs {
		w.dispatchActionRun(ctx, actionRunFromRecord(row))
	}
}

func (w *workspaceService) sweepActionRunRecords(ctx context.Context) {
	w.purgeDeletedActionRecords(ctx)
	var ended []postgres.ActionRun
	err := w.server.config.TenantRepository.ReadTenant(ctx, w.organization(), corestore.Background, func(reader tenantstore.Reader) error {
		owner := reader.(workspaceReader)
		rows, err := owner.ActiveActionRunsAfter(ctx, w.afterActionRun, workspaceMaintenanceBatch)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.WorkspaceID)
		}
		sessions, err := owner.WorkspaceSessionsByIDs(ctx, ids)
		if err != nil {
			return err
		}
		states := make(map[string]string, len(sessions))
		for _, session := range sessions {
			states[session.ID] = session.State
		}
		for _, row := range rows {
			if workspacesession.Terminal(states[row.WorkspaceID]) {
				ended = append(ended, row)
			}
		}
		if len(rows) < workspaceMaintenanceBatch {
			w.afterActionRun = ""
		} else {
			w.afterActionRun = rows[len(rows)-1].ID
		}
		return nil
	})
	if err != nil {
		w.logger.Warn("action_run.sweep_failed", "error", err)
		return
	}
	for _, row := range ended {
		if err = w.finishActionRunRecord(ctx, &execRunState{runID: row.ID}, workspacesession.RunFailed, nil, workspacesession.RunReasonWorkspaceEnd); err != nil {
			w.logger.Warn("action_run.sweep_transition_failed", "run_id", row.ID, "error", err)
		}
	}
}

func (w *workspaceService) purgeDeletedActionRecords(ctx context.Context) {
	var actions []postgres.DeletedProjectAction
	err := w.server.config.TenantRepository.ReadTenant(ctx, w.organization(), corestore.Background, func(r tenantstore.Reader) error {
		var err error
		actions, err = r.(workspaceReader).DeletedProjectActions(ctx, workspaceMaintenanceBatch)
		return err
	})
	if err != nil {
		w.logger.Warn("action_run.cleanup_failed", "error", err)
		return
	}
	for _, action := range actions {
		scope := nativeScope{organization: w.organization(), project: action.ProjectID}
		err = w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, now time.Time) error {
			row, err := writer.PurgeDeletedActionRun(ctx, action.ProjectID, action.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				if err := keepActionOutputArchive(ctx, writer, row, attachment.ArchiveReference{}, now); err != nil {
					return err
				}
				object, err := writer.ArchiveObjectByReference(ctx, workspaceArchive(row.CommandObjectKey, row.CommandSha256, row.CommandBytes, row.CommandContentBytes))
				if err != nil {
					return err
				}
				if err := writer.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.organization, ArchiveID: object.ID, Kind: "project-action-run", ID: actionstore.ArchiveOwnerID(scope.project, row.ID), CreatedAt: now}); err != nil {
					return err
				}
			}
			return writer.PurgeEmptyDeletedAction(ctx, action.ProjectID, action.ID)
		})
		if err != nil {
			w.logger.Warn("action_run.cleanup_failed", "action_id", action.ID, "error", err)
		}
	}
}
