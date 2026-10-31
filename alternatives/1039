package hubclient

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestActivityReadContract(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request operatortool.ActivityRequest
		query   string
	}{
		{name: "organization"},
		{name: "filtered", request: operatortool.ActivityRequest{ProjectID: "prj_fixture", RunnerID: "runner/fixture", Limit: 2, From: "2026-10-01T00:00:00Z", To: "2026-10-02T00:00:00Z"}, query: "from=2026-10-01T00%3A00%3A00Z&limit=2&project_id=prj_fixture&runner_id=runner%2Ffixture&to=2026-10-02T00%3A00%3A00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/organizations/org_fixture/activity" || r.URL.RawQuery != tt.query {
					t.Fatalf("request=%s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"organization_id":"org_fixture","observed_at":"2026-10-02T00:00:00Z","running":[{"work_item_id":"wi_fixture","number":1,"stage":"code","stage_started_at":"2026-10-01T00:00:00Z","stage_elapsed_seconds":60,"session_id":"session","change_id":"change"}],"finished":[{"outcome":"failed","finished_at":"2026-10-01T00:01:00Z","stage_duration_seconds":60}],"typical_durations":[{"project_id":"prj_fixture","stage":"code","count":2,"p50_seconds":60,"p90_seconds":90,"partial":true}],"partial":true}`))}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Activity(t.Context(), "org_fixture", tt.request)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Running) != 1 || got.Running[0].Number != 1 || got.Running[0].StageStartedAt == nil || !got.Running[0].StageStartedAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || got.Running[0].SessionID != "session" || got.Running[0].ChangeID != "change" || got.Running[0].StageElapsedSeconds == nil || *got.Running[0].StageElapsedSeconds != 60 || len(got.Finished) != 1 || got.Finished[0].Outcome != "failed" || got.Finished[0].StageDurationSeconds == nil || *got.Finished[0].StageDurationSeconds != 60 || len(got.TypicalDurations) != 1 || got.TypicalDurations[0].P90Seconds != 90 || !got.Partial {
				t.Fatalf("activity=%+v", got)
			}
		})
	}
}
