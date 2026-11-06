package usagecost

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestAllocateMonths(t *testing.T) {
	boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		amount   int64
		from, to time.Time
		want     []int64
	}{
		{"nanosecond remainder", 101, boundary.Add(-time.Nanosecond), boundary.Add(time.Nanosecond), []int64{50, 51}},
		{"negative correction", -101, boundary.Add(-time.Nanosecond), boundary.Add(time.Nanosecond), []int64{-50, -51}},
		{"exclusive upper boundary", 101, boundary.Add(-time.Nanosecond), boundary, []int64{101}},
		{"maximum amount", math.MaxInt64, boundary.Add(-time.Nanosecond), boundary.Add(time.Nanosecond), []int64{4611686018427387903, 4611686018427387904}},
		{"minimum amount", math.MinInt64, boundary.Add(-time.Nanosecond), boundary.Add(time.Nanosecond), []int64{-4611686018427387904, -4611686018427387904}},
		{"UTC offset", 7, boundary.In(time.FixedZone("example", -21600)), boundary.Add(time.Hour), []int64{7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AllocateMonths(tt.amount, tt.from, tt.to)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("months = %d, want %d", len(got), len(tt.want))
			}
			for i, want := range tt.want {
				if got[i].Micros != want {
					t.Fatalf("month %d = %d, want %d", i, got[i].Micros, want)
				}
				if got[i].Month.Location() != time.UTC || got[i].Month.Day() != 1 || got[i].Month.Hour() != 0 {
					t.Fatalf("invalid month: %v", got[i].Month)
				}
			}
		})
	}
	for _, to := range []time.Time{boundary, boundary.Add(-time.Nanosecond), boundary.Add(366*24*time.Hour + time.Nanosecond)} {
		if _, err := AllocateMonths(1, boundary, to); !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("invalid interval accepted: %v", err)
		}
	}
}
