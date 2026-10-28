package templates

import "testing"

func TestOperationsShellAndMetrics(t *testing.T) {
	t.Parallel()
	if got := eventsPath(DashboardShellData{ActiveNav: "operations"}); got != "/events?nav=operations" {
		t.Fatalf("events path: %s", got)
	}
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
