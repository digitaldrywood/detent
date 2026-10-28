package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type nativeProgressView struct {
	Request progresscheck.Request `json:"request"`
	Record  *progresscheck.Record `json:"record,omitempty"`
}

func progressSequence(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (int64, error) {
	var sequence int64
	err := q.QueryRowContext(ctx, "SELECT event_sequence FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", scope.organization, scope.project, item).Scan(&sequence)
	return sequence, err
}

func (s *Service) nativeProgressRequest(ctx context.Context, scope nativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	before, err := progressSequence(ctx, s.database.reader, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery, err := s.readNativeRecovery(ctx, scope, item, true)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT ?`, scope.organization, scope.project, item, progresscheck.MaxAttempts)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return progresscheck.Request{}, 0, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery.Attempts = nil
	slices.Reverse(ids)
	for _, id := range ids {
		a, err := readNativeAttemptRecord(s.runDetailContext(ctx), s.database.reader, scope, item, id, s.config.now())
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
		recovery.Attempts = append(recovery.Attempts, a)
	}
	rows, err = s.database.reader.QueryContext(ctx, `SELECT c.id FROM native_comments c WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND c.deleted=0 AND c.progress_request_id=''
 ORDER BY c.sequence DESC LIMIT ?`, scope.organization, scope.project, item, progresscheck.MaxComments)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	ids = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return progresscheck.Request{}, 0, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery.Discussion = nil
	var successAt string
	var successTime time.Time
	err = s.database.reader.QueryRowContext(ctx, `SELECT updated_at FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND status='succeeded' ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&successAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return progresscheck.Request{}, 0, err
	}
	if err == nil {
		successTime, err = parseTimeValue(successAt)
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
	}
	slices.Reverse(ids)
	for _, id := range ids {
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, id)
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
		if comment.UpdatedAt.After(successTime) {
			recovery.Discussion = append(recovery.Discussion, comment)
		}
	}
	var humanEvent, humanData string
	err = s.database.reader.QueryRowContext(ctx, `SELECT id,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND actor_kind='human' AND type IN ('comment.created','comment.edited') ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&humanEvent, &humanData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return progresscheck.Request{}, 0, err
	}
	if err == nil {
		var data struct {
			CommentID string `json:"comment_id"`
		}
		if err := json.Unmarshal([]byte(humanData), &data); err != nil {
			return progresscheck.Request{}, 0, err
		}
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, data.CommentID)
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
		if comment.UpdatedAt.After(successTime) {
			recovery.Discussion = slices.DeleteFunc(recovery.Discussion, func(c tracker.NativeComment) bool { return c.ID == comment.ID })
			recovery.Discussion = append(recovery.Discussion, comment)
		}
	}
	request := progresscheck.NativeRequest(recovery, policy, instance)
	request.EvidenceRevision += ":" + humanEvent
	if err := s.database.reader.QueryRowContext(ctx, "SELECT version FROM machines WHERE organization_id=? AND id=?", scope.organization, instance).Scan(&request.InstanceVersion); err != nil {
		return progresscheck.Request{}, 0, err
	}
	defect, state, err := readNativeProgressDefect(ctx, s.database.reader, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	if state == "Done" {
		if len(request.Receipts) == progresscheck.MaxReceipts {
			request.Receipts = request.Receipts[1:]
		}
		request.Receipts = append(request.Receipts, progresscheck.Receipt{ID: defect + ":done", Step: "system_defect", Outcome: "fixed"})
	}
	after, err := progressSequence(ctx, s.database.reader, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	if before != after {
		return progresscheck.Request{}, 0, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
	return request, after, nil
}

func readNativeProgressDefect(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (string, string, error) {
	var defect, state string
	err := q.QueryRowContext(ctx, `SELECT d.defect_issue_id,ws.detent_state FROM native_progress_decisions d JOIN issues i ON i.native_id=d.defect_issue_id AND i.organization_id=d.organization_id AND i.project_id=d.project_id JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE d.organization_id=? AND d.project_id=? AND d.work_item_id=? AND d.outcome='system_defect' ORDER BY d.created_at DESC,d.request_id DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&defect, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return defect, state, err
}

func (s *Service) getNativeProgress(c echo.Context) error {
	scope := nativeRequestScope(c)
	request, _, err := s.nativeProgressRequest(c.Request().Context(), scope, c.Param("item"), c.QueryParam("policy"), c.QueryParam("instance"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	view := nativeProgressView{Request: request}
	if len(request.Attempts) > 0 && progresscheck.Failed(request.Attempts[len(request.Attempts)-1].Outcome) {
		id, err := request.ID()
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		record, found, err := readNativeProgress(c.Request().Context(), s.database.reader, scope, request.IssueID, id)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if found {
			view.Record = &record
		}
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) publishNativeProgress(c echo.Context) error {
	var input progresscheck.Publication
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	request, sequence, err := s.nativeProgressRequest(c.Request().Context(), scope, c.Param("item"), input.PolicyID, input.Instance)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	id, err := request.ID()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if id != input.RequestID {
		return s.nativeAPIError(c, &nativeError{Code: "revision_conflict", status: http.StatusConflict})
	}
	if len(request.Attempts) == 0 || !progresscheck.Failed(request.Attempts[len(request.Attempts)-1].Outcome) {
		return s.nativeAPIError(c, nativeInvalid("Progress decision requires a failed previous attempt"))
	}
	owner := nativeProgressOwner{service: s, scope: scope, request: request, leaseID: input.LeaseID, fencingToken: input.FencingToken, guardSequence: &sequence}
	record, err := owner.Publish(c.Request().Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record)
}
