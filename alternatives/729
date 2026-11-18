package platformstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type Notification struct{ Event, ID, Email, Name, Plan, OccurredAt string }

func (t *EntryTransaction) EnqueueNotification(ctx context.Context, account bool, value Notification) error {
	query := `INSERT INTO platform.platform_notifications(event,entity_id,email,name,plan,occurred_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event,entity_id) DO NOTHING`
	if account {
		query = `INSERT INTO platform.auth_notifications(event,entity_id,email,name,plan,occurred_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event,entity_id) DO NOTHING`
	}
	_, err := t.Exec(ctx, query, value.Event, value.ID, value.Email, value.Name, value.Plan, value.OccurredAt)
	return t.Classify(ctx, err)
}
func (r *Repository) ClaimNotification(ctx context.Context, workload corestore.Workload, account bool, at string) (Notification, error) {
	query := `UPDATE platform.platform_notifications SET attempted_at=$1 WHERE attempted_at IS NULL AND (event,entity_id)=(SELECT event,entity_id FROM platform.platform_notifications WHERE attempted_at IS NULL ORDER BY occurred_at LIMIT 1) RETURNING event,entity_id,email,name,plan,occurred_at`
	if account {
		query = `UPDATE platform.auth_notifications SET attempted_at=$1 WHERE attempted_at IS NULL AND (event,entity_id)=(SELECT event,entity_id FROM platform.auth_notifications WHERE attempted_at IS NULL ORDER BY occurred_at LIMIT 1) RETURNING event,entity_id,email,name,plan,occurred_at`
	}
	var value Notification
	var occurred time.Time
	err := r.QueryRowEntry(ctx, workload, query, at).Scan(&value.Event, &value.ID, &value.Email, &value.Name, &value.Plan, &occurred)
	if err == nil {
		value.OccurredAt = occurred.UTC().Format(time.RFC3339Nano)
	}
	return value, err
}
