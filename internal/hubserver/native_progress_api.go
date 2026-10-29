package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressView struct {
	Request progresscheck.Request `json:"request"`
	Record  *progresscheck.Record `json:"record,omitempty"`
}

func progressRevision(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (int64, error) {
	var sequence int64
	err := q.QueryRowContext(ctx, "SELECT event_sequence + progress_evidence_revision FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", scope.organization, scope.project, item).Scan(&sequence)
	return sequence, err
}

func (s *Service) nativeProgressRequest(ctx context.Context, scope nativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	before, err := progressRevision(ctx, s.database.reader, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery, err := s.readNativeRecovery(ctx, scope, item, true)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id FROM (SELECT id,status,fencing_token FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT ?) WHERE status <> 'running' ORDER BY fencing_token DESC LIMIT ?`, scope.organization, scope.project, item, progresscheck.MaxAttempts+1, progresscheck.MaxAttempts)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
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
	rows, err = s.database.reader.QueryContext(ctx, `SELECT c.id FROM (SELECT id,deleted,progress_request_id,sequence FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=(SELECT CASE WHEN progress_comment_sequence>0 THEN progress_comment_sequence ELSE event_sequence END FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) ORDER BY sequence DESC LIMIT ?) c WHERE c.deleted=0 AND c.progress_request_id='' ORDER BY c.sequence DESC`, scope.organization, scope.project, item, scope.organization, scope.project, item, progresscheck.MaxComments)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	defer rows.Close()
	ids = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return progresscheck.Request{}, 0, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery.Discussion = nil
	slices.Reverse(ids)
	for _, id := range ids {
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, id)
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
		recovery.Discussion = append(recovery.Discussion, comment)
	}
	var humanComment string
	var revision int64
	if err := s.database.reader.QueryRowContext(ctx, `SELECT progress_evidence_revision,progress_human_comment_id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?`, scope.organization, scope.project, item).Scan(&revision, &humanComment); err != nil {
		return progresscheck.Request{}, 0, err
	}
	if humanComment != "" {
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, humanComment)
		if err != nil {
			return progresscheck.Request{}, 0, err
		}
		if !comment.Deleted {
			recovery.Discussion = slices.DeleteFunc(recovery.Discussion, func(c tracker.NativeComment) bool { return c.ID == comment.ID })
			recovery.Discussion = append(recovery.Discussion, comment)
		}
	}
	request := progresscheck.NativeRequest(recovery, policy, instance)
	request.EvidenceRevision = strconv.FormatInt(revision, 10)
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
	after, err := progressRevision(ctx, s.database.reader, scope, item)
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
	owner := nativeProgressOwner{service: s, scope: scope, request: request, leaseID: input.LeaseID, fencingToken: input.FencingToken, guardRevision: &sequence}
	record, err := owner.Publish(c.Request().Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record)
}
