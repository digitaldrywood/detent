package hubserver

import (
	"database/sql"

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

const (
	conversationOriginUser   = hubconversation.OriginUser
	conversationOriginWorker = hubconversation.OriginWorker
)

type conversationScanner interface {
	Scan(dest ...any) error
}

// Message references.
