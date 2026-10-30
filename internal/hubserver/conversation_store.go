package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

// conversationStore is the durable storage for the hosted conversation
// product. Transactional operations take the caller's *sql.Tx so that state
// and the events describing it commit together.
type conversationStore struct {
	db       *sql.DB
	reader   *sql.DB
	contents attachment.ArchiveContentStore
	database *database
}

func newConversationStore(db *sql.DB) *conversationStore {
	return &conversationStore{db: db}
}

func conversationInvalidCursor() error {
	return &nativeError{Code: "invalid_request", Message: "Cursor is not valid", status: http.StatusUnprocessableEntity, publicMessage: true}
}

const (
	conversationOriginUser   = hubconversation.OriginUser
	conversationOriginWorker = hubconversation.OriginWorker
)

type conversationScanner interface {
	Scan(dest ...any) error
}

type conversationCursor = hubconversation.Cursor

// Message references.

// resolveMessageReferences fills the references of every message in one
// query. The hub pool holds a single connection, so the caller must have
// drained its own rows first. The rows come back in insertion order, which
// is the order the tokens appeared in the message text: replaceMessageReferences
// rewrites a message's rows as a set, so rowid order is extraction order.
func resolveMessageReferences(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := sqlList(ids)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	rows, err := querySQL(ctx, query, `SELECT r.message_id, r.target_kind, r.target_id, r.label,
CASE WHEN r.target_kind='issue' THEN '/' || o.slug || '/' || p.slug || '/issues/' || i.number ELSE '/chat/c/' || r.target_id END
FROM message_references r LEFT JOIN issues i ON r.target_kind='issue' AND i.native_id=r.target_id LEFT JOIN projects p ON p.id=i.project_id LEFT JOIN organizations o ON o.id=i.organization_id
WHERE r.message_id IN (SELECT value FROM sql_rows(?)) ORDER BY r.message_id, r.rowid`, encoded)
	if err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byMessage := map[string][]conversationMessageReference{}
	for rows.Next() {
		var messageID string
		var reference conversationMessageReference
		if err := rows.Scan(&messageID, &reference.Kind, &reference.ID, &reference.Label, &reference.URL); err != nil {
			return fmt.Errorf("read message references: %w", err)
		}
		byMessage[messageID] = append(byMessage[messageID], reference)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	for _, message := range messages {
		message.References = byMessage[message.ID]
	}
	return nil
}
