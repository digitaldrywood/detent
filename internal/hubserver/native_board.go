package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBoardReplayLimit = 2048
const nativeBoardTrimLimit = 32

func (s *Service) maintainNativeBoard(ctx context.Context) {
	if s.database.postgres != nil {
		tenant := hubmaintenance.Tenant{Organization: s.appOrganizationID(), Generation: 1}
		if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
			tenant.Generation = s.config.Hosted.SharedEntry.Generation
		}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
		if err := s.WorkItemPageMaintenanceFeature().Step(budget.Context(ctx, tenant), tenant, budget); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "refresh work item page", "error", err)
		}
	}
	if s.config.TenantRepository != nil {
		organization := tracker.OrganizationID(s.appOrganizationID())
		var next tracker.ProjectID
		err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			var err error
			next, err = w.TrimBoardDeltas(ctx, s.boardRetentionScope.project)
			return err
		})
		if err == nil {
			s.boardRetentionScope = nativeScope{organization: organization, project: next}
		} else if !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "trim native board history", "error", err)
		}
	}
	if err := s.maintainTeamUnread(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "refresh team unread summary", "error", err)
	}
	if err := s.maintainBoardRecords(ctx); err != nil {
		failures := []error{err}
		if batch, ok := err.(interface{ Unwrap() []error }); ok {
			failures = batch.Unwrap()
		}
		for _, failure := range failures {
			if errors.Is(failure, context.Canceled) {
				continue
			}
			attributes := []any{"error", failure}
			var card *boardRefreshError
			if errors.As(failure, &card) {
				attributes = append(attributes, "work_item_id", card.item, "step", card.step)
			}
			s.config.Logger.ErrorContext(ctx, "refresh native board card", attributes...)
		}
	}
}

func (d *database) commit(_ context.Context, tx *sql.Tx) error {
	return tx.Commit()
}

func resetNativeBoardPolicy(ctx context.Context, tx *sql.Tx, scope string) error {
	var organization, project string
	var sequence int64
	err := tx.QueryRowContext(ctx, `INSERT INTO native_board_deltas(organization_id,project_id,previous_json,current_json)
SELECT organization_id,id,NULL,NULL FROM projects WHERE organization_id||'/'||id=? AND profile='native'
RETURNING organization_id,project_id,sequence`, scope).Scan(&organization, &project, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES(?,?,?,?)
ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=excluded.sequence,replay_floor=excluded.replay_floor`, organization, project, sequence, sequence)
	return err
}

func redactNativeBoardCard(card *tracker.NativeBoardCard, grants map[tracker.ProjectID]bool) {
	if card == nil {
		return
	}
	card.Issue.Dependencies = []tracker.NativeWorkItemID{}
	card.Issue.Blockers = slices.DeleteFunc(card.Issue.Blockers, func(blocker tracker.NativeDependency) bool { return !grants[blocker.ProjectID] })
	for _, blocker := range card.Issue.Blockers {
		card.Issue.Dependencies = append(card.Issue.Dependencies, blocker.ID)
	}
	card.StatusLine = nativeBoardStatusLine(*card)
}

func nativeBoardStatusLine(card tracker.NativeBoardCard) string {
	if card.Issue.Terminal {
		return card.Issue.State
	}
	blockers := 0
	number := 0
	for _, blocker := range card.Issue.Blockers {
		if !blocker.Terminal {
			blockers++
			number = blocker.Number
		}
	}
	if blockers > 0 {
		if blockers == 1 && number > 0 {
			return "Waiting on #" + strconv.Itoa(number)
		}
		return "Waiting on " + strconv.Itoa(blockers)
	}
	if card.Execution != nil {
		if label := nativeBoardExecutionLabel(card.Execution.Stage); label != "" {
			return label
		}
		if card.Execution.Running {
			return "Running"
		}
		if card.Attempt == nil || card.Attempt.ID != card.Execution.ID {
			return ""
		}
	}
	if card.Attempt != nil {
		switch card.Attempt.Status {
		case "failed":
			return "Attempt failed"
		case "interrupted":
			return "Interrupted"
		}
	}
	return ""
}
