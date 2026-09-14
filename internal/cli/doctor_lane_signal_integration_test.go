package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestIgnoredLaneDiagnosticsReachEverySurface(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		source string
		issue  connector.Issue
		want   []string
	}{
		{name: "out of lane label", source: workflowconfig.GitHubStatusSourceProjectV2,
			issue: connector.Issue{ID: "I_1", Identifier: "owner/repo#1", State: "Triage", Labels: []string{"detent:todo"}},
			want:  []string{"ProjectV2 Status", "detent:todo", "set Status to Todo"}},
		{name: "multiple project statuses", source: workflowconfig.GitHubStatusSourceLabel,
			issue: connector.Issue{ID: "I_1", Identifier: "owner/repo#1", State: "Backlog", LaneSignalStatuses: []connector.LaneSignalStatus{
				{Field: "Status", Value: "Todo", ProjectID: "PVT_1", ProjectTitle: "Delivery"},
				{Field: "Status", Value: "Backlog", ProjectID: "PVT_2", ProjectTitle: "Intake"},
			}}, want: []string{"labels with prefix detent:", "Delivery; PVT_1", "Intake; PVT_2", "apply label detent:todo", "apply label detent:backlog"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			state := orchestrator.State{TrackerKind: workflowconfig.TrackerGitHub, TrackerStatusSource: tt.source,
				LaneSignalStates: []string{"Backlog", "Todo"}, LaneSignalCandidates: []connector.Issue{tt.issue}}
			snapshot := state.Snapshot(now)
			snapshot.Project.ID = "detent"
			service := explain.New(explain.Dependencies{Snapshots: laneDiagnosticSnapshot{snapshot: snapshot}})
			explanation, err := service.Explain(t.Context(), explain.Query{ProjectID: "detent", IssueID: "I_1"})
			if err != nil {
				t.Fatal(err)
			}
			explanationJSON, err := json.Marshal(explanation.Reasons)
			if err != nil {
				t.Fatal(err)
			}
			if len(explanation.Reasons) != len(snapshot.LaneSignalWarnings) {
				t.Fatalf("reasons = %#v", explanation.Reasons)
			}

			var board, health bytes.Buffer
			data := templates.DashboardData{Snapshot: snapshot}
			if err := templates.BoardSnapshot(data).Render(t.Context(), &board); err != nil {
				t.Fatal(err)
			}
			if err := templates.HealthPageV2(data).Render(t.Context(), &health); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"status": "needs_attention", "mode": "running", "checks": map[string]string{"hub": "configured", "store": "configured", "registry": "configured", "connector": "configured"}, "lane_signal_warnings": snapshot.LaneSignalWarnings}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			host, portText := splitStalenessTestServerAddress(t, server)
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			check := checkDoctorLaneSignals(t.Context(), BootConfig{Host: host, Port: &port}, "", doctorDeps{httpDo: server.Client().Do}.withDefaults())
			if check.Status != doctorWarn || !strings.HasPrefix(check.Detail, "1 open issue(s)") {
				t.Fatalf("doctor = %#v, want one affected issue", check)
			}
			for surface, content := range map[string]string{"explanation": string(explanationJSON), "board": board.String(), "health": health.String(), "doctor": check.Detail} {
				for _, want := range tt.want {
					if !strings.Contains(content, want) {
						t.Errorf("%s missing %q", surface, want)
					}
				}
			}
			wantCount := "1 signal"
			if len(snapshot.LaneSignalWarnings) == 2 {
				wantCount = "2 signals"
			}
			if !strings.Contains(board.String(), wantCount) {
				t.Errorf("board missing count %q", wantCount)
			}
		})
	}
}

type laneDiagnosticSnapshot struct{ snapshot telemetry.Snapshot }

func (s laneDiagnosticSnapshot) Snapshot(context.Context) (explain.SnapshotObservation, error) {
	return explain.SnapshotObservation{State: explain.SourceLive, Snapshot: s.snapshot}, nil
}
