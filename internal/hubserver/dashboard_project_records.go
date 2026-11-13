package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func requireDashboardRecordItem(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string) error {
	row, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *Service) dashboardProjectRecordPage(ctx context.Context, d *dashboardDelivery, sub *dashboardSubscription, scope hubscope.NativeScope) (bool, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	frames := make([]dashboardFrame, 0, min(d.config.pageSize, corestore.MaxPageSize))
	type actionFrame struct {
		run      postgres.ActionRun
		status   string
		revision int64
	}
	actions := make(map[int]actionFrame)
	var head int64
	var grants string
	gap := false
	sequence := sub.sequence
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if sub.item != "" {
			if err := requireDashboardRecordItem(ctx, r, scope, sub.item); err != nil {
				return err
			}
		}
		var selectedWorkspace postgres.WorkspaceSession
		var selectedResource workspacesession.Session
		if sub.workspace != "" {
			selectedWorkspace, err = s.dashboardWorkspaceRecord(ctx, r, scope, sub.workspace)
			if err != nil {
				return err
			}
			selectedResource, err = s.workspaces.presentWorkspaceRecord(ctx, r, scope, selectedWorkspace)
			if err != nil {
				return err
			}
		}
		grants, err = s.boardAccessVersion(ctx, r, scope)
		if err != nil {
			return err
		}
		watermark, err := r.BoardSequence(ctx, scope.Project)
		if err != nil {
			return err
		}
		head = watermark.Sequence
		if sub.initial && sequence < watermark.ReplayFloor {
			sequence = watermark.ReplayFloor
		}
		gap = sub.initial && scope.Credential.HostedRole == "reporter" || sequence < watermark.ReplayFloor || sequence > head || sub.grantVersion != "" && sub.grantVersion != grants
		if gap || sequence == head {
			return nil
		}
		rows, err := r.BoardDeltas(ctx, scope.Project, sequence, min(d.config.pageSize, corestore.MaxPageSize))
		if err != nil {
			return err
		}
		runs := make(map[string]postgres.ActionRun)
		for _, row := range rows {
			if row.Sequence != sequence+1 || row.Sequence > head {
				gap = true
				return nil
			}
			sequence = row.Sequence
			var frame dashboardFrame
			var visible bool
			if sub.workspace != "" && row.Topic == "workspace" {
				frame = dashboardFrame{sequence: row.Sequence}
				if row.EntityID == sub.workspace && (sub.item == "" || sub.item == selectedWorkspace.SubjectWorkItemID) {
					if row.EntityRevision == nil || *row.EntityRevision < 1 || !workspacesession.ValidState(row.Status) {
						return viewstore.ErrInvalidView
					}
					resource := selectedResource
					resource.State, resource.Revision = row.Status, *row.EntityRevision
					raw, marshalErr := json.Marshal(struct {
						workspacesession.Session
						ChangeSequence int64 `json:"change_sequence"`
					}{resource, row.Sequence})
					if marshalErr != nil {
						return marshalErr
					}
					frame.event, frame.data, visible = workspacesession.EventType(row.Status), string(raw), true
				}
			} else if sub.workspace != "" && row.Topic == "action_run" {
				frame = dashboardFrame{sequence: row.Sequence, event: "action_run." + row.Status}
				if row.RelatedID == sub.workspace && (sub.item == "" || sub.item == selectedWorkspace.SubjectWorkItemID) {
					owner, ok := r.(postgres.RelayActionReader)
					if !ok {
						return hubapi.WorkspaceUnavailable()
					}
					run, found := runs[row.EntityID]
					var readErr error
					if !found {
						run, readErr = owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: row.EntityID})
						if readErr == nil {
							runs[row.EntityID] = run
						}
					}
					if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
						return readErr
					}
					if readErr == nil && run.WorkspaceID == sub.workspace {
						if row.EntityRevision == nil || *row.EntityRevision < 1 || !workspacesession.ValidRunStatus(row.Status) {
							return viewstore.ErrInvalidView
						}
						actions[len(frames)] = actionFrame{run: run, status: row.Status, revision: *row.EntityRevision}
						visible = true
					}
				}
			} else if sub.workspace != "" {
				frame = dashboardFrame{sequence: row.Sequence}
			} else {
				frame, visible, err = s.dashboardRecordFrame(ctx, r, scope, sub, row)
			}
			if err != nil {
				return err
			}
			if !visible {
				frame = dashboardFrame{sequence: row.Sequence, event: "checkpoint", data: "{}"}
			}
			frames = append(frames, frame)
		}
		if len(rows) == 0 && sequence < head {
			gap = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if gap {
		sub.reset(head)
		return false, nil
	}
	if !sub.initial && sub.sequence == head {
		sub.grantVersion = grants
		return false, nil
	}
	resources := make(map[string]workspacesession.Run)
	for i := range frames {
		if action, ok := actions[i]; ok {
			resource, found := resources[action.run.ID]
			if !found {
				record, err := s.actionRunRecord(ctx, action.run, false)
				if err != nil {
					return false, err
				}
				resource = record.resource()
				resources[action.run.ID] = resource
			}
			resource.Status, resource.Revision = action.status, action.revision
			raw, err := json.Marshal(resource)
			if err != nil {
				return false, err
			}
			frames[i].data = string(raw)
		}
	}
	for _, frame := range frames {
		if !sub.enqueue(frame) {
			return false, nil
		}
	}
	if len(frames) == 0 || frames[len(frames)-1].sequence < sequence {
		if !sub.enqueue(dashboardFrame{sequence: sequence, event: "checkpoint", data: "{}"}) {
			return false, nil
		}
	}
	sub.sequence, sub.initial, sub.grantVersion = sequence, false, grants
	return sequence < head, nil
}

func (s *Service) dashboardRecordFrame(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, sub *dashboardSubscription, row viewstore.BoardDelta) (dashboardFrame, bool, error) {
	frame := dashboardFrame{sequence: row.Sequence, event: row.Topic}
	item := string(row.WorkItemID)
	if row.Topic == "activity" || row.Topic == "human_action" || row.Topic == "work_item" {
		item = row.RelatedID
		if item == "" {
			item = row.EntityID
		}
	}
	if sub.item != "" && sub.item != item {
		return frame, false, nil
	}
	if scope.Credential.HostedRole == "reporter" {
		if (row.Topic == "workspace" || row.Topic == "action_run") && item == "" {
			return frame, false, nil
		}
		if row.Topic == "board" || row.Topic == "health" {
			return frame, false, nil
		}
		if item != "" {
			if err := requireDashboardRecordItem(ctx, r, scope, item); errors.Is(err, corestore.ErrNotFound) {
				return frame, false, nil
			} else if err != nil {
				return frame, false, err
			}
		}
	}
	if row.Topic == "activity" {
		frame.data = strconv.FormatInt(row.Sequence, 10)
		return frame, true, nil
	}
	if row.Topic == "card" {
		frame.event = "work_item"
		kind := "upsert"
		if row.Current == nil {
			kind = "delete"
		}
		raw, err := json.Marshal(struct {
			Sequence int64  `json:"sequence"`
			EntityID string `json:"entity_id"`
			Kind     string `json:"kind"`
		}{row.Sequence, string(row.WorkItemID), kind})
		frame.data = string(raw)
		return frame, true, err
	}
	if row.Topic != "work_item" && row.Topic != "human_action" && row.Topic != "lane" && row.Topic != "board" && row.Topic != "health" && row.Topic != "workspace" && row.Topic != "action_run" {
		return frame, false, hubapi.NativeWorkItemStorageUnavailable()
	}
	if row.Topic == "health" {
		frame.event = "health.findings"
	}
	raw, err := json.Marshal(struct {
		Sequence       int64  `json:"sequence"`
		Topic          string `json:"topic"`
		EntityID       string `json:"entity_id"`
		EntityRevision *int64 `json:"entity_revision,omitempty"`
		Kind           string `json:"kind"`
		Status         string `json:"status"`
		RelatedID      string `json:"related_id,omitempty"`
	}{row.Sequence, row.Topic, row.EntityID, row.EntityRevision, row.Kind, row.Status, row.RelatedID})
	frame.data = string(raw)
	return frame, true, err
}

func (s *Service) dashboardWorkspaceRecord(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string) (postgres.WorkspaceSession, error) {
	if s.workspaces == nil {
		return postgres.WorkspaceSession{}, hubapi.NativeNotFound()
	}
	if err := workspacesession.ValidateID(id); err != nil {
		return postgres.WorkspaceSession{}, corestore.ErrNotFound
	}
	owner, ok := r.(postgres.WorkspaceReader)
	if !ok {
		return postgres.WorkspaceSession{}, hubapi.WorkspaceUnavailable()
	}
	row, err := owner.WorkspaceSession(ctx, scope.Project, id)
	if err != nil {
		return row, err
	}
	if err := workspaceReadableSubject(ctx, r, scope, row.SubjectWorkItemID); err != nil {
		return row, err
	}
	return row, nil
}
