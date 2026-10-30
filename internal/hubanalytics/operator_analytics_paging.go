package hubanalytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubsql"
)

type PagingKey struct{}

type Paging struct {
	Name         string
	offset       int
	issuesOffset int
	Next         map[string]string
}

func PagingContext(ctx context.Context, cursor string) (context.Context, *Paging, error) {
	if page := analyticsPagingFromContext(ctx); page != nil {
		return ctx, page, nil
	}
	page := &Paging{Next: map[string]string{}}
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		switch parts[0] {
		case "attempts", "landings", "usage", "decisions", "capacity", "residence_issues", "residence_events", "failures", "quality_landings", "quality_events", "quality_evidence", "quality_worked":
		default:
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		offset, err := strconv.Atoi(parts[1])
		issues, issueErr := strconv.Atoi(parts[2])
		if err != nil || issueErr != nil || offset < 0 || offset > int(^uint(0)>>1)-MaxPopulation || issues < 0 || issues > int(^uint(0)>>1)-MaxPopulation {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		page.Name, page.offset, page.issuesOffset = parts[0], offset, issues
	}
	return context.WithValue(ctx, PagingKey{}, page), page, nil
}

func analyticsPagingFromContext(ctx context.Context) *Paging {
	if page, ok := ctx.Value(PagingKey{}).(*Paging); ok {
		return page
	}
	return nil
}

func PopulationOffset(ctx context.Context, name string) int {
	page := analyticsPagingFromContext(ctx)
	if page == nil {
		return 0
	}
	if page.Name == name {
		return page.offset
	}
	if name == "residence_issues" && page.Name == "residence_events" {
		return page.issuesOffset
	}
	return 0
}

type Rows struct {
	rows   *sql.Rows
	err    error
	count  int
	name   string
	page   *Paging
	offset int
}

func QueryAnalyticsRows(ctx context.Context, q hubsql.Queryer, statement string, args ...any) (*Rows, error) {
	rows, err := hubsql.Query(ctx, q, statement, args...)
	return &Rows{rows: rows}, err
}

func QueryAnalyticsPopulation(ctx context.Context, q hubsql.Queryer, name, statement string, args ...any) (*Rows, error) {
	offset := PopulationOffset(ctx, name)
	args = append(args, MaxPopulation+1, offset)
	rows, err := QueryAnalyticsRows(ctx, q, statement, args...)
	if err != nil {
		return rows, err
	}
	rows.name, rows.offset = name, offset
	rows.page = analyticsPagingFromContext(ctx)
	return rows, nil
}

func (r *Rows) Next() bool {
	if !r.rows.Next() {
		return false
	}
	r.count++
	if r.count > MaxPopulation && r.page != nil && r.name != "" {
		issues := r.page.issuesOffset
		if r.page.Name == "residence_issues" {
			issues = r.page.offset
		}
		r.page.Next[r.name] = fmt.Sprintf("%s:%d:%d", r.name, r.offset+MaxPopulation, issues)
	}
	return true
}

func (r *Rows) Scan(dest ...any) error { return r.rows.Scan(dest...) }

func (r *Rows) Err() error { return errors.Join(r.err, r.rows.Err()) }

func (r *Rows) Close() { r.err = errors.Join(r.err, r.rows.Close()) }
