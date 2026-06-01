package chart_test

import (
	"math"
	"testing"

	"github.com/digitaldrywood/detent/internal/web/chart"
)

func TestScalePoints(t *testing.T) {
	t.Parallel()

	minValue := -10.0
	maxValue := 10.0

	tests := []struct {
		name   string
		points []chart.Point
		opts   chart.Options
		want   []chart.ScaledPoint
	}{
		{
			name: "scales values into padded SVG coordinates",
			points: []chart.Point{
				{Label: "low", Value: 10},
				{Label: "mid", Value: 20},
				{Label: "high", Value: 30},
			},
			opts: chart.Options{
				Width:   100,
				Height:  50,
				Padding: 5,
			},
			want: []chart.ScaledPoint{
				{Label: "low", Value: 10, X: 5, Y: 45},
				{Label: "mid", Value: 20, X: 50, Y: 25},
				{Label: "high", Value: 30, X: 95, Y: 5},
			},
		},
		{
			name: "centers a single flat point",
			points: []chart.Point{
				{Label: "only", Value: 42},
			},
			opts: chart.Options{
				Width:   100,
				Height:  50,
				Padding: 5,
			},
			want: []chart.ScaledPoint{
				{Label: "only", Value: 42, X: 50, Y: 25},
			},
		},
		{
			name: "honors an explicit min max range",
			points: []chart.Point{
				{Label: "low", Value: -10},
				{Label: "zero", Value: 0},
				{Label: "high", Value: 10},
			},
			opts: chart.Options{
				Width:   100,
				Height:  50,
				Padding: 5,
				Min:     &minValue,
				Max:     &maxValue,
			},
			want: []chart.ScaledPoint{
				{Label: "low", Value: -10, X: 5, Y: 45},
				{Label: "zero", Value: 0, X: 50, Y: 25},
				{Label: "high", Value: 10, X: 95, Y: 5},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chart.ScalePoints(tt.points, tt.opts)
			assertScaledPoints(t, got, tt.want)
		})
	}
}

func TestPathBuilders(t *testing.T) {
	t.Parallel()

	points := []chart.ScaledPoint{
		{X: 5, Y: 45},
		{X: 50, Y: 25},
		{X: 95, Y: 5},
	}

	if got, want := chart.LinePath(points), "M 5 45 L 50 25 L 95 5"; got != want {
		t.Fatalf("LinePath() = %q, want %q", got, want)
	}

	if got, want := chart.AreaPath(points, 45), "M 5 45 L 50 25 L 95 5 L 95 45 L 5 45 Z"; got != want {
		t.Fatalf("AreaPath() = %q, want %q", got, want)
	}

	if got, want := chart.SmoothLinePath(points), "M 5 45 C 12.5 41.67 35 31.67 50 25 C 65 18.33 87.5 8.33 95 5"; got != want {
		t.Fatalf("SmoothLinePath() = %q, want %q", got, want)
	}

	if got, want := chart.SmoothAreaPath(points, 45), "M 5 45 C 12.5 41.67 35 31.67 50 25 C 65 18.33 87.5 8.33 95 5 L 95 45 L 5 45 Z"; got != want {
		t.Fatalf("SmoothAreaPath() = %q, want %q", got, want)
	}

	if got := chart.LinePath(nil); got != "" {
		t.Fatalf("LinePath(nil) = %q, want empty string", got)
	}

	if got := chart.AreaPath(nil, 45); got != "" {
		t.Fatalf("AreaPath(nil) = %q, want empty string", got)
	}

	if got := chart.SmoothLinePath(nil); got != "" {
		t.Fatalf("SmoothLinePath(nil) = %q, want empty string", got)
	}

	if got := chart.SmoothAreaPath(nil, 45); got != "" {
		t.Fatalf("SmoothAreaPath(nil) = %q, want empty string", got)
	}
}

func TestScaleBars(t *testing.T) {
	t.Parallel()

	got := chart.ScaleBars([]chart.Point{
		{Label: "zero", Value: 0},
		{Label: "half", Value: 50},
		{Label: "full", Value: 100},
	}, chart.Options{
		Width:   120,
		Height:  60,
		Padding: 10,
		Gap:     5,
	})

	want := []chart.Bar{
		{Label: "zero", Value: 0, X: 10, Y: 50, Width: 30, Height: 0},
		{Label: "half", Value: 50, X: 45, Y: 30, Width: 30, Height: 20},
		{Label: "full", Value: 100, X: 80, Y: 10, Width: 30, Height: 40},
	}

	if len(got) != len(want) {
		t.Fatalf("ScaleBars() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Label != want[i].Label {
			t.Fatalf("bar %d label = %q, want %q", i, got[i].Label, want[i].Label)
		}
		if got[i].Value != want[i].Value {
			t.Fatalf("bar %d value = %f, want %f", i, got[i].Value, want[i].Value)
		}
		assertFloat(t, got[i].X, want[i].X)
		assertFloat(t, got[i].Y, want[i].Y)
		assertFloat(t, got[i].Width, want[i].Width)
		assertFloat(t, got[i].Height, want[i].Height)
	}
}

func assertScaledPoints(t *testing.T, got []chart.ScaledPoint, want []chart.ScaledPoint) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("ScalePoints() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Label != want[i].Label {
			t.Fatalf("point %d label = %q, want %q", i, got[i].Label, want[i].Label)
		}
		if got[i].Value != want[i].Value {
			t.Fatalf("point %d value = %f, want %f", i, got[i].Value, want[i].Value)
		}
		assertFloat(t, got[i].X, want[i].X)
		assertFloat(t, got[i].Y, want[i].Y)
	}
}

func assertFloat(t *testing.T, got float64, want float64) {
	t.Helper()

	if math.Abs(got-want) > 0.001 {
		t.Fatalf("value = %f, want %f", got, want)
	}
}
