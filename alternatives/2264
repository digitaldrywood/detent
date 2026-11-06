package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeProgressAttemptsSQL = `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND status <> 'running' ORDER BY fencing_token DESC LIMIT ?`

type nativeProgressView struct {
	Request progresscheck.Request `json:"request"`
	Record  *progresscheck.Record `json:"record,omitempty"`
}

func progressRevision(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (int64, error) {
	var sequence int64
	err := q.QueryRowContext(ctx, "SELECT progress_evidence_revision FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", scope.organization, scope.project, item).Scan(&sequence)
	return sequence, err
}

func (s *Service) nativeProgressRequest(ctx context.Context, scope nativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	return s.nativeProgressRequestTenant(ctx, scope, item, policy, instance)
}

func (s *Service) nativeProgressRequestTenant(ctx context.Context, scope nativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	ctx = s.runDetailContext(s.contentContext(ctx))
	before, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery, err := s.readNativeRecoveryCommentsTenant(ctx, scope, item, true)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	request := progresscheck.NativeRequest(recovery, policy, instance)
	request.EvidenceRevision = strconv.FormatInt(int64(recovery.Issue.Revision), 10)
	defectReceipt, hasDefectReceipt, err := s.nativeProgressDefectReceiptTenant(ctx, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	if hasDefectReceipt {
		if len(request.Receipts) == progresscheck.MaxReceipts {
			request.Receipts = request.Receipts[1:]
		}
		request.Receipts = append(request.Receipts, defectReceipt)
	}
	if instance != "" {
		var version string
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			machine, err := reader.Machine(ctx, instance)
			if err != nil {
				return err
			}
			version = machine.Version
			return nil
		})
		if err != nil {
			return progresscheck.Request{}, 0, projectRecordError(err)
		}
		request.InstanceVersion = version
	}
	after, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	if before.Revision != after.Revision || before.EventSequence != after.EventSequence || int64(recovery.Issue.Revision) != before.Revision {
		return progresscheck.Request{}, 0, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
	return request, after.EventSequence, nil
}

func (s *Service) nativeProgressDefectReceiptTenant(ctx context.Context, scope nativeScope, item string) (progresscheck.Receipt, bool, error) {
	var defectID tracker.NativeWorkItemID
	var defectState string
	var defectCreated time.Time
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		cursor := corestore.RelationshipCursor{}
		for range claimPreparationLimit {
			page, err := reader.Relationships(ctx, scope.project, tracker.NativeWorkItemID(item), cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, relationship := range page.Items {
				if relationship.Type != "dependency" || relationship.Direction != "blocked_by" {
					continue
				}
				blocker, err := reader.WorkItemByNativeID(ctx, relationship.RelatedWorkItemID)
				if err != nil {
					return err
				}
				if blocker.NativeSourceKey == nil || !strings.HasPrefix(*blocker.NativeSourceKey, "progress-check:") {
					continue
				}
				state := ""
				if blocker.WorkflowStateName != nil {
					state = *blocker.WorkflowStateName
				}
				if relationship.CreatedAt.After(defectCreated) || relationship.CreatedAt.Equal(defectCreated) && relationship.RelatedWorkItemID > defectID {
					defectID, defectState, defectCreated = relationship.RelatedWorkItemID, state, relationship.CreatedAt
				}
			}
			if page.Next == (corestore.RelationshipCursor{}) {
				return nil
			}
			cursor = page.Next
		}
		return corestore.ErrInvalidPage
	})
	if err != nil {
		return progresscheck.Receipt{}, false, projectRecordError(err)
	}
	if defectID == "" || defectState != "Done" {
		return progresscheck.Receipt{}, false, nil
	}
	return progresscheck.Receipt{ID: string(defectID) + ":done", Step: "system_defect", Outcome: "fixed"}, true, nil
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
	if request.NeedsCheck() {
		id, err := request.ID()
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		record, found, err := (nativeProgressOwner{service: s, scope: scope, request: request}).Load(c.Request().Context(), request.ProjectID, request.IssueID, id)
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
	if !request.NeedsCheck() {
		return s.nativeAPIError(c, nativeInvalid("Progress decision requires a non-progressing previous attempt"))
	}
	owner := nativeProgressOwner{service: s, scope: scope, request: request, leaseID: input.LeaseID, fencingToken: input.FencingToken, guardRevision: &sequence, expectedRevision: input.ExpectedRevision}
	record, err := owner.Publish(c.Request().Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record)
}
