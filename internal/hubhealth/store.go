package hubhealth

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Hooks struct {
	ProjectionPage   func(context.Context, string, string, string, int) ([]any, error)
	ChargeConversion func(context.Context, int) error

	ReportOccurrence func(context.Context, *sql.Tx, Scope, Finding, tracker.NativeWorkItemID, sql.NullString, sql.NullString, time.Time, map[string]Cancellation) (tracker.NativeIssue, bool, error)
	ResolveIssue     func(context.Context, *sql.Tx, Scope, string, time.Time) error
	AfterCommit      func(context.Context, tracker.OrganizationID, []tracker.NativeIssue) error
	SendEmails       func(context.Context, tracker.OrganizationID, []Finding) error

	ReadTimeout  time.Duration
	Invalid      func(string) error
	Watermark    func(context.Context, hubsql.Queryer) (int64, int64, error)
	ReadProject  func(context.Context, hubsql.Queryer) error
	PrepareQuery func(context.Context, *sql.Tx) (hubsql.Queryer, error)
	Visibility   func() (string, []any)

	NewID           func(string) string
	SyncProjects    func(context.Context, *sql.Tx, tracker.OrganizationID, string, []string) error
	AppendDashboard func(context.Context, *sql.Tx, string) error
	EnqueueOpened   func(context.Context, *sql.Tx, tracker.OrganizationID, string, time.Time) error
	EnqueueResolved func(context.Context, *sql.Tx, tracker.OrganizationID, hubsql.ParameterRows, time.Time) error
}
type Store struct {
	db, reader *sql.DB
	hooks      Hooks
}

func NewStore(writer, reader *sql.DB, hooks Hooks) *Store {
	return &Store{db: writer, reader: reader, hooks: hooks}
}
func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
