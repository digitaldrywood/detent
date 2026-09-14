package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
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
// The design is the conversation stream's, because that one already solved it:
// events are committed in the same transaction as the state they describe, a
// subscriber is only woken and then reads from its own cursor, and a cursor
// older than the retained window is answered with a re-snapshot rather than a
// gap. The old activity frame is kept beside the typed ones so nothing that
// reads it today has to change.

const (
	// projectEventPage bounds one replay read.
	projectEventPage = 200
	// projectEventRetention is how long a typed project event stays
	// replayable. It is generous next to the 60-second reconnect window a
	// browser actually needs and small enough that the log never becomes
	// storage.
	projectEventRetention = time.Hour
	// projectEventSweepBatch bounds one sweep so a long-neglected hub does
	// not delete a million rows in one transaction.
	projectEventSweepBatch = 500
)

// projectEvent is one committed typed event.
type projectEvent struct {
	Seq       int64
	Type      string
	SubjectID string
	Data      json.RawMessage
	CreatedAt time.Time
}

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

// listProjectEvents reads one page of events after cursor, oldest first.
func listProjectEvents(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID, after int64, limit int) ([]projectEvent, error) {
	rows, err := query.QueryContext(ctx, `SELECT seq, type, subject_id, data_json, created_at FROM project_events
WHERE organization_id = ? AND project_id = ? AND seq > ? ORDER BY seq LIMIT ?`, organization, project, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query project events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := []projectEvent{}
	for rows.Next() {
		var event projectEvent
		var data, created string
		if err := rows.Scan(&event.Seq, &event.Type, &event.SubjectID, &data, &created); err != nil {
			return nil, fmt.Errorf("scan project event: %w", err)
		}
		event.Data = json.RawMessage(data)
		if event.CreatedAt, err = parseTimeValue(created); err != nil {
			return nil, fmt.Errorf("decode project event %d created_at: %w", event.Seq, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project events: %w", err)
	}
	return events, nil
}

// projectEventHead reports the project's highest committed typed sequence, and
// the oldest one still retained. A subscriber whose cursor is below oldest-1
// has lost events and is told to re-snapshot rather than shown a gap.
func projectEventHead(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID) (head, oldest int64, err error) {
	row := query.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0), COALESCE(MIN(seq), 0) FROM project_events
WHERE organization_id = ? AND project_id = ?`, organization, project)
	if err := row.Scan(&head, &oldest); err != nil {
		return 0, 0, fmt.Errorf("read project event head: %w", err)
	}
	return head, oldest, nil
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

// projectEventBroker wakes the subscribers of one project. It carries no
// payload on purpose: the durable log is the queue, and a wake only means
// "read from your cursor again", so a slow subscriber can never make the hub
// hold a backlog in memory.
type projectEventBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[int]chan struct{}
	next        int
}

func newProjectEventBroker() *projectEventBroker {
	return &projectEventBroker{subscribers: map[string]map[int]chan struct{}{}}
}

// projectEventKey scopes a subscription. Two organizations may name projects
// the same way, so the key is both.
func projectEventKey(organization tracker.OrganizationID, project tracker.ProjectID) string {
	return string(organization) + "\x00" + string(project)
}

// subscribe registers interest in a project and returns the wake channel and a
// cancel. The channel has room for one pending wake: a subscriber that has not
// drained the last one does not need a second, because it will read from its
// cursor and see everything either way.
func (b *projectEventBroker) subscribe(organization tracker.OrganizationID, project tracker.ProjectID) (<-chan struct{}, func()) {
	key := projectEventKey(organization, project)
	wake := make(chan struct{}, 1)
	b.mu.Lock()
	id := b.next
	b.next++
	if b.subscribers[key] == nil {
		b.subscribers[key] = map[int]chan struct{}{}
	}
	b.subscribers[key][id] = wake
	b.mu.Unlock()
	return wake, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if channels := b.subscribers[key]; channels != nil {
			delete(channels, id)
			if len(channels) == 0 {
				delete(b.subscribers, key)
			}
		}
	}
}

// notify wakes every subscriber of the project. Call it after the transaction
// that appended events committed, never inside it: a subscriber woken by an
// uncommitted write would read nothing and sleep again.
func (b *projectEventBroker) notify(organization tracker.OrganizationID, project tracker.ProjectID) {
	key := projectEventKey(organization, project)
	b.mu.Lock()
	channels := make([]chan struct{}, 0, len(b.subscribers[key]))
	for _, wake := range b.subscribers[key] {
		channels = append(channels, wake)
	}
	b.mu.Unlock()
	for _, wake := range channels {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
