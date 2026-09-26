package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The typed project event stream (decisions section 12, required by 18.1).
//
// The hosted project stream used to carry one integer on a one-second tick:
// the project's highest issue event sequence, with no id and no body. A client
// could learn that something changed and nothing about what, so every reader
// answered a tick by re-fetching. Section 18.1 requires the opposite for
// workspaces -- "a client observes readiness by subscription and never by
// polling" -- which means the stream has to carry the resource.
//
// Events are committed in the same transaction as the state they describe and
// swept past the retention window. Serving them on the hosted project stream
// is not wired yet; the log is written so a reader can replay from a cursor.

const (
	// projectEventRetention is how long a typed project event stays
	// replayable. It is generous next to the 60-second reconnect window a
	// browser actually needs and small enough that the log never becomes
	// storage.
	projectEventRetention = time.Hour
	// projectEventSweepBatch bounds one sweep so a long-neglected hub does
	// not delete a million rows in one transaction.
	projectEventSweepBatch = 500
)

// appendProjectEvent commits one typed event for a project and returns its
// sequence. It must be called inside the transaction that wrote the state the
// event describes: that is what makes "snapshot plus events after cursor"
// exact rather than nearly exact.
func appendProjectEvent(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, project tracker.ProjectID, eventType, subject string, data any, now time.Time) (int64, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("encode project event %s: %w", eventType, err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM project_events
WHERE organization_id = ? AND project_id = ?`, organization, project).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate project event sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_events (organization_id, project_id, seq, type, subject_id, data_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, organization, project, seq, eventType, subject, string(encoded), formatHubTime(now)); err != nil {
		return 0, fmt.Errorf("insert project event: %w", err)
	}
	return seq, nil
}

// sweepProjectEvents drops events past the retention window and reports how
// many it removed.
func sweepProjectEvents(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	cutoff := formatHubTime(now.Add(-projectEventRetention))
	result, err := db.ExecContext(ctx, `DELETE FROM project_events WHERE rowid IN (
 SELECT rowid FROM project_events WHERE created_at < ? LIMIT ?)`, cutoff, projectEventSweepBatch)
	if err != nil {
		return 0, fmt.Errorf("sweep project events: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count swept project events: %w", err)
	}
	return removed, nil
}
