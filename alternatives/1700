package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedRunEventState struct {
	eventRowID, attemptRecorded int64
}

func readHostedRunEventState(ctx context.Context, query nativeQueryer, event tracker.NativeRunEvent) (hostedRunEventState, error) {
	var state hostedRunEventState
	err := query.QueryRowContext(ctx, `SELECT
		coalesce((SELECT max(rowid) FROM collaboration_events),0),
		EXISTS(SELECT 1 FROM native_attempt_events WHERE attempt_id=? AND sequence=?)`,
		event.Data.AttemptID, event.Data.Sequence).
		Scan(&state.eventRowID, &state.attemptRecorded)
	return state, err
}

func (d *database) hostedRunEventConsumption(ctx context.Context, query nativeQueryer, now time.Time, before map[string]int64, state hostedRunEventState, event tracker.NativeRunEvent) (map[string]int64, error) {
	var events, eventBytes, attemptRecorded int64
	if err := query.QueryRowContext(ctx, `SELECT
		count(*),coalesce(sum(CASE WHEN `+customerContentEvent+` THEN length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB)) ELSE 0 END),0),
		EXISTS(SELECT 1 FROM native_attempt_events WHERE attempt_id=? AND sequence=?)
		FROM collaboration_events WHERE rowid>?`, event.Data.AttemptID, event.Data.Sequence, state.eventRowID).
		Scan(&events, &eventBytes, &attemptRecorded); err != nil {
		return nil, err
	}
	after, err := d.hostedConsumption(ctx, query, now, "usage_windows")
	if err != nil {
		return nil, err
	}
	after["events_total"] = before["events_total"] + events
	after["history_records"] = before["history_records"] + events + attemptRecorded - state.attemptRecorded
	after["collaboration_bytes"] = before["collaboration_bytes"] + eventBytes
	return after, nil
}
