package hubserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeProgressAttemptsSQL = `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND status <> 'running' ORDER BY fencing_token DESC LIMIT ?`

type nativeProgressView struct {
	Request progresscheck.Request `json:"request"`
	Record  *progresscheck.Record `json:"record,omitempty"`
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
	var evidenceSequence int64
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		owner, ok := reader.(eventstore.ProgressEvidenceReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		evidenceSequence, err = owner.ProgressEvidenceSequence(ctx, scope.project, before.NativeID)
		return err
	}); err != nil {
		return progresscheck.Request{}, 0, err
	}
	recovery, err := s.readNativeRecoveryCommentsTenant(ctx, scope, item, true)
	if err != nil {
		return progresscheck.Request{}, 0, err
	}
	request := progresscheck.NativeRequest(recovery, policy, instance)
	request.EvidenceRevision = strconv.FormatInt(evidenceSequence, 10)
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
	if before.Revision != after.Revision || int64(recovery.Issue.Revision) != before.Revision {
		return progresscheck.Request{}, 0, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
	if before.EventSequence != after.EventSequence {
		var currentSequence int64
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			currentSequence, err = reader.(eventstore.ProgressEvidenceReader).ProgressEvidenceSequence(ctx, scope.project, before.NativeID)
			return err
		}); err != nil {
			return progresscheck.Request{}, 0, err
		}
		if currentSequence != evidenceSequence {
			return progresscheck.Request{}, 0, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
		}
	}
	return request, after.EventSequence, nil
}

func (s *Service) nativeProgressDefectReceiptTenant(ctx context.Context, scope nativeScope, item string) (progresscheck.Receipt, bool, error) {
	var defectID tracker.NativeWorkItemID
	var defectState string
	var defectCreated time.Time
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		cursor := corestore.WorkItemBlockerCursor{}
		for range claimPreparationLimit {
			page, err := reader.ProgressDependencies(ctx, scope.project, tracker.NativeWorkItemID(item), cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, relationship := range page[:min(corestore.MaxPageSize, len(page))] {
				blocker, err := reader.WorkItemByNativeID(ctx, relationship.WorkItemID)
				if err != nil {
					return err
				}
				state := stringValue(blocker.WorkflowStateName)
				if relationship.CreatedAt.After(defectCreated) || relationship.CreatedAt.Equal(defectCreated) && relationship.WorkItemID > defectID {
					defectID, defectState, defectCreated = relationship.WorkItemID, state, relationship.CreatedAt
				}
			}
			if len(page) <= corestore.MaxPageSize {
				return nil
			}
			last := page[corestore.MaxPageSize-1]
			cursor = corestore.WorkItemBlockerCursor{ProjectID: last.ProjectID, IssueID: last.IssueID}
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
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
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
