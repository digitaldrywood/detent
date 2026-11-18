package hubserver

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type staleLandingItem struct {
	Source healthstore.ItemSource
	Change eventstore.Change
	Found  bool
}

type staleLandingRoutes struct {
	lanes []string
	items []staleLandingItem
	moves []*changeWorkflowTransition
}

type staleLandingReader interface {
	LatestChange(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (eventstore.Change, error)
}

func readStaleLandingItems(ctx context.Context, r staleLandingReader, project tracker.ProjectID, lanes []string) ([]staleLandingItem, error) {
	owner, ok := r.(healthstore.SourceReader)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	var items []staleLandingItem
	for _, lane := range lanes {
		var after int64
		complete := false
		for range claimPreparationLimit {
			rows, err := owner.HealthSourceItems(ctx, project, lane, after, corestore.MaxPageSize)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				item := staleLandingItem{Source: row}
				change, err := r.LatestChange(ctx, project, row.WorkItemID)
				if err == nil {
					item.Change, item.Found = change, true
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return nil, err
				}
				items = append(items, item)
			}
			if len(rows) < corestore.MaxPageSize {
				complete = true
				break
			}
			after = rows[len(rows)-1].ID
		}
		if !complete {
			return nil, corestore.ErrInvalidPage
		}
	}
	return items, nil
}

func staleLandingTarget(project tracker.NativeProject, current string) string {
	states := make([]connector.WorkflowState, len(project.States))
	for i, lane := range project.States {
		states[i] = connector.WorkflowState{Name: lane.Name, Dispatchable: lane.Dispatchable, Terminal: lane.Terminal, OperatorOnly: lane.OperatorOnly, Transitions: lane.Transitions}
	}
	target, _ := connector.LandingRefusalLane(states, current, "Rework", true)
	return target
}

func (s *Service) prepareStaleLandingRoutes(ctx context.Context, scope hubscope.NativeScope, approval hubrecords.ChangeVersionApproval, now time.Time) (*staleLandingRoutes, error) {
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	routes := &staleLandingRoutes{}
	for _, state := range project.States {
		if !state.Terminal && strings.EqualFold(strings.TrimSpace(state.Name), landingLane) {
			routes.lanes = append(routes.lanes, state.Name)
		}
	}
	if len(routes.lanes) == 0 {
		return routes, nil
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		routes.items, err = readStaleLandingItems(ctx, r, scope.Project, routes.lanes)
		return err
	})
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	for _, item := range routes.items {
		current := item.Change.CurrentVersionID
		if !item.Found || current == nil || item.Change.LandedVersionID != nil && *item.Change.LandedVersionID == *current {
			continue
		}
		var row eventstore.Version
		var active bool
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			if row, err = r.Version(ctx, item.Change.ID, *current); err != nil {
				return err
			}
			active, err = r.WorkItemHasActiveLease(ctx, scope.Project, item.Source.WorkItemID, now)
			return err
		})
		if err != nil {
			return nil, err
		}
		if active {
			continue
		}
		version, err := content.Version(ctx, row)
		if err != nil {
			return nil, err
		}
		policyID, reviewID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
		if policyID == version.PolicyID && reviewID == version.ReviewPolicy.ID {
			continue
		}
		change, err := content.Change(ctx, item.Change)
		if err != nil {
			return nil, err
		}
		move, err := s.prepareChangeWorkflowRoute(ctx, scope, change, approval, changeWorkflowRoute{rework: true, reason: "stale_policy", target: staleLandingTarget}, now)
		if err != nil {
			return nil, err
		}
		if move == nil || !move.eligible {
			return nil, hubapi.NativeInvalid("The workflow allows no move from " + landingLane + " to a landing refusal lane")
		}
		routes.moves = append(routes.moves, move)
	}
	return routes, nil
}

func (s *Service) applyStaleLandingRoutes(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, routes *staleLandingRoutes) error {
	if routes == nil || len(routes.lanes) == 0 {
		return nil
	}
	items, err := readStaleLandingItems(ctx, w, scope.Project, routes.lanes)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(items, routes.items) {
		return corestore.ErrRevisionConflict
	}
	for _, move := range routes.moves {
		if _, err := s.lockChangeWorkflowTransition(ctx, w, scope, move); err != nil {
			return err
		}
		if err := s.applyChangeWorkflowTransition(ctx, w, scope, move); err != nil {
			return err
		}
	}
	return nil
}
