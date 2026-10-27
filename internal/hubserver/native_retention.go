package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	commandReceiptRetention  = 24 * time.Hour
	commandReceiptPruneBatch = 5000
	hostedAuditRetention     = 30 * 24 * time.Hour
)

func (s *Service) maintainNativeRetention(ctx context.Context, now time.Time) error {
	stages := []struct {
		name string
		run  func(context.Context) error
	}{
		{"populate conversation query metadata", s.convertConversationQueryMetadata},
		{"populate claim facts", func(ctx context.Context) error {
			return s.database.populateClaimFacts(s.contentContext(ctx))
		}},
		{"populate runner facts", func(ctx context.Context) error {
			return s.database.populateRunnerFacts(s.runDetailContext(s.contentContext(ctx)))
		}},
		{"populate archive facts", s.database.populateArchiveFacts},
		{"populate hosted import fields", s.database.populateHostedImportFields},
		{"populate time indexes", s.database.populateTimeIndexes},
		{"expire native records", func(ctx context.Context) error { return s.expireNativeRecords(ctx, now) }},
		{"archive closed issues", func(ctx context.Context) error { return s.archiveClosedNativeIssues(ctx, now) }},
		{"compact retained events", func(ctx context.Context) error { return s.compactRetainedEvents(ctx, now) }},
		{"compact analytics samples", func(ctx context.Context) error { return s.database.compactAnalyticsSamples(ctx, now) }},
		{"reclaim storage", s.database.reclaimStorage},
	}
	var failures []error
	for _, stage := range stages {
		stageCtx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
		err := stage.run(stageCtx)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", stage.name, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) expireNativeRecords(ctx context.Context, now time.Time) error {
	if _, err := s.database.db.ExecContext(ctx, `DELETE FROM hosted_audit WHERE rowid IN (SELECT rowid FROM hosted_audit
WHERE route LIKE 'GET %' AND status<400 AND actual_actor=effective_user AND reason=''
AND event IN ('action','administration','key_call','billing_viewed','billing_exported') ORDER BY id LIMIT ?)`, commandReceiptPruneBatch); err != nil {
		return fmt.Errorf("delete legacy successful audit reads: %w", err)
	}
	if _, err := s.database.db.ExecContext(ctx, `DELETE FROM hosted_audit WHERE rowid IN (SELECT audit_id FROM hosted_audit_times
WHERE recorded_at_ns<? ORDER BY recorded_at_ns,audit_id LIMIT ?)`, now.Add(-hostedAuditRetention).UnixNano(), commandReceiptPruneBatch); err != nil {
		return fmt.Errorf("expire hosted audit: %w", err)
	}
	if _, err := s.database.db.ExecContext(ctx, `DELETE FROM native_commands WHERE rowid IN (SELECT rowid FROM native_commands
WHERE (operation LIKE 'POST /%' OR operation LIKE 'PUT /%' OR operation LIKE 'PATCH /%' OR operation LIKE 'DELETE /%')
AND operation NOT LIKE '%/conversations' AND instr(operation, '/conversations/')=0
AND julianday(created_at)<julianday(?) LIMIT ?)`, formatHubTime(now.Add(-commandReceiptRetention)), commandReceiptPruneBatch); err != nil {
		return fmt.Errorf("expire command receipts: %w", err)
	}
	if _, err := s.database.db.ExecContext(ctx, `DELETE FROM native_duplicate_challenges WHERE rowid IN (
SELECT rowid FROM native_duplicate_challenges WHERE consumed_at IS NOT NULL OR julianday(created_at)<=julianday(?) LIMIT ?)`, formatHubTime(now.Add(-duplicateChallengeLifetime)), commandReceiptPruneBatch); err != nil {
		return fmt.Errorf("expire duplicate challenges: %w", err)
	}
	if _, err := s.database.db.ExecContext(ctx, "UPDATE attachments SET deleted_at=? WHERE rowid IN (SELECT rowid FROM attachments WHERE kind='attachment' AND retention_class='expiring' AND deleted_at IS NULL AND work_item_id IS NULL AND julianday(created_at)<=julianday(?) AND NOT EXISTS (SELECT 1 FROM release_audit_parts WHERE attachment_id=attachments.id) LIMIT ?)", formatHubTime(now), formatHubTime(now.Add(-attachment.OrphanTTL)), commandReceiptPruneBatch); err != nil {
		return err
	}
	if _, err := s.database.db.ExecContext(ctx, "DELETE FROM runner_host_hours WHERE rowid IN (SELECT rowid FROM runner_host_hours WHERE hour <= ? LIMIT ?)", formatHubTime(now.UTC().Truncate(time.Hour).Add(-720*time.Hour)), commandReceiptPruneBatch); err != nil {
		return err
	}
	period := "CASE WHEN ws.detent_state = 'Cancelled' THEN p.archive_cancelled_after_days ELSE p.archive_completed_after_days END"
	restored := `(SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.edited' AND e.archive_operation='restore' ORDER BY e.sequence DESC LIMIT 1)`
	entered := "julianday(" + nativeTerminalEnteredAt + ")"
	selection := `SELECT i.organization_id, i.project_id, i.native_id, ` + period + `
FROM issues i JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE p.deleted_at IS NULL AND p.profile='native' AND i.archived=0 AND ws.terminal=1 AND ` + period + ` IS NOT NULL
AND max(` + entered + `, COALESCE(julianday(` + restored + `), ` + entered + `)) < julianday(?) - (` + period + `)
AND NOT EXISTS (SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.edited' AND e.archive_operation IS NULL)
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND NOT EXISTS (SELECT 1 FROM change_issue_links l JOIN change_requests c ON c.id=l.change_id
WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id
AND (c.claim_version_id IS NULL OR c.archive_landed_version_id IS NULL OR (c.claim_version_id<>'' AND c.archive_landed_version_id<>c.claim_version_id)))
`
	rows, err := s.database.reader.QueryContext(ctx, selection+` ORDER BY i.id LIMIT 200`, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		scope nativeScope
		item  string
		days  int
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.scope.organization, &item.scope.project, &item.item, &item.days); err != nil {
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		if err := func() error {
			tx, err := s.database.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			item.scope.sourceActor = nativeIntegrationActor("auto_archive")
			err = tx.QueryRowContext(ctx, selection+` AND i.organization_id=? AND i.project_id=? AND i.native_id=?`, formatHubTime(now), formatHubTime(now), item.scope.organization, item.scope.project, item.item).Scan(&item.scope.organization, &item.scope.project, &item.item, &item.days)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			issue, _, err := readNativeIssue(ctx, tx, item.scope, item.item)
			if err != nil {
				return err
			}
			issue.Archived = true
			data := tracker.CollaborationData{Fields: []string{"archived"}, Operation: "archive", ReasonDetail: fmt.Sprintf("Archived automatically after %d days", item.days)}
			if _, err := persistNativeIssue(ctx, tx, item.scope, issue, "issue.edited", data, now); err != nil {
				return err
			}
			return s.database.commit(ctx, tx)
		}(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) compactRetainedEvents(ctx context.Context, now time.Time) error {
	plans, err := planEventCompaction(ctx, s.database.reader, now)
	if err != nil || len(plans) == 0 {
		return err
	}
	for _, plan := range plans {
		if err := s.compactRetainedEvent(ctx, plan, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) compactRetainedEvent(ctx context.Context, plan eventCompactionPlan, now time.Time) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	conversations, removed, err := applyEventCompaction(ctx, tx, []eventCompactionPlan{plan}, now)
	if err != nil {
		return err
	}
	if err := s.database.commit(ctx, tx); err != nil {
		return err
	}
	if s.conversations != nil {
		for _, id := range conversations {
			s.conversations.broker.notify(id)
		}
	}
	if removed > 0 {
		s.config.Logger.InfoContext(ctx, "compacted retained events", "removed_events", removed)
	}
	return nil
}
