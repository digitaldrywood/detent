package chart

import (
	"math"
	"strconv"
	"strings"
)

type Point struct {
	Label string
	Value float64
}

type ScaledPoint struct {
	Label string
	Value float64
	X     float64
	Y     float64
}

type Bar struct {
	Label  string
	Value  float64
	X      float64
	Y      float64
	Width  float64
	Height float64
}

type Options struct {
	Width   float64
	Height  float64
	Padding float64
	Gap     float64
	Min     *float64
	Max     *float64
}

func ScalePoints(points []Point, opts Options) []ScaledPoint {
	if len(points) == 0 {
		return nil
	}

	opts = normalizeOptions(opts)
	minValue, maxValue := valueRange(points, opts)
	plotWidth := innerSize(opts.Width, opts.Padding)
	plotHeight := innerSize(opts.Height, opts.Padding)

	scaled := make([]ScaledPoint, 0, len(points))
	for i, point := range points {
		x := opts.Width / 2
		if len(points) > 1 {
			x = opts.Padding + float64(i)*plotWidth/float64(len(points)-1)
		}

		y := opts.Padding + plotHeight/2
		if maxValue > minValue {
			ratio := clamp((point.Value-minValue)/(maxValue-minValue), 0, 1)
			y = opts.Padding + (1-ratio)*plotHeight
		}

		scaled = append(scaled, ScaledPoint{
			Label: point.Label,
			Value: point.Value,
			X:     roundCoord(x),
			Y:     roundCoord(y),
		})
	}
	return scaled
}

func ScaleBars(points []Point, opts Options) []Bar {
	if len(points) == 0 {
		return nil
	}

	opts = normalizeOptions(opts)
	minValue := 0.0
	if opts.Min != nil {
		minValue = *opts.Min
	}
	maxValue := maxPointValue(points)
	if opts.Max != nil {
		maxValue = *opts.Max
	}
	if maxValue < minValue {
		maxValue = minValue
	}

	plotWidth := innerSize(opts.Width, opts.Padding)
	plotHeight := innerSize(opts.Height, opts.Padding)
	gap := opts.Gap
	if gap < 0 {
		gap = 0
	}
	totalGap := gap * float64(len(points)-1)
	if totalGap > plotWidth {
		gap = 0
		totalGap = 0
	}
	barWidth := (plotWidth - totalGap) / float64(len(points))
	baseline := opts.Height - opts.Padding
	span := maxValue - minValue

	bars := make([]Bar, 0, len(points))
	for i, point := range points {
		value := point.Value
		if value < minValue {
			value = minValue
		}
		if value > maxValue {
			value = maxValue
		}

		height := 0.0
		if span > 0 {
			height = clamp((value-minValue)/span, 0, 1) * plotHeight
		}
		x := opts.Padding + float64(i)*(barWidth+gap)
		y := baseline - height
		bars = append(bars, Bar{
			Label:  point.Label,
			Value:  point.Value,
			X:      roundCoord(x),
			Y:      roundCoord(y),
			Width:  roundCoord(barWidth),
			Height: roundCoord(height),
		})
	}
	return bars
}

func LinePath(points []ScaledPoint) string {
	if len(points) == 0 {
		return ""
	}

	var path strings.Builder
	path.WriteString("M ")
	path.WriteString(FormatCoord(points[0].X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(points[0].Y))
	for _, point := range points[1:] {
		path.WriteString(" L ")
		path.WriteString(FormatCoord(point.X))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(point.Y))
	}
	return path.String()
}

func AreaPath(points []ScaledPoint, baselineY float64) string {
	if len(points) == 0 {
		return ""
	}

	first := points[0]
	last := points[len(points)-1]
	var path strings.Builder
	path.WriteString(LinePath(points))
	path.WriteString(" L ")
	path.WriteString(FormatCoord(last.X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(baselineY))
	path.WriteString(" L ")
	path.WriteString(FormatCoord(first.X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(baselineY))
	path.WriteString(" Z")
	return path.String()
}

func SmoothLinePath(points []ScaledPoint) string {
	if len(points) == 0 {
		return ""
	}
	if len(points) < 3 {
		return LinePath(points)
	}

	var path strings.Builder
	path.WriteString("M ")
	path.WriteString(FormatCoord(points[0].X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(points[0].Y))
	for i := 0; i < len(points)-1; i++ {
		p0 := boundedPoint(points, i-1)
		p1 := boundedPoint(points, i)
		p2 := boundedPoint(points, i+1)
		p3 := boundedPoint(points, i+2)

		c1x := p1.X + (p2.X-p0.X)/6
		c1y := p1.Y + (p2.Y-p0.Y)/6
		c2x := p2.X - (p3.X-p1.X)/6
		c2y := p2.Y - (p3.Y-p1.Y)/6

		path.WriteString(" C ")
		path.WriteString(FormatCoord(c1x))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(c1y))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(c2x))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(c2y))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(p2.X))
		path.WriteByte(' ')
		path.WriteString(FormatCoord(p2.Y))
	}
	return path.String()
}

func SmoothAreaPath(points []ScaledPoint, baselineY float64) string {
	if len(points) == 0 {
		return ""
	}

	first := points[0]
	last := points[len(points)-1]
	var path strings.Builder
	path.WriteString(SmoothLinePath(points))
	path.WriteString(" L ")
	path.WriteString(FormatCoord(last.X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(baselineY))
	path.WriteString(" L ")
	path.WriteString(FormatCoord(first.X))
	path.WriteByte(' ')
	path.WriteString(FormatCoord(baselineY))
	path.WriteString(" Z")
	return path.String()
}

func FormatCoord(value float64) string {
	value = roundCoord(value)
	if value == 0 {
		return "0"
	}
	formatted := strconv.FormatFloat(value, 'f', 2, 64)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimRight(formatted, ".")
	return formatted
}

func normalizeOptions(opts Options) Options {
	if opts.Width <= 0 {
		opts.Width = 240
	}
	if opts.Height <= 0 {
		opts.Height = 80
	}
	if opts.Padding < 0 {
		opts.Padding = 0
	}
	maxPadding := math.Min(opts.Width, opts.Height) / 2
	if opts.Padding > maxPadding {
		opts.Padding = maxPadding
	}
	return opts
}

func valueRange(points []Point, opts Options) (float64, float64) {
	minValue := points[0].Value
	maxValue := points[0].Value
	for _, point := range points[1:] {
		if point.Value < minValue {
			minValue = point.Value
		}
		if point.Value > maxValue {
			maxValue = point.Value
		}
	}
	if opts.Min != nil {
		minValue = *opts.Min
	}
	if opts.Max != nil {
		maxValue = *opts.Max
	}
	if maxValue < minValue {
		minValue, maxValue = maxValue, minValue
	}
	return minValue, maxValue
}

func maxPointValue(points []Point) float64 {
	maxValue := points[0].Value
	for _, point := range points[1:] {
		if point.Value > maxValue {
			maxValue = point.Value
		}
	}
	return maxValue
}

func innerSize(size float64, padding float64) float64 {
	inner := size - padding*2
	if inner < 0 {
		return 0
	}
	return inner
}

func boundedPoint(points []ScaledPoint, index int) ScaledPoint {
	if index < 0 {
		return points[0]
	}
	if index >= len(points) {
		return points[len(points)-1]
	}
	return points[index]
}

func roundCoord(value float64) float64 {
	return math.Round(value*100) / 100
}

func clamp(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
