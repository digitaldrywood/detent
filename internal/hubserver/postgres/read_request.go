package postgres

import (
	"context"
	"database/sql"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type readRequestKey struct{}
type readRequest struct {
	repository *Repository
	session    *session
	mu         sync.Mutex
}

func (r *Repository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, run func(context.Context) error) error {
	return r.transactionIsolation(ctx, organization, corestore.Serving, true, sql.LevelRepeatableRead, func(s *session) error {
		s.credentials = make(map[string]accessstore.Credential)
		s.members = make(map[string]accessstore.Member)
		return run(context.WithValue(ctx, readRequestKey{}, &readRequest{repository: r, session: s}))
	})
}
