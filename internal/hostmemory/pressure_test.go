package hostmemory

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantAvg60 float64
		wantTotal uint64
		wantError string
	}{
		{
			name:      "linux pressure sample",
			input:     "some avg10=1.25 avg60=10.50 avg300=3.00 total=1234\nfull avg10=0.00 avg60=0.25 avg300=0.10 total=42\n",
			wantAvg60: 10.5,
			wantTotal: 42,
		},
		{name: "missing some", input: "full avg10=0 avg60=0 avg300=0 total=0\n", wantError: "some row is missing"},
		{name: "malformed average", input: "some avg10=0 avg60=nope avg300=0 total=0\n", wantError: "avg60"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.input)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Parse() error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Some.Avg60 != test.wantAvg60 || got.Full.Total != test.wantTotal {
				t.Fatalf("Parse() = %#v, want some.avg60=%v full.total=%d", got, test.wantAvg60, test.wantTotal)
			}
		})
	}
}
