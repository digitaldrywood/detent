package hubanalytics

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Scope struct {
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
}
type RetryState struct {
	mu     sync.Mutex
	cursor string
}
type Hooks struct {
	IssueClause      func(context.Context, string) (string, []any)
	HasIssueFilter   func(context.Context) bool
	DecodeRunHistory func(context.Context, hubsql.Queryer, string, *tracker.CollaborationData) error

	DecodeStorageJSON   func(context.Context, []byte, any) error
	RefreshProductivity func(context.Context, ProjectionQuery, Scope, string) error
	ProjectionRows      int
}
type Store struct {
	db, reader *sql.DB
	analytics  *Statements
	retry      *RetryState
	hooks      Hooks
}

func NewStore(writer, reader *sql.DB, statements *Statements, retry *RetryState, hooks Hooks) *Store {
	return &Store{db: writer, reader: reader, analytics: statements, retry: retry, hooks: hooks}
}
func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
func nativePageIDs(ctx context.Context, query hubsql.Queryer, statement string, args ...any) ([]string, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func collaborationPayloadColumn(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "CASE WHEN " + alias + "payload_version=1 THEN 'event:' || " + alias + "id ELSE " + alias + "data_json END"
}

type DiagnosticsView struct {
	From, To time.Time
	Partial  bool
	Capacity []DiagnosticsCapacity
}

func (d *Store) readResidenceFrames(ctx context.Context, q hubsql.Queryer, scope Scope, w operatortool.AnalyticsWindow, points []time.Time) (ResidenceFrames, error) {
	clause, args := d.hooks.IssueClause(ctx, "i.native_id")
	return ReadResidenceFrames(ctx, q, scope, w, points, clause, args)
}
