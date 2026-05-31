package templates

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	webchart "github.com/digitaldrywood/symphony/internal/web/chart"
)

type SeriesChartData struct {
	Title       string
	AriaLabel   string
	Points      []webchart.Point
	ValueSuffix string
	Class       string
	ColorClass  string
	Width       float64
	Height      float64
	Padding     float64
}

type BarChartData struct {
	Title       string
	AriaLabel   string
	Bars        []webchart.Point
	ValueSuffix string
	Class       string
	ColorClass  string
	Width       float64
	Height      float64
	Padding     float64
	Gap         float64
}

type TimelineSegment struct {
	Label string
	Value float64
	Class string
}

type TimelineChartData struct {
	Title       string
	AriaLabel   string
	Segments    []TimelineSegment
	ValueSuffix string
	Class       string
	Width       float64
	Height      float64
	Padding     float64
	Gap         float64
}

type seriesChartView struct {
	Title     string
	AriaLabel string
	Class     string
	ViewBox   string
	LinePath  string
	AreaPath  string
	Points    []pointView
}

type barChartView struct {
	Title     string
	AriaLabel string
	Class     string
	ViewBox   string
	Bars      []barView
}

type timelineChartView struct {
	Title     string
	AriaLabel string
	Class     string
	ViewBox   string
	Segments  []timelineSegmentView
}

type pointView struct {
	X     string
	Y     string
	Title string
}

type barView struct {
	X      string
	Y      string
	Width  string
	Height string
	Title  string
}

type timelineSegmentView struct {
	X      string
	Y      string
	Width  string
	Height string
	Class  string
	Title  string
}

func newSparklineChartView(data SeriesChartData) seriesChartView {
	data.Height = chartDimension(data.Height, 80)
	return newSeriesChartView(data, false, "text-success")
}

func newLineAreaChartView(data SeriesChartData) seriesChartView {
	data.Height = chartDimension(data.Height, 120)
	return newSeriesChartView(data, true, "text-accent")
}

func newSeriesChartView(data SeriesChartData, withArea bool, defaultColor string) seriesChartView {
	width := chartDimension(data.Width, 240)
	height := chartDimension(data.Height, 80)
	padding := chartPadding(data.Padding, 8)
	scaled := webchart.ScalePoints(data.Points, webchart.Options{
		Width:   width,
		Height:  height,
		Padding: padding,
	})

	points := make([]pointView, 0, len(scaled))
	for _, point := range scaled {
		points = append(points, pointView{
			X:     webchart.FormatCoord(point.X),
			Y:     webchart.FormatCoord(point.Y),
			Title: chartPointTitle(point.Label, point.Value, data.ValueSuffix),
		})
	}

	linePath := webchart.LinePath(scaled)
	areaPath := ""
	if withArea && linePath != "" {
		areaPath = webchart.AreaPath(scaled, height-padding)
	}

	return seriesChartView{
		Title:     chartText(data.Title, "Chart"),
		AriaLabel: chartText(data.AriaLabel, chartText(data.Title, "Chart")),
		Class: chartClass(
			"block h-20 w-full overflow-visible rounded-md border border-border bg-muted "+chartText(data.ColorClass, defaultColor),
			data.Class,
		),
		ViewBox:  chartViewBox(width, height),
		LinePath: linePath,
		AreaPath: areaPath,
		Points:   points,
	}
}

func newBarChartView(data BarChartData) barChartView {
	width := chartDimension(data.Width, 240)
	height := chartDimension(data.Height, 80)
	padding := chartPadding(data.Padding, 8)
	gap := chartPadding(data.Gap, 4)
	scaled := webchart.ScaleBars(data.Bars, webchart.Options{
		Width:   width,
		Height:  height,
		Padding: padding,
		Gap:     gap,
	})

	bars := make([]barView, 0, len(scaled))
	for _, bar := range scaled {
		bars = append(bars, barView{
			X:      webchart.FormatCoord(bar.X),
			Y:      webchart.FormatCoord(bar.Y),
			Width:  webchart.FormatCoord(bar.Width),
			Height: webchart.FormatCoord(bar.Height),
			Title:  chartPointTitle(bar.Label, bar.Value, data.ValueSuffix),
		})
	}

	return barChartView{
		Title:     chartText(data.Title, "Bar chart"),
		AriaLabel: chartText(data.AriaLabel, chartText(data.Title, "Bar chart")),
		Class: chartClass(
			"block h-20 w-full overflow-visible rounded-md border border-border bg-muted "+chartText(data.ColorClass, "text-accent"),
			data.Class,
		),
		ViewBox: chartViewBox(width, height),
		Bars:    bars,
	}
}

func newTimelineChartView(data TimelineChartData) timelineChartView {
	width := chartDimension(data.Width, 240)
	height := chartDimension(data.Height, 28)
	padding := chartPadding(data.Padding, 3)
	gap := chartPadding(data.Gap, 3)

	segments := positiveTimelineSegments(data.Segments)
	total := 0.0
	for _, segment := range segments {
		total += segment.Value
	}

	views := make([]timelineSegmentView, 0, len(segments))
	if total > 0 {
		innerWidth := width - padding*2 - gap*float64(len(segments)-1)
		if innerWidth < 0 {
			innerWidth = width - padding*2
			gap = 0
		}
		if innerWidth < 0 {
			innerWidth = 0
		}

		x := padding
		for _, segment := range segments {
			segmentWidth := innerWidth * segment.Value / total
			views = append(views, timelineSegmentView{
				X:      webchart.FormatCoord(x),
				Y:      webchart.FormatCoord(padding),
				Width:  webchart.FormatCoord(segmentWidth),
				Height: webchart.FormatCoord(height - padding*2),
				Class:  chartText(segment.Class, "text-accent"),
				Title:  chartPointTitle(segment.Label, segment.Value, data.ValueSuffix),
			})
			x += segmentWidth + gap
		}
	}

	return timelineChartView{
		Title:     chartText(data.Title, "Timeline chart"),
		AriaLabel: chartText(data.AriaLabel, chartText(data.Title, "Timeline chart")),
		Class:     chartClass("block h-7 w-full overflow-hidden rounded-md border border-border bg-muted", data.Class),
		ViewBox:   chartViewBox(width, height),
		Segments:  views,
	}
}

func positiveTimelineSegments(segments []TimelineSegment) []TimelineSegment {
	positive := make([]TimelineSegment, 0, len(segments))
	for _, segment := range segments {
		if segment.Value <= 0 {
			continue
		}
		positive = append(positive, segment)
	}
	return positive
}

func chartDimension(value float64, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func chartPadding(value float64, fallback float64) float64 {
	if value < 0 {
		return 0
	}
	if value == 0 {
		return fallback
	}
	return value
}

func chartViewBox(width float64, height float64) string {
	return "0 0 " + webchart.FormatCoord(width) + " " + webchart.FormatCoord(height)
}

func chartClass(base string, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	return base + " " + extra
}

func chartText(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func chartPointTitle(label string, value float64, suffix string) string {
	valueLabel := chartValueLabel(value, suffix)
	label = strings.TrimSpace(label)
	if label == "" {
		return valueLabel
	}
	return label + ": " + valueLabel
}

func chartValueLabel(value float64, suffix string) string {
	var label string
	rounded := math.Round(value)
	if math.Abs(value-rounded) < 0.000001 {
		label = formatInt(int64(rounded))
	} else {
		label = strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64)
	}

	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return label
	}
	return fmt.Sprintf("%s %s", label, suffix)
}
