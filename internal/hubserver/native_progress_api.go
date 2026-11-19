package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressView struct {
	Request progresscheck.Request `json:"request"`
	Record  *progresscheck.Record `json:"record,omitempty"`
}

type nativeProgressSnapshot struct {
	request              progresscheck.Request
	sequence             int64
	completedDependency  corestore.WorkItemReference
	receiptsAfterCleanup []progresscheck.Receipt
}

func (s *Service) nativeProgressRequest(ctx context.Context, scope hubscope.NativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	return s.nativeProgressRequestTenant(ctx, scope, item, policy, instance)
}

func (s *Service) nativeProgressRequestTenant(ctx context.Context, scope hubscope.NativeScope, item, policy, instance string) (progresscheck.Request, int64, error) {
	snapshot, err := s.readNativeProgressSnapshot(ctx, scope, item, policy, instance)
	return snapshot.request, snapshot.sequence, err
}

func (s *Service) readNativeProgressSnapshot(ctx context.Context, scope hubscope.NativeScope, item, policy, instance string) (nativeProgressSnapshot, error) {
	ctx = s.runDetailContext(s.contentContext(ctx))
	before, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return nativeProgressSnapshot{}, err
	}
	var evidenceSequence int64
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		owner, ok := reader.(eventstore.ProgressEvidenceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		evidenceSequence, err = owner.ProgressEvidenceSequence(ctx, scope.Project, before.NativeID)
		return err
	}); err != nil {
		return nativeProgressSnapshot{}, err
	}
	recovery, err := s.readNativeRecoveryCommentsTenant(ctx, scope, item, true)
	if err != nil {
		return nativeProgressSnapshot{}, err
	}
	request := progresscheck.NativeRequest(recovery, policy, instance)
	request.EvidenceRevision = strconv.FormatInt(evidenceSequence, 10)
	defectReceipt, completedDependency, err := s.nativeProgressDefectReceiptTenant(ctx, scope, item)
	if err != nil {
		return nativeProgressSnapshot{}, err
	}
	receiptsAfterCleanup := request.Receipts
	if defectReceipt.ID != "" {
		if len(request.Receipts) == progresscheck.MaxReceipts {
			request.Receipts = request.Receipts[1:]
		}
		request.Receipts = append(request.Receipts, defectReceipt)
	}
	if instance != "" {
		var version string
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			machine, err := reader.Machine(ctx, instance)
			if err != nil {
				return err
			}
			version = machine.Version
			return nil
		})
		if err != nil {
			return nativeProgressSnapshot{}, hubapi.ProjectRecordError(err)
		}
		request.InstanceVersion = version
	}
	after, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return nativeProgressSnapshot{}, err
	}
	if before.Revision != after.Revision || int64(recovery.Issue.Revision) != before.Revision {
		return nativeProgressSnapshot{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	if before.EventSequence != after.EventSequence {
		var currentSequence int64
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			evidence, ok := reader.(eventstore.ProgressEvidenceReader)
			if !ok {
				return errors.New("tenant reader does not support progress evidence")
			}
			var err error
			currentSequence, err = evidence.ProgressEvidenceSequence(ctx, scope.Project, before.NativeID)
			return err
		}); err != nil {
			return nativeProgressSnapshot{}, err
		}
		if currentSequence != evidenceSequence {
			return nativeProgressSnapshot{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
	}
	return nativeProgressSnapshot{request: request, sequence: after.EventSequence, completedDependency: completedDependency, receiptsAfterCleanup: receiptsAfterCleanup}, nil
}

func (s *Service) nativeProgressDefectReceiptTenant(ctx context.Context, scope hubscope.NativeScope, item string) (progresscheck.Receipt, corestore.WorkItemReference, error) {
	var defect corestore.WorkItemReference
	var defectState string
	var defectCreated time.Time
	var owned bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		cursor := corestore.WorkItemBlockerCursor{}
		for range claimPreparationLimit {
			page, err := reader.ProgressDependencies(ctx, scope.Project, tracker.NativeWorkItemID(item), cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, relationship := range page[:min(corestore.MaxPageSize, len(page))] {
				blocker, err := reader.WorkItemByNativeID(ctx, relationship.WorkItemID)
				if err != nil {
					return err
				}
				state := hubapi.StringValue(blocker.WorkflowStateName)
				if relationship.CreatedAt.After(defectCreated) || relationship.CreatedAt.Equal(defectCreated) && relationship.WorkItemID > defect.WorkItemID {
					defect = corestore.WorkItemReference{ProjectID: relationship.ProjectID, WorkItemID: relationship.WorkItemID}
					defectState, defectCreated = state, relationship.CreatedAt
					owned = strings.HasPrefix(hubapi.StringValue(blocker.NativeSourceKey), nativeProgressDependencySourcePrefix)
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
		return progresscheck.Receipt{}, corestore.WorkItemReference{}, hubapi.ProjectRecordError(err)
	}
	if defect.WorkItemID == "" || defectState != "Done" {
		return progresscheck.Receipt{}, corestore.WorkItemReference{}, nil
	}
	receipt := progresscheck.Receipt{ID: string(defect.WorkItemID) + ":done", Step: "system_defect", Outcome: "fixed"}
	if !owned {
		defect = corestore.WorkItemReference{}
	}
	return receipt, defect, nil
}

func (s *Service) getNativeProgress(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
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
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	request, sequence, err := s.nativeProgressRequest(c.Request().Context(), scope, c.Param("item"), input.PolicyID, input.Instance)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	id, err := request.ID()
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	publicationHash, err := hubapi.CommandHash(input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	owner := nativeProgressOwner{service: s, scope: scope, request: request, leaseID: input.LeaseID, fencingToken: input.FencingToken, guardRevision: &sequence, expectedRevision: input.ExpectedRevision, publicationHash: publicationHash}
	if id != input.RequestID {
		record, err := owner.replayAfterCleanup(c.Request().Context(), input.RequestID, id, sequence)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, record)
	}
	if !request.NeedsCheck() {
		return s.nativeAPIError(c, hubapi.NativeInvalid("Progress decision requires a non-progressing previous attempt"))
	}
	record, err := owner.Publish(c.Request().Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record)
}
