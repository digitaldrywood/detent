package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBoardReplayLimit = 2048
const nativeBoardTrimLimit = 128

const nativeBoardCandidateQuery = `SELECT work_item_id,1 FROM (SELECT work_item_id FROM native_board_dirty
WHERE work_item_id>? ORDER BY work_item_id LIMIT 1)
UNION ALL SELECT work_item_id,expires_at<>'' AND expires_at<=? FROM (SELECT work_item_id,expires_at FROM native_board_items
WHERE work_item_id>? ORDER BY work_item_id LIMIT 1)`

func (s *Service) maintainNativeBoard(ctx context.Context) {
	ctx = s.runDetailContext(ctx)
	if err := s.trimNextDashboardProject(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "trim dashboard changes", "error", err)
	}
	deadline := s.config.now().Add(analyticsBackfillBudget)
	for ctx.Err() == nil {
		now := s.config.now()
		if !now.Before(deadline) {
			return
		}
		more, id, err := s.database.drainNativeBoardDirtyAfter(ctx, now, s.boardRefreshCursor)
		if id != "" {
			s.boardRefreshCursor = id
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "refresh native board card", "work_item_id", id, "error", err)
		}
		if err != nil && id != "" && ctx.Err() == nil {
			continue
		}
		if err == nil && !more {
			s.boardRefreshCursor = ""
		}
		if err != nil || !more {
			return
		}
	}
}

func (d *database) drainNativeBoardDirtyAfter(ctx context.Context, now time.Time, cursor string) (bool, string, error) {
	id, refresh, err := d.nextNativeBoardCandidate(ctx, now, cursor)
	if err != nil || id == "" {
		return false, "", err
	}
	if !refresh {
		return true, id, nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, id, err
	}
	defer tx.Rollback()
	if err := refreshNativeBoardCard(ctx, tx, id, now, map[string]changeSummaryPolicy{}); err != nil {
		return false, id, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM native_board_dirty WHERE work_item_id=?", id); err != nil {
		return false, id, err
	}
	return true, id, tx.Commit()
}

func (d *database) nextNativeBoardCandidate(ctx context.Context, now time.Time, cursor string) (string, bool, error) {
	rows, err := d.reader.QueryContext(ctx, nativeBoardCandidateQuery, cursor, now.UTC().Format("2006-01-02T15:04:05.000000000Z"), cursor)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var id string
	var refresh bool
	for rows.Next() {
		var candidate string
		var due bool
		if err := rows.Scan(&candidate, &due); err != nil {
			return "", false, err
		}
		if id == "" || candidate < id {
			id = candidate
			refresh = due
		} else if candidate == id {
			refresh = refresh || due
		}
	}
	return id, refresh, rows.Err()
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

func refreshNativeBoardCard(ctx context.Context, tx *sql.Tx, id string, now time.Time, policies map[string]changeSummaryPolicy) error {
	var previous *tracker.NativeBoardCard
	var oldJSON string
	err := tx.QueryRowContext(ctx, "SELECT record_json FROM native_board_items WHERE work_item_id=?", id).Scan(&oldJSON)
	if err == nil {
		if err := json.Unmarshal([]byte(oldJSON), &previous); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	scope := nativeScope{credential: apiCredential{Scope: apiScopeAdmin}}
	var workspace bool
	err = tx.QueryRowContext(ctx, "SELECT i.organization_id,i.project_id, NOT ("+notWorkspaceItemClause+") FROM issues i WHERE i.native_id=?", id).Scan(&scope.organization, &scope.project, &workspace)
	var current *tracker.NativeBoardCard
	if err == nil && !workspace {
		issue, _, err := readNativeIssueProjection(withDisplayLandingFacts(ctx), tx, scope, id, true)
		if err != nil {
			return fmt.Errorf("read issue projection: %w", err)
		}
		current = &tracker.NativeBoardCard{Issue: issue}
		var latest *tracker.NativeAttempt
		var attemptID string
		var attemptCount int
		err = tx.QueryRowContext(ctx, `SELECT id,(SELECT count(*) FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=?)
FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT 1`,
			scope.organization, scope.project, id, scope.organization, scope.project, id).Scan(&attemptID, &attemptCount)
		if err == nil {
			attempt, err := readNativeAttemptRecord(ctx, tx, scope, id, attemptID, now)
			if err != nil {
				return fmt.Errorf("read latest attempt: %w", err)
			}
			current.Attempt = &tracker.NativeBoardAttempt{Count: attemptCount, ID: attempt.AttemptID, Status: attempt.Status, RunnerID: attempt.RunnerID, MachineID: string(attempt.MachineID), Identity: nativeAttemptDisplayIdentity(attempt), StartedAt: attempt.StartedAt, ExpiresAt: attempt.LeaseExpiresAt}
			latest = &attempt
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current.Execution, err = readNativeBoardExecution(ctx, tx, scope, id, latest, now)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read execution: %w", err)
		}
		change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, id)
		if err != nil {
			return fmt.Errorf("read latest change: %w", err)
		}
		if found {
			key := string(scope.organization) + "/" + string(scope.project)
			currentPolicy, found := policies[key]
			if !found {
				currentPolicy, err = readChangeSummaryPolicy(ctx, tx, scope)
				if err != nil {
					return fmt.Errorf("read change summary policy: %w", err)
				}
				policies[key] = currentPolicy
			}
			detail, err := readCurrentChangeDetailWithPolicy(ctx, tx, scope, change, now, &currentPolicy)
			if err != nil {
				return fmt.Errorf("read change detail: %w", err)
			}
			var version tracker.ChangeVersion
			if len(detail.Versions) > 0 {
				version = detail.Versions[0]
			}
			current.Change = &tracker.NativeBoardChange{ID: change.ID, Title: change.Title, Status: detail.Summary.Status, External: version.External}
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != nil {
		current.StatusLine = nativeBoardStatusLine(*current)
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if current == nil && previous == nil || string(raw) == oldJSON {
		return nil
	}
	if current == nil {
		if previous == nil {
			return nil
		}
		scope.organization, scope.project = previous.Issue.OrganizationID, previous.Issue.ProjectID
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM native_board_items WHERE work_item_id=?", id); err != nil {
		return err
	}
	if current != nil {
		issue := current.Issue
		rank, err := readNativeBoardRank(ctx, tx, string(issue.ProjectID), issue.State)
		if err != nil {
			return err
		}
		priority := 4
		if issue.Priority != nil && *issue.Priority >= 0 && *issue.Priority <= 3 {
			priority = *issue.Priority
		}
		if issue.Terminal {
			priority = 0
		}
		activity := strings.TrimRight(formatHubTime(issue.LastActivityAt), "Z")
		missing := 0
		if issue.LastActivityAt.IsZero() {
			activity, missing = "", 1
		}
		closed := ""
		if issue.ClosedAt != nil {
			closed = issue.ClosedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
		expires := ""
		if current.Execution != nil {
			expires = current.Execution.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
		running := !issue.Terminal && current.Execution != nil && current.Execution.Running
		if _, err := tx.ExecContext(ctx, `INSERT INTO native_board_items VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, issue.OrganizationID, issue.ProjectID, issue.State, issue.Archived, issue.Terminal, running, rank, priority, missing, activity, string(issue.ProjectID)+"#"+strconv.Itoa(issue.Number), closed, expires, string(raw)); err != nil {
			return err
		}
	}
	var before, after any
	if previous != nil {
		before = oldJSON
	}
	if current != nil {
		after = string(raw)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO native_board_deltas(organization_id,project_id,previous_json,current_json,entity_id,entity_revision,kind) VALUES(?,?,?,?,?,?,?)", scope.organization, scope.project, before, after, id, 0, nativeBoardChangeKind(current))
	if err != nil {
		return err
	}
	return trimDashboardChanges(ctx, tx, scope)
}

func nativeBoardGrants(ctx context.Context, q nativeQueryer, scope nativeScope) (map[tracker.ProjectID]bool, error) {
	condition, args := scope.credential.projectGrantSQL("p.organization_id", "p.id")
	ids, err := nativePageIDs(ctx, q, "SELECT p.id FROM projects p WHERE p.organization_id=? AND ("+condition+")", append([]any{scope.organization}, args...)...)
	grants := map[tracker.ProjectID]bool{}
	for _, id := range ids {
		grants[tracker.ProjectID(id)] = true
	}
	return grants, err
}

func nativeBoardGrantVersion(grants map[tracker.ProjectID]bool) string {
	ids := make([]string, 0, len(grants))
	for id := range grants {
		ids = append(ids, string(id))
	}
	slices.Sort(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(digest[:])
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

func readNativeBoardFrame(ctx context.Context, q nativeQueryer, scope nativeScope, since int64) (tracker.NativeBoardFrame, error) {
	return readNativeBoardFrameThrough(ctx, q, scope, since, 0)
}

func readNativeBoardFrameThrough(ctx context.Context, q nativeQueryer, scope nativeScope, since, through int64) (tracker.NativeBoardFrame, error) {
	frame := tracker.NativeBoardFrame{Deltas: []tracker.NativeBoardDelta{}}
	grants, err := nativeBoardGrants(ctx, q, scope)
	if err != nil {
		return frame, err
	}
	frame.GrantVersion = nativeBoardGrantVersion(grants)
	var floor int64
	err = q.QueryRowContext(ctx, "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&frame.Sequence, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return frame, nil
	}
	if err != nil {
		return frame, err
	}
	if since < floor || since > frame.Sequence {
		frame.Gap = true
		return frame, nil
	}
	if through > 0 {
		frame.Sequence = min(frame.Sequence, through)
	}
	rows, err := q.QueryContext(ctx, "SELECT sequence,previous_json,current_json FROM native_board_deltas WHERE organization_id=? AND project_id=? AND topic='board' AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?", scope.organization, scope.project, since, frame.Sequence, nativeBoardReplayLimit)
	if err != nil {
		return frame, err
	}
	defer rows.Close()
	for rows.Next() {
		var delta tracker.NativeBoardDelta
		var previous, current sql.NullString
		if err := rows.Scan(&delta.Sequence, &previous, &current); err != nil {
			return frame, err
		}
		if previous.Valid {
			if err := json.Unmarshal([]byte(previous.String), &delta.Previous); err != nil {
				return frame, err
			}
		}
		if current.Valid {
			if err := json.Unmarshal([]byte(current.String), &delta.Current); err != nil {
				return frame, err
			}
		}
		if delta.Previous != nil && !scope.reporterOwns(delta.Previous.Issue.Actor) {
			delta.Previous = nil
		}
		if delta.Current != nil && !scope.reporterOwns(delta.Current.Issue.Actor) {
			delta.Current = nil
		}
		if delta.Previous == nil && delta.Current == nil {
			continue
		}
		if scope.credential.HostedRole == "reporter" {
			for _, card := range []*tracker.NativeBoardCard{delta.Previous, delta.Current} {
				if card == nil || card.Change == nil {
					continue
				}
				if _, err := readChange(ctx, q, scope, string(card.Issue.WorkItemID), card.Change.ID); isNativeNotFound(err) {
					card.Change = nil
				} else if err != nil {
					return frame, err
				}
			}
		}
		if delta.Previous != nil {
			if err := readNativeDependencyDetails(ctx, q, scope, &delta.Previous.Issue); errors.Is(err, sql.ErrNoRows) {
				delta.Previous = nil
			} else if err != nil {
				return frame, err
			}
		}
		if delta.Current != nil {
			if err := readNativeDependencyDetails(ctx, q, scope, &delta.Current.Issue); errors.Is(err, sql.ErrNoRows) {
				delta.Current = nil
			} else if err != nil {
				return frame, err
			}
		}
		if delta.Previous != nil {
			delta.Previous.Issue = scope.reporterIssue(delta.Previous.Issue)
		}
		if delta.Current != nil {
			delta.Current.Issue = scope.reporterIssue(delta.Current.Issue)
		}
		redactNativeBoardCard(delta.Previous, grants)
		redactNativeBoardCard(delta.Current, grants)
		frame.Deltas = append(frame.Deltas, delta)
	}
	return frame, rows.Err()
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
