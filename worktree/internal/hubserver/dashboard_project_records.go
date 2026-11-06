package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func requireDashboardRecordItem(ctx context.Context, r tenantstore.Reader, scope nativeScope, id string) error {
	row, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *Service) dashboardProjectRecordPage(ctx context.Context, d *dashboardDelivery, sub *dashboardSubscription, scope nativeScope) (bool, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok || sub.workspace != "" {
		return false, workItemStorageUnavailable()
	}
	frames := make([]dashboardFrame, 0, min(d.config.pageSize, corestore.MaxPageSize))
	var head int64
	var grants string
	gap := false
	sequence := sub.sequence
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
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
		grants, err = s.boardAccessVersion(ctx, r, scope)
		if err != nil {
			return err
		}
		watermark, err := r.BoardSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		head = watermark.Sequence
		gap = sequence < watermark.ReplayFloor || sequence > head || sub.grantVersion != "" && sub.grantVersion != grants
		if gap || sequence == head {
			return nil
		}
		rows, err := r.BoardDeltas(ctx, scope.project, sequence, min(d.config.pageSize, corestore.MaxPageSize))
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Sequence != sequence+1 || row.Sequence > head {
				gap = true
				return nil
			}
			sequence = row.Sequence
			if sub.item != "" && sub.item != string(row.WorkItemID) {
				continue
			}
			if scope.credential.HostedRole == "reporter" {
				if err := requireDashboardRecordItem(ctx, r, scope, string(row.WorkItemID)); errors.Is(err, corestore.ErrNotFound) {
					gap = true
					return nil
				} else if err != nil {
					return err
				}
			}
			kind := "upsert"
			if row.Current == nil {
				kind = "delete"
			}
			raw, err := json.Marshal(struct {
				Sequence int64  `json:"sequence"`
				EntityID string `json:"entity_id"`
				Kind     string `json:"kind"`
			}{sequence, string(row.WorkItemID), kind})
			if err != nil {
				return err
			}
			frames = append(frames, dashboardFrame{sequence: sequence, event: "work_item", data: string(raw)})
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
