package operatortool

import (
	"testing"
	"time"
)

func TestDecodeActivity(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, raw string
		valid     bool
	}{
		{"default", `{}`, true},
		{"filters and dates", `{"project_id":"prj_fixture","runner_id":"runner_fixture","limit":2,"from":"2026-09-01","to":"2026-10-01"}`, true},
		{"short window", `{"from":"2026-09-30T23:59:00Z"}`, true},
		{"unknown field", `{"offset":1}`, false},
		{"zero limit", `{"limit":0}`, false},
		{"negative limit", `{"limit":-1}`, false},
		{"limit overflow", `{"limit":201}`, false},
		{"invalid date", `{"from":"yesterday"}`, false},
		{"future", `{"to":"2026-10-02"}`, false},
		{"empty window", `{"from":"2026-10-01"}`, false},
		{"inverted", `{"from":"2026-09-30","to":"2026-09-29"}`, false},
		{"too long", `{"from":"2026-01-01"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, w, err := DecodeActivity([]byte(tt.raw), now)
			if (err == nil) != tt.valid {
				t.Fatalf("request=%+v window=%+v err=%v", r, w, err)
			}
			if tt.name == "default" && (r.Limit != DefaultItemLimit || !w.From.Equal(now.Add(-7*24*time.Hour)) || !w.To.Equal(now)) {
				t.Fatalf("default request=%+v window=%+v", r, w)
			}
			if tt.name == "filters and dates" && (r.ProjectID != "prj_fixture" || r.RunnerID != "runner_fixture" || r.Limit != 2 || !w.From.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))) {
				t.Fatalf("filters=%+v window=%+v", r, w)
			}
		})
	}
}
