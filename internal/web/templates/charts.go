package templates

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	webchart "github.com/digitaldrywood/detent/internal/web/chart"
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

type SplitSeriesPoint struct {
	Label  string
	Input  float64
	Output float64
}

type SplitSeriesChartData struct {
	Title       string
	AriaLabel   string
	InputLabel  string
	OutputLabel string
	Points      []SplitSeriesPoint
	ValueSuffix string
	Class       string
	InputClass  string
	OutputClass string
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

type BudgetProjectionPoint struct {
	Label string
	At    time.Time
	Value float64
}

type BudgetProjectionChartData struct {
	Title            string
	AriaLabel        string
	ActualPoints     []BudgetProjectionPoint
	ProjectionPoints []BudgetProjectionPoint
	PeriodStart      time.Time
	PeriodEnd        time.Time
	Cap              float64
	Class            string
	ActualClass      string
	ProjectionClass  string
	CapClass         string
	Width            float64
	Height           float64
	Padding          float64
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

type splitSeriesChartView struct {
	Title          string
	AriaLabel      string
	Class          string
	ViewBox        string
	InputAreaPath  string
	InputLinePath  string
	OutputAreaPath string
	OutputLinePath string
	InputClass     string
	OutputClass    string
	InputPoints    []pointView
	OutputPoints   []pointView
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

type budgetProjectionChartView struct {
	Title              string
	AriaLabel          string
	Class              string
	ViewBox            string
	ActualAreaPath     string
	ActualLinePath     string
	ProjectionLinePath string
	ActualClass        string
	ProjectionClass    string
	CapClass           string
	CapLineX1          string
	CapLineX2          string
	CapLineY           string
	CapTitle           string
	ActualPoints       []pointView
	ProjectionPoints   []pointView
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

func newBudgetProjectionChartView(data BudgetProjectionChartData) budgetProjectionChartView {
	width := chartDimension(data.Width, 300)
	height := chartDimension(data.Height, 140)
	padding := chartPadding(data.Padding, 10)
	baseline := height - padding
	start, end := budgetProjectionPeriod(data)
	maxValue := budgetProjectionMax(data)

	actualScaled := scaleBudgetProjectionPoints(data.ActualPoints, start, end, width, height, padding, maxValue)
	projectionScaled := scaleBudgetProjectionPoints(data.ProjectionPoints, start, end, width, height, padding, maxValue)

	actualPoints := make([]pointView, 0, len(actualScaled))
	for _, point := range actualScaled {
		actualPoints = append(actualPoints, pointView{
			X:     webchart.FormatCoord(point.X),
			Y:     webchart.FormatCoord(point.Y),
			Title: budgetChartPointTitle(point.Label, point.Value),
		})
	}

	projectionPoints := make([]pointView, 0, len(projectionScaled))
	for _, point := range projectionScaled {
		projectionPoints = append(projectionPoints, pointView{
			X:     webchart.FormatCoord(point.X),
			Y:     webchart.FormatCoord(point.Y),
			Title: budgetChartPointTitle(point.Label, point.Value),
		})
	}

	capLineY := ""
	capTitle := ""
	if data.Cap > 0 {
		capLineY = webchart.FormatCoord(scaleBudgetProjectionY(data.Cap, height, padding, maxValue))
		capTitle = "Budget cap: " + formatUSD(data.Cap)
	}

	return budgetProjectionChartView{
		Title:              chartText(data.Title, "Cost burn-down"),
		AriaLabel:          chartText(data.AriaLabel, chartText(data.Title, "Cost burn-down")),
		Class:              chartClass("block h-36 w-full overflow-visible rounded-md border border-border bg-muted", data.Class),
		ViewBox:            chartViewBox(width, height),
		ActualAreaPath:     webchart.SmoothAreaPath(actualScaled, baseline),
		ActualLinePath:     webchart.SmoothLinePath(actualScaled),
		ProjectionLinePath: webchart.LinePath(projectionScaled),
		ActualClass:        chartText(data.ActualClass, "text-accent"),
		ProjectionClass:    chartText(data.ProjectionClass, "text-warning"),
		CapClass:           chartText(data.CapClass, "text-danger"),
		CapLineX1:          webchart.FormatCoord(padding),
		CapLineX2:          webchart.FormatCoord(width - padding),
		CapLineY:           capLineY,
		CapTitle:           capTitle,
		ActualPoints:       actualPoints,
		ProjectionPoints:   projectionPoints,
	}
}

func newSplitSeriesChartView(data SplitSeriesChartData) splitSeriesChartView {
	width := chartDimension(data.Width, 300)
	height := chartDimension(data.Height, 120)
	padding := chartPadding(data.Padding, 8)
	baseline := height - padding
	zero := 0.0
	maxValue := splitSeriesMax(data.Points)

	inputPoints := make([]webchart.Point, 0, len(data.Points))
	outputPoints := make([]webchart.Point, 0, len(data.Points))
	for _, point := range data.Points {
		inputPoints = append(inputPoints, webchart.Point{Label: point.Label, Value: point.Input})
		outputPoints = append(outputPoints, webchart.Point{Label: point.Label, Value: point.Output})
	}

	inputScaled := webchart.ScalePoints(inputPoints, webchart.Options{
		Width:   width,
		Height:  height,
		Padding: padding,
		Min:     &zero,
		Max:     &maxValue,
	})
	outputScaled := webchart.ScalePoints(outputPoints, webchart.Options{
		Width:   width,
		Height:  height,
		Padding: padding,
		Min:     &zero,
		Max:     &maxValue,
	})

	return splitSeriesChartView{
		Title:          chartText(data.Title, "Split series chart"),
		AriaLabel:      chartText(data.AriaLabel, chartText(data.Title, "Split series chart")),
		Class:          chartClass("block h-28 w-full overflow-visible rounded-md border border-border bg-muted", data.Class),
		ViewBox:        chartViewBox(width, height),
		InputAreaPath:  webchart.SmoothAreaPath(inputScaled, baseline),
		InputLinePath:  webchart.SmoothLinePath(inputScaled),
		OutputAreaPath: webchart.SmoothAreaPath(outputScaled, baseline),
		OutputLinePath: webchart.SmoothLinePath(outputScaled),
		InputClass:     chartText(data.InputClass, "text-accent"),
		OutputClass:    chartText(data.OutputClass, "text-success"),
		InputPoints:    splitSeriesPointViews(chartText(data.InputLabel, "Input"), inputScaled, data.ValueSuffix),
		OutputPoints:   splitSeriesPointViews(chartText(data.OutputLabel, "Output"), outputScaled, data.ValueSuffix),
	}
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

func budgetProjectionPeriod(data BudgetProjectionChartData) (time.Time, time.Time) {
	start := data.PeriodStart.UTC()
	end := data.PeriodEnd.UTC()
	if !start.IsZero() && end.After(start) {
		return start, end
	}

	for _, point := range data.ActualPoints {
		at := point.At.UTC()
		if at.IsZero() {
			continue
		}
		if start.IsZero() || at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}
	for _, point := range data.ProjectionPoints {
		at := point.At.UTC()
		if at.IsZero() {
			continue
		}
		if start.IsZero() || at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}
	if start.IsZero() {
		start = time.Unix(0, 0).UTC()
	}
	if !end.After(start) {
		end = start.Add(time.Hour)
	}
	return start, end
}

func budgetProjectionMax(data BudgetProjectionChartData) float64 {
	maxValue := data.Cap
	for _, point := range data.ActualPoints {
		if point.Value > maxValue {
			maxValue = point.Value
		}
	}
	for _, point := range data.ProjectionPoints {
		if point.Value > maxValue {
			maxValue = point.Value
		}
	}
	if maxValue <= 0 {
		return 1
	}
	return maxValue
}

func scaleBudgetProjectionPoints(points []BudgetProjectionPoint, start time.Time, end time.Time, width float64, height float64, padding float64, maxValue float64) []webchart.ScaledPoint {
	if len(points) == 0 {
		return nil
	}

	totalSeconds := end.Sub(start).Seconds()
	if totalSeconds <= 0 {
		totalSeconds = 1
	}
	plotWidth := width - padding*2
	if plotWidth < 0 {
		plotWidth = 0
	}

	scaled := make([]webchart.ScaledPoint, 0, len(points))
	for index, point := range points {
		at := point.At.UTC()
		xRatio := 0.0
		if !at.IsZero() {
			xRatio = at.Sub(start).Seconds() / totalSeconds
		} else if len(points) > 1 {
			xRatio = float64(index) / float64(len(points)-1)
		}
		xRatio = chartClamp(xRatio, 0, 1)
		scaled = append(scaled, webchart.ScaledPoint{
			Label: point.Label,
			Value: point.Value,
			X:     padding + plotWidth*xRatio,
			Y:     scaleBudgetProjectionY(point.Value, height, padding, maxValue),
		})
	}
	return scaled
}

func scaleBudgetProjectionY(value float64, height float64, padding float64, maxValue float64) float64 {
	plotHeight := height - padding*2
	if plotHeight < 0 {
		plotHeight = 0
	}
	ratio := 0.0
	if maxValue > 0 {
		ratio = chartClamp(value/maxValue, 0, 1)
	}
	return padding + (1-ratio)*plotHeight
}

func chartClamp(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func splitSeriesMax(points []SplitSeriesPoint) float64 {
	maxValue := 0.0
	for _, point := range points {
		if point.Input > maxValue {
			maxValue = point.Input
		}
		if point.Output > maxValue {
			maxValue = point.Output
		}
	}
	return maxValue
}

func splitSeriesPointViews(seriesLabel string, points []webchart.ScaledPoint, suffix string) []pointView {
	views := make([]pointView, 0, len(points))
	for _, point := range points {
		views = append(views, pointView{
			X:     webchart.FormatCoord(point.X),
			Y:     webchart.FormatCoord(point.Y),
			Title: chartPointTitle(seriesLabel+" "+point.Label, point.Value, suffix),
		})
	}
	return views
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

func budgetChartPointTitle(label string, value float64) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return formatUSD(value)
	}
	return label + ": " + formatUSD(value)
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
