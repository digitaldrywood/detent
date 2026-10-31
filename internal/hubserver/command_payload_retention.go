package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func commandResponseExpired() error {
	return &nativeError{Code: "idempotency_expired", Message: "The command completed; its response replay horizon has passed. The command will not be repeated", status: http.StatusGone, publicMessage: true}
}

func (s *Service) retainCommandPayloads(ctx context.Context, now time.Time) error {
	cutoff := now.UnixNano()
	rows, err := s.database.reader.QueryContext(ctx, `SELECT rowid FROM native_commands WHERE response_pending=0 AND response_expired=0 AND response_replay_until_ns<? ORDER BY response_replay_until_ns LIMIT ?`, cutoff, commandReceiptPruneBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	var expired []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		expired = append(expired, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range expired {
		if _, err := s.database.db.ExecContext(ctx, `UPDATE native_commands SET response_json='{}',response_object_key='',response_expired=1 WHERE rowid=? AND response_pending=0 AND response_expired=0 AND response_replay_until_ns<?`, id, cutoff); err != nil {
			return err
		}
	}
	for range storageConversionRows {
		more, err := s.convertCommandPayload(s.freshRunDetailContext(ctx), now)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (s *Service) convertCommandPayload(ctx context.Context, now time.Time) (bool, error) {
	var id int64
	var organization, operation, raw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT rowid,organization_id,operation,response_json FROM native_commands WHERE response_pending=0 AND response_expired=0 AND response_object_key='' AND response_json<>'{}' AND response_replay_until_ns>=? LIMIT 1`, now.UnixNano()).Scan(&id, &organization, &operation, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ref, err := prepareMeteredContent(commandCompletionContent(ctx), organization, []byte(raw), !strings.HasPrefix(operation, "attachment."))
	if err != nil {
		return false, fmt.Errorf("store command payload: %w", err)
	}
	_, err = s.database.db.ExecContext(ctx, `UPDATE native_commands SET response_json='{}',response_object_key=? WHERE rowid=? AND organization_id=? AND response_json=? AND response_pending=0 AND response_expired=0 AND response_object_key=''`, ref.Key, id, organization, raw)
	return true, err
}

func commandCompletionContent(ctx context.Context) context.Context {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		content.completion = true
		content.mu.Unlock()
	}
	return ctx
}
