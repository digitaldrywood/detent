package templates_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/symphony/internal/web/chart"
	"github.com/digitaldrywood/symphony/internal/web/templates"
)

func TestSparklineChartRendersAccessibleSVG(t *testing.T) {
	t.Parallel()

	html := renderComponent(t, templates.SparklineChart(templates.SeriesChartData{
		Title:     "Token sparkline",
		AriaLabel: "Token sparkline",
		Points: []chart.Point{
			{Label: "14:55", Value: 20_000},
			{Label: "15:00", Value: 200_000},
		},
	}))

	for _, want := range []string{
		"<svg",
		`role="img"`,
		`aria-label="Token sparkline"`,
		`viewBox="0 0 240 80"`,
		"<title>Token sparkline</title>",
		`stroke="currentColor"`,
		"14:55: 20,000",
		"15:00: 200,000",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sparkline missing %q:\n%s", want, html)
		}
	}
}

func TestLineAreaChartRendersAreaPath(t *testing.T) {
	t.Parallel()

	html := renderComponent(t, templates.LineAreaChart(templates.SeriesChartData{
		Title:     "Token trend",
		AriaLabel: "Token trend",
		Points: []chart.Point{
			{Label: "Input", Value: 100},
			{Label: "Output", Value: 250},
			{Label: "Total", Value: 350},
		},
	}))

	for _, want := range []string{
		"<title>Token trend</title>",
		`fill="currentColor"`,
		`opacity="0.18"`,
		`stroke="currentColor"`,
		"Total: 350",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("line area chart missing %q:\n%s", want, html)
		}
	}
}

func TestSplitSeriesChartRendersInputOutputTrend(t *testing.T) {
	t.Parallel()

	html := renderComponent(t, templates.SplitSeriesChart(templates.SplitSeriesChartData{
		Title:       "Token trend",
		AriaLabel:   "Token trend",
		InputLabel:  "Input",
		OutputLabel: "Output",
		ValueSuffix: "tokens",
		Points: []templates.SplitSeriesPoint{
			{Label: "15:00", Input: 120, Output: 40},
			{Label: "15:01", Input: 240, Output: 60},
			{Label: "15:02", Input: 360, Output: 90},
		},
	}))

	for _, want := range []string{
		"<title>Token trend</title>",
		`aria-label="Token trend"`,
		`text-accent`,
		`text-success`,
		" C ",
		"Input 15:00: 120 tokens",
		"Output 15:02: 90 tokens",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("split series chart missing %q:\n%s", want, html)
		}
	}
}

func TestMiniBarAndTimelineChartsRenderTitles(t *testing.T) {
	t.Parallel()

	barHTML := renderComponent(t, templates.MiniBarChart(templates.BarChartData{
		Title:     "Token bars",
		AriaLabel: "Token bars",
		Bars: []chart.Point{
			{Label: "Input", Value: 120},
			{Label: "Output", Value: 80},
		},
	}))
	for _, want := range []string{
		`aria-label="Token bars"`,
		"<rect",
		"<title>Input: 120</title>",
		"<title>Output: 80</title>",
	} {
		if !strings.Contains(barHTML, want) {
			t.Fatalf("bar chart missing %q:\n%s", want, barHTML)
		}
	}

	timelineHTML := renderComponent(t, templates.TimelineChart(templates.TimelineChartData{
		Title:     "Workflow timeline",
		AriaLabel: "Workflow timeline",
		Segments: []templates.TimelineSegment{
			{Label: "Todo", Value: 2, Class: "text-warning"},
			{Label: "In Progress", Value: 3, Class: "text-accent"},
			{Label: "Done", Value: 5, Class: "text-success"},
		},
	}))
	for _, want := range []string{
		`aria-label="Workflow timeline"`,
		"<title>Workflow timeline</title>",
		"<title>Todo: 2</title>",
		"<title>Done: 5</title>",
	} {
		if !strings.Contains(timelineHTML, want) {
			t.Fatalf("timeline chart missing %q:\n%s", want, timelineHTML)
		}
	}
}

func renderComponent(t *testing.T, component templ.Component) string {
	t.Helper()

	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}
