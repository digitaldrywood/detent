package operations

import (
	"testing"
	"time"
)

func TestWithQuestionAges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   *time.Time
		want *int64
	}{
		{"known", new(now.Add(-6 * time.Hour)), new(int64(21600))},
		{"legacy unknown", nil, nil},
		{"clock skew", new(now.Add(time.Hour)), new(int64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := []Decision{{Question: "Choose target?", AskedAt: tc.at}}
			got := WithQuestionAges(source, now)
			if len(got) != 1 || got[0].Question != source[0].Question || (got[0].AgeSeconds == nil) != (tc.want == nil) || tc.want != nil && *got[0].AgeSeconds != *tc.want {
				t.Fatalf("ages = %+v", got)
			}
			if source[0].AgeSeconds != nil {
				t.Fatal("mutated cached decision")
			}
			got[0].Question = "Changed"
			if source[0].Question != "Choose target?" {
				t.Fatal("aliased cached decisions")
			}
		})
	}
}
