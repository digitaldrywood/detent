package hubserver

import (
	"context"
	"encoding/hex"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) publishNativeChecklist(c echo.Context, scope hubscope.NativeScope, input progresscheck.Publication) error {
	if input.AttemptID == "" || input.LeaseID == "" || input.FencingToken <= 0 || input.PolicyID == "" || input.RoundID != "" || len(input.Checklist.Checks) > progresscheck.MaxChecklistEntries {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Checklist publication requires a fenced running attempt and bounded checks"))
	}
	ctx := s.runDetailContext(s.contentContext(c.Request().Context()))
	id := tracker.NativeWorkItemID(c.Param("item"))
	list, err := s.freezeNativeChecklist(ctx, scope, id, input)
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Service) freezeNativeChecklist(ctx context.Context, scope hubscope.NativeScope, id tracker.NativeWorkItemID, input progresscheck.Publication) (tracker.ExecutionChecklist, error) {
	var item corestore.WorkItem
	var policyRevision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		item, err = s.nativeExecutionItem(ctx, r, scope, id, true)
		if err != nil {
			return err
		}
		if _, err := s.nativeExecutionAttempt(ctx, r, scope, item, input.AttemptID, input.PolicyID, input.Mutation); err != nil {
			return err
		}
		policyRevision, err = r.ProjectPolicyRevision(ctx, scope.Project, input.PolicyID)
		return err
	})
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	issue, err := s.readWorkItem(ctx, scope, string(id))
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	descriptor, err := s.projectPolicyDescriptor(ctx, eventstore.NewContent(s.runDetailStoreFor(ctx)), scope.Organization, scope.Project, policyRevision)
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	resolved, err := config.ResolveNativePolicy(descriptor)
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	workflow, err := resolved.Workflow()
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	workflow.Config.Policy = descriptor
	list, err := progresscheck.FreezeChecklist(issue.Body, workflow.Config)
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	if list.Revision != input.Checklist.Revision || list.PolicyID != input.PolicyID || !slices.Equal(list.Checks, input.Checklist.Checks) {
		return tracker.ExecutionChecklist{}, hubapi.NativePublicInvalid("Checklist differs from the current issue contract and pinned project policy")
	}
	list.AttemptID, list.FrozenAt = input.AttemptID, s.config.Clock().UTC()
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.LockWorkItem(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if current.Archived || hubapi.StringValue(current.BodyObjectKey) != hubapi.StringValue(item.BodyObjectKey) {
			return terminalProgressConflict()
		}
		attempt, err := s.lockNativeExecutionAttempt(ctx, w, scope, current, input.AttemptID, input.PolicyID, input.Mutation)
		if err != nil {
			return err
		}
		if attempt.Status != "running" {
			return hubapi.NativeStaleExecution("The item attempt has ended")
		}
		writer, ok := w.(leasestore.ChecklistWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return writer.FreezeExecutionChecklist(ctx, scope.Project, list)
	})
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	stored, err := s.readExecutionChecklist(ctx, scope, input.AttemptID)
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	if stored == nil {
		return tracker.ExecutionChecklist{}, corestore.ErrNotFound
	}
	return *stored, nil
}

func (s *Service) readExecutionChecklist(ctx context.Context, scope hubscope.NativeScope, attempt string) (*tracker.ExecutionChecklist, error) {
	var list *tracker.ExecutionChecklist
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(leasestore.ChecklistReader)
		if !ok {
			return nil
		}
		var err error
		list, err = reader.ExecutionChecklist(ctx, scope.Project, attempt)
		return err
	})
	return list, err
}

func (s *Service) publishLoopIntervention(c echo.Context, scope hubscope.NativeScope, input progresscheck.Publication) error {
	event := *input.LoopIntervention
	shape := event.Period >= 1 && event.Period <= 4 && event.Repeats <= progresscheck.ToolLoopWindow && event.Sequence > 0 && event.Sequence <= math.MaxInt64
	action := event.Action == string(progresscheck.ToolLoopSteer) && event.Repeats >= 3 || event.Action == string(progresscheck.ToolLoopStop) && event.Repeats >= 6
	if input.AttemptID == "" || input.RoundID != "" || input.Checklist != nil || input.LeaseID == "" || input.FencingToken <= 0 || input.PolicyID == "" || !shape || !action ||
		event.ThreadID == "" || len(event.ThreadID) > 128 || event.TurnID == "" || len(event.TurnID) > 128 || !validChecklistDigest(event.Revision) || !validChecklistDigest(event.Fingerprint) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Loop intervention requires scoped provider evidence on a fenced running attempt"))
	}
	ctx := c.Request().Context()
	id := tracker.NativeWorkItemID(c.Param("item"))
	var bodyKey string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := s.nativeExecutionItem(ctx, r, scope, id, true)
		if err != nil {
			return err
		}
		if _, err := s.nativeExecutionAttempt(ctx, r, scope, item, input.AttemptID, input.PolicyID, input.Mutation); err != nil {
			return err
		}
		bodyKey = hubapi.StringValue(item.BodyObjectKey)
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	issue, err := s.readWorkItem(ctx, scope, string(id))
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	if event.Revision != progresscheck.ChecklistRevision(issue.Body, input.PolicyID) {
		return s.nativeAPIError(c, terminalProgressConflict())
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.LockWorkItem(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if item.Archived || hubapi.StringValue(item.BodyObjectKey) != bodyKey {
			return terminalProgressConflict()
		}
		attempt, err := s.lockNativeExecutionAttempt(ctx, w, scope, item, input.AttemptID, input.PolicyID, input.Mutation)
		if err != nil {
			return err
		}
		if attempt.Status != "running" {
			return hubapi.NativeStaleExecution("The item attempt has ended")
		}
		writer, ok := w.(leasestore.ChecklistWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		event.ObservedAt = s.config.Clock().UTC()
		return writer.RecordLoopIntervention(ctx, scope.Project, attempt.ID, event)
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

func validChecklistDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
