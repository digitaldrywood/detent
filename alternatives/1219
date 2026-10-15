package connector

import (
	"reflect"
	"testing"
	"time"
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
			attribution := RESTScopeFromContext(ctx).Attribution("issue comments", "")
			RESTScopeFromContext(ctx).Set("later", "later")
			attribution.Record(tt.outcome)
			attribution.Observe("http_transport", tt.outcome, 2*time.Millisecond)
			attribution.Observe("http_transport", tt.outcome, 3*time.Millisecond)
			attribution.Observe("http_transport", tt.outcome, -1)
			wantTiming := []GitHubTiming{{GitHubTimingKey: GitHubTimingKey{RESTScopeKey: RESTScopeKey{tt.stage, tt.step, "issue comments", tt.outcome}, Boundary: "http_transport"}, AttemptCount: 3, TimedCount: 2, ElapsedSumNS: int64(5 * time.Millisecond), ElapsedMaxNS: int64(3 * time.Millisecond)}}
			want := []RESTScopeCount{{RESTScopeKey{tt.stage, tt.step, "issue comments", tt.outcome}, 1}}
			drained := scope.Drain()
			gotTiming := drained.Timings()
			if len(gotTiming) != 1 || gotTiming[0].FirstObservedAt.IsZero() || gotTiming[0].LastObservedAt.Before(gotTiming[0].FirstObservedAt) {
				t.Fatalf("timing observation interval = %#v", gotTiming)
			}
			wantTiming[0].FirstObservedAt = gotTiming[0].FirstObservedAt
			wantTiming[0].LastObservedAt = gotTiming[0].LastObservedAt
			if !reflect.DeepEqual(gotTiming, wantTiming) {
				t.Fatalf("timings = %#v, want %#v", gotTiming, wantTiming)
			}
			if got := drained.Counts(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Drain().Counts() = %#v, want %#v", got, want)
			}
			if got := scope.Timings(); len(got) != 0 {
				t.Fatalf("timings after drain = %#v", got)
			}
			if scope.Drain() != nil {
				t.Fatal("empty drain retained a cohort")
			}
			attribution.Observe("token_resolution_inclusive", "200", time.Millisecond)
			if got := scope.Drain(); got == nil || len(got.Counts()) != 0 || len(got.Timings()) != 1 {
				t.Fatalf("timing-only drain = %#v", got)
			}
			if got := scope.Counts(); len(got) != 0 {
				t.Fatalf("Counts() after drain = %#v", got)
			}
		})
	}
}
