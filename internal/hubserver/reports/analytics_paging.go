package reports

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AnalyticsPagingKey struct{}

type AnalyticsPaging struct {
	Name            string `json:"-"`
	PopulationToken string `json:"-"`
	Offset          int    `json:"-"`
	issuesOffset    int
	Next            map[string]string `json:"-"`
}

func AnalyticsPagingContext(ctx context.Context, cursor string) (context.Context, *AnalyticsPaging, error) {
	if page := AnalyticsPagingFromContext(ctx); page != nil {
		return ctx, page, nil
	}
	page := &AnalyticsPaging{Next: map[string]string{}}
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
		if strings.Contains(parts[1], ".") && parts[2] == "0" {
			if len(cursor) > 4096 {
				return ctx, nil, errors.New("invalid analytics population cursor")
			}
			page.Name, page.PopulationToken = parts[0], parts[1]
			return context.WithValue(ctx, AnalyticsPagingKey{}, page), page, nil
		}
		offset, err := strconv.Atoi(parts[1])
		issues, issueErr := strconv.Atoi(parts[2])
		if err != nil || issueErr != nil || offset < 0 || offset > int(^uint(0)>>1)-MaxAnalyticsPopulation || issues < 0 || issues > int(^uint(0)>>1)-MaxAnalyticsPopulation {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		page.Name, page.Offset, page.issuesOffset = parts[0], offset, issues
	}
	return context.WithValue(ctx, AnalyticsPagingKey{}, page), page, nil
}

func AnalyticsPagingFromContext(ctx context.Context) *AnalyticsPaging {
	if page, ok := ctx.Value(AnalyticsPagingKey{}).(*AnalyticsPaging); ok {
		return page
	}
	return nil
}

func analyticsPopulationOffset(ctx context.Context, name string) int {
	page := AnalyticsPagingFromContext(ctx)
	if page == nil {
		return 0
	}
	if page.Name == name {
		return page.Offset
	}
	if name == "residence_issues" && page.Name == "residence_events" {
		return page.issuesOffset
	}
	return 0
}

type AnalyticsResidenceCursor struct {
	IssuesAfter   tracker.NativeWorkItemID
	EventItem     tracker.NativeWorkItemID
	EventSequence int64
}

type AnalyticsFactCursor struct {
	Failure  diagnosticstore.AnalyticsFailureCursor
	Landing  eventstore.AnalyticsLandingCursor
	Workflow eventstore.AnalyticsWorkflowCursor
	Evidence eventstore.AnalyticsEvidenceCursor
}
