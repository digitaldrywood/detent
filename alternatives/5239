package templates

import "testing"

func TestOperationsNumber(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value *float64
		want  string
	}{{"unknown", nil, "Unavailable"}, {"zero", new(float64), "0.00"}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationsNumber(tc.value); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
