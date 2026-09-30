package connector

import (
	"reflect"
	"testing"
)

func TestRESTScopeAttribution(t *testing.T) {
	for _, tt := range []struct {
		name    string
		stage   string
		step    string
		outcome string
	}{
		{"refresh success", "tracker_fetch", "fetch_issues", "200"},
		{"refresh deferred", "dispatch", "dispatch_ready_issues", "fanout-deferred"},
		{"outside refresh", "", "", "reserve-refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := &RESTScope{Name: tt.name}
			ctx := WithRESTScope(t.Context(), scope)
			RESTScopeFromContext(ctx).Set(tt.stage, tt.step)
			RESTScopeFromContext(ctx).Record("issue comments", tt.outcome)
			want := []RESTScopeCount{{RESTScopeKey{tt.stage, tt.step, "issue comments", tt.outcome}, 1}}
			if got := scope.Drain().Counts(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Drain().Counts() = %#v, want %#v", got, want)
			}
			if got := scope.Counts(); len(got) != 0 {
				t.Fatalf("Counts() after drain = %#v", got)
			}
		})
	}
}
