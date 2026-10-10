package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBoardReplayLimit = 2048

func (d *database) commit(ctx context.Context, tx *sql.Tx) error {
	if err := refreshNativeBoard(ctx, tx, d.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func refreshNativeBoard(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO native_board_dirty SELECT work_item_id FROM native_board_items WHERE running=1 AND expires_at<=?", now.UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		return err
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT work_item_id FROM native_board_dirty ORDER BY work_item_id")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if err := refreshNativeBoardCard(ctx, tx, id, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM native_board_dirty")
	return err
}

func refreshNativeBoardCard(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
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
		issue, _, err := readNativeIssueProjection(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		current = &tracker.NativeBoardCard{Issue: issue}
		var attemptID string
		var attemptCount int
		err = tx.QueryRowContext(ctx, "SELECT id,(SELECT count(*) FROM native_attempts WHERE work_item_id=?) FROM native_attempts WHERE work_item_id=? ORDER BY fencing_token DESC LIMIT 1", id, id).Scan(&attemptID, &attemptCount)
		if err == nil {
			attempt, err := readNativeAttempt(ctx, tx, scope, id, attemptID, now)
			if err != nil {
				return err
			}
			current.Attempt = &tracker.NativeBoardAttempt{Count: attemptCount, ID: attempt.AttemptID, Status: attempt.Status, RunnerID: attempt.RunnerID, MachineID: string(attempt.MachineID), Identity: attempt.Identity, StartedAt: attempt.StartedAt, ExpiresAt: attempt.LeaseExpiresAt}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if found {
			detail, err := readCurrentChangeDetail(ctx, tx, scope, change, now)
			if err != nil {
				return err
			}
			version, _, err := readNativeChangeVersion(ctx, tx, change)
			if err != nil {
				return err
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
		var rank int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT CAST(key AS INTEGER) FROM json_each(states_json) WHERE json_extract(value,'$.name')=?),2147483647) FROM projects WHERE id=?", issue.State, issue.ProjectID).Scan(&rank); err != nil {
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
		if current.Attempt != nil {
			expires = current.Attempt.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
		running := !issue.Terminal && current.Attempt != nil && current.Attempt.Status == "running"
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
	result, err := tx.ExecContext(ctx, "INSERT INTO native_board_deltas(organization_id,project_id,previous_json,current_json) VALUES(?,?,?,?)", scope.organization, scope.project, before, after)
	if err != nil {
		return err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_board_sequences(organization_id,project_id,sequence) VALUES(?,?,?) ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=excluded.sequence`, scope.organization, scope.project, sequence); err != nil {
		return err
	}
	var floor sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT sequence FROM native_board_deltas WHERE organization_id=? AND project_id=? ORDER BY sequence DESC LIMIT 1 OFFSET ?", scope.organization, scope.project, nativeBoardReplayLimit).Scan(&floor); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if floor.Valid {
		if _, err := tx.ExecContext(ctx, "DELETE FROM native_board_deltas WHERE organization_id=? AND project_id=? AND sequence<=?", scope.organization, scope.project, floor.Int64); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE native_board_sequences SET replay_floor=? WHERE organization_id=? AND project_id=?", floor.Int64, scope.organization, scope.project); err != nil {
			return err
		}
	}
	return nil
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
	frame := tracker.NativeBoardFrame{Deltas: []tracker.NativeBoardDelta{}}
	var floor int64
	err := q.QueryRowContext(ctx, "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&frame.Sequence, &floor)
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
	grants, err := nativeBoardGrants(ctx, q, scope)
	if err != nil {
		return frame, err
	}
	rows, err := q.QueryContext(ctx, "SELECT sequence,previous_json,current_json FROM native_board_deltas WHERE organization_id=? AND project_id=? AND sequence>? ORDER BY sequence LIMIT ?", scope.organization, scope.project, since, nativeBoardReplayLimit)
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
	if card.Attempt != nil && card.Attempt.Status == "running" {
		return "Running"
	}
	blockers := 0
	for _, blocker := range card.Issue.Blockers {
		if !blocker.Terminal {
			blockers++
		}
	}
	if blockers > 0 {
		return "Blocked · " + strconv.Itoa(blockers)
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
