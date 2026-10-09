package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type analyticsPagingKey struct{}
type analyticsPaging struct {
	name         string
	offset       int
	issuesOffset int
	next         map[string]string
}

func analyticsPagingContext(ctx context.Context, cursor string) (context.Context, *analyticsPaging, error) {
	if page := analyticsPagingFromContext(ctx); page != nil {
		return ctx, page, nil
	}
	page := &analyticsPaging{next: map[string]string{}}
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		switch parts[0] {
		case "attempts", "landings", "usage", "decisions", "capacity", "residence_issues", "residence_events", "barriers", "failures", "quality_landings", "quality_events", "quality_evidence", "quality_worked":
		default:
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		offset, err := strconv.Atoi(parts[1])
		issues, issueErr := strconv.Atoi(parts[2])
		if err != nil || issueErr != nil || offset < 0 || offset > int(^uint(0)>>1)-maxAnalyticsPopulation || issues < 0 || issues > int(^uint(0)>>1)-maxAnalyticsPopulation {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		page.name, page.offset, page.issuesOffset = parts[0], offset, issues
	}
	return context.WithValue(ctx, analyticsPagingKey{}, page), page, nil
}

func analyticsPagingFromContext(ctx context.Context) *analyticsPaging {
	if page, ok := ctx.Value(analyticsPagingKey{}).(*analyticsPaging); ok {
		return page
	}
	return nil
}

func analyticsPopulationOffset(ctx context.Context, name string) int {
	page := analyticsPagingFromContext(ctx)
	if page == nil {
		return 0
	}
	if page.name == name {
		return page.offset
	}
	if name == "residence_issues" && page.name == "residence_events" {
		return page.issuesOffset
	}
	return 0
}

type analyticsRows struct {
	rows   *sql.Rows
	err    error
	count  int
	name   string
	page   *analyticsPaging
	offset int
}

func queryAnalyticsRows(ctx context.Context, q nativeQueryer, statement string, args ...any) (*analyticsRows, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	return &analyticsRows{rows: rows}, err
}

func queryAnalyticsPopulation(ctx context.Context, q nativeQueryer, name, statement string, args ...any) (*analyticsRows, error) {
	offset := analyticsPopulationOffset(ctx, name)
	args = append(args, maxAnalyticsPopulation+1, offset)
	rows, err := queryAnalyticsRows(ctx, q, statement, args...)
	if err != nil {
		return rows, err
	}
	rows.name, rows.offset = name, offset
	rows.page = analyticsPagingFromContext(ctx)
	return rows, nil
}

func (r *analyticsRows) Next() bool {
	if !r.rows.Next() {
		return false
	}
	r.count++
	if r.count > maxAnalyticsPopulation && r.page != nil && r.name != "" {
		issues := r.page.issuesOffset
		if r.page.name == "residence_issues" {
			issues = r.page.offset
		}
		r.page.next[r.name] = fmt.Sprintf("%s:%d:%d", r.name, r.offset+maxAnalyticsPopulation, issues)
	}
	return true
}
func (r *analyticsRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *analyticsRows) Err() error             { return errors.Join(r.err, r.rows.Err()) }
func (r *analyticsRows) Close()                 { r.err = errors.Join(r.err, r.rows.Close()) }
