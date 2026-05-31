package templates_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/web/templates"
)

func TestReportsRendersEveryProjectBreakdown(t *testing.T) {
	t.Parallel()

	projects := make([]templates.UsageBucketData, 0, 6)
	var totalTokens int64
	for i := 1; i <= 6; i++ {
		tokens := int64((7 - i) * 100)
		totalTokens += tokens
		projects = append(projects, templates.UsageBucketData{
			Bucket:       "project-" + strconv.Itoa(i),
			Label:        "project-" + strconv.Itoa(i),
			InputTokens:  tokens / 2,
			OutputTokens: tokens / 2,
			TotalTokens:  tokens,
			Events:       1,
		})
	}

	html := renderComponent(t, templates.Reports(templates.ReportsData{
		Title:       "Symphony reports",
		GeneratedAt: time.Date(2026, 5, 31, 17, 0, 0, 0, time.UTC),
		Day: templates.UsageReportData{
			Totals: templates.UsageTotalsData{
				TotalTokens: totalTokens,
				Events:      int64(len(projects)),
			},
		},
		Project: templates.UsageReportData{
			Totals: templates.UsageTotalsData{
				TotalTokens: totalTokens,
				Events:      int64(len(projects)),
			},
			Breakdowns: projects,
		},
	}))

	for i := 1; i <= 6; i++ {
		want := "project-" + strconv.Itoa(i)
		if !strings.Contains(html, want) {
			t.Fatalf("reports page missing %q:\n%s", want, html)
		}
	}
}
