package runnerrecords

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Deps struct {
	Organization      tracker.OrganizationID
	DB                DB
	Identity          func(context.Context, string) (runnerstore.Identity, error)
	CheckOrganization func(tracker.OrganizationID) error
	MapError          func(context.Context, error) error
	UpdateOwned       func(context.Context, pgconn.CommandTag, error) error
}

type Store struct {
	organization      tracker.OrganizationID
	db                DB
	identity          func(context.Context, string) (runnerstore.Identity, error)
	checkOrganization func(tracker.OrganizationID) error
	mapError          func(context.Context, error) error
	updateOwned       func(context.Context, pgconn.CommandTag, error) error
}

func New(deps Deps) *Store {
	return &Store{organization: deps.Organization, db: deps.DB, identity: deps.Identity, checkOrganization: deps.CheckOrganization, mapError: deps.MapError, updateOwned: deps.UpdateOwned}
}
