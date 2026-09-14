package orchestrator

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestConfigFromWorkflowIncludesLaneSignalConfiguration(t *testing.T) {
	t.Parallel()
	workflow := workflowconfig.Config{}
	workflow.Tracker.Kind = workflowconfig.TrackerGitHub
	workflow.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceProjectV2
	workflow.Tracker.StatusField = "Workflow Status"
	workflow.Tracker.StatusLabelPrefix = "workflow:"
	workflow.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	workflow.Tracker.ObservedStates = []string{"Backlog", "Todo"}
	workflow.Tracker.TerminalStates = []string{"Done"}
	workflow.Tracker.StateMap = workflowconfig.MapValue(map[string]any{"Todo": "Ready"})

	got := ConfigFromWorkflow(workflow)
	if got.TrackerKind != workflowconfig.TrackerGitHub || got.TrackerStatusSource != workflowconfig.GitHubStatusSourceProjectV2 || got.TrackerStatusField != "Workflow Status" || got.TrackerStatusLabelPrefix != "workflow:" {
		t.Fatalf("tracker lane signal config = %#v", got)
	}
	if want := map[string]string{"Todo": "Ready"}; !reflect.DeepEqual(got.TrackerStateMap, want) {
		t.Fatalf("TrackerStateMap = %#v, want %#v", got.TrackerStateMap, want)
	}
	if want := []string{"Todo", "In Progress", "Backlog", "Done"}; !reflect.DeepEqual(got.LaneSignalStates, want) {
		t.Fatalf("LaneSignalStates = %#v, want %#v", got.LaneSignalStates, want)
	}
}

func TestLaneSignalWarningsReportSignalsIgnoredByConfiguredSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     string
		issue      connector.Issue
		wantKind   string
		wantSignal string
		wantSource string
		wantAction string
	}{
		{
			name:       "lane label on ProjectV2 status source",
			source:     workflowconfig.GitHubStatusSourceProjectV2,
			issue:      connector.Issue{ID: "issue-label", Identifier: "owner/repo#1", Labels: []string{"bug", "detent:todo"}},
			wantKind:   "label",
			wantSignal: "detent:todo",
			wantSource: "ProjectV2 Status",
			wantAction: "set Status to Todo",
		},
		{
			name:       "lane status on label source",
			source:     workflowconfig.GitHubStatusSourceLabel,
			issue:      connector.Issue{ID: "issue-status", Identifier: "owner/repo#2", Fields: map[string]string{"Status": "Todo"}},
			wantKind:   "status",
			wantSignal: "Status Todo",
			wantSource: "labels with prefix detent:",
			wantAction: "apply label detent:todo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := State{
				TrackerKind:              workflowconfig.TrackerGitHub,
				TrackerStatusSource:      tt.source,
				TrackerStatusField:       "Status",
				TrackerStatusLabelPrefix: "detent:",
				LaneSignalStates:         []string{"Backlog", "Todo", "In Progress", "Done"},
			}

			warnings := laneSignalWarnings(state, []connector.Issue{tt.issue})
			if len(warnings) != 1 {
				t.Fatalf("laneSignalWarnings() = %#v, want one warning", warnings)
			}
			warning := warnings[0]
			if warning.ReasonCode != telemetry.LaneSignalIgnoredReasonCode || warning.SignalKind != tt.wantKind || warning.Signal != tt.wantSignal {
				t.Fatalf("warning identity = %#v", warning)
			}
			if warning.ConfiguredSource != tt.wantSource || warning.Lane != "Todo" || warning.Action != tt.wantAction {
				t.Fatalf("warning source/lane/action = %#v", warning)
			}
			for _, want := range []string{tt.wantSource, tt.wantSignal + " has no effect", tt.wantAction} {
				if !strings.Contains(warning.Reason, want) {
					t.Fatalf("reason %q missing %q", warning.Reason, want)
				}
			}
		})
	}
}

func TestLaneSignalWarningsIgnoreUnrelatedAndClosedSignals(t *testing.T) {
	t.Parallel()
	state := State{
		TrackerKind:              workflowconfig.TrackerGitHub,
		TrackerStatusSource:      workflowconfig.GitHubStatusSourceProjectV2,
		TrackerStatusField:       "Status",
		TrackerStatusLabelPrefix: "detent:",
		LaneSignalStates:         []string{"Todo"},
	}
	issues := []connector.Issue{
		{ID: "ordinary", Labels: []string{"bug"}},
		{ID: "unknown", Labels: []string{"detent:not-a-lane"}},
		{ID: "closed", Closed: true, Labels: []string{"detent:todo"}},
	}
	if warnings := laneSignalWarnings(state, issues); len(warnings) != 0 {
		t.Fatalf("laneSignalWarnings() = %#v, want no warnings", warnings)
	}
}

func TestLaneSignalDiagnosticsSurviveRefreshAndSnapshot(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		source      string
		issue       connector.Issue
		wantSignals []string
	}{
		{name: "unconfigured ProjectV2 lane", source: workflowconfig.GitHubStatusSourceProjectV2,
			issue:       connector.Issue{ID: "I_1", Identifier: "owner/repo#1", State: "Triage", Labels: []string{"detent:todo"}},
			wantSignals: []string{"detent:todo"}},
		{name: "distinct project statuses", source: workflowconfig.GitHubStatusSourceLabel,
			issue: connector.Issue{ID: "I_1", Identifier: "owner/repo#1", State: "Backlog", LaneSignalStatuses: []connector.LaneSignalStatus{
				{Field: "Status", Value: "Todo", ProjectID: "PVT_1", ProjectTitle: "Delivery"},
				{Field: "Status", Value: "Backlog", ProjectID: "PVT_2", ProjectTitle: "Intake"},
			}}, wantSignals: []string{"Status Backlog (project Intake; PVT_2)", "Status Todo (project Delivery; PVT_1)"}},
		{name: "same status in distinct projects", source: workflowconfig.GitHubStatusSourceLabel,
			issue: connector.Issue{ID: "I_1", Identifier: "owner/repo#1", LaneSignalStatuses: []connector.LaneSignalStatus{
				{Field: "Status", Value: "Todo", ProjectID: "PVT_1"},
				{Field: "Status", Value: "Todo", ProjectID: "PVT_2"},
			}}, wantSignals: []string{"Status Todo (project PVT_1)", "Status Todo (project PVT_2)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			cfg := normalizeConfig(Config{TrackerKind: workflowconfig.TrackerGitHub, TrackerStatusSource: tt.source, LaneSignalStates: []string{"Backlog", "Todo"}})
			state := newState(cfg)
			candidates := []connector.Issue{tt.issue}
			if tt.source == workflowconfig.GitHubStatusSourceProjectV2 {
				o := &Orchestrator{cfg: cfg}
				fetched, ok := o.fetchCombinedTickIssues(t.Context(), &state, now, nil, laneSignalRefreshFixture{issues: candidates})
				if !ok || len(fetched.candidates) != 0 || len(fetched.status) != 0 {
					t.Fatalf("diagnostic entered scheduling: %#v, %v", fetched, ok)
				}
			} else {
				state.StatusDrift.LaneSignalCandidates = candidates
			}
			snapshot := state.clone().Snapshot(now)
			var signals []string
			for _, warning := range snapshot.LaneSignalWarnings {
				signals = append(signals, warning.Signal)
				if warning.IssueID != "I_1" || warning.ReasonCode != telemetry.LaneSignalIgnoredReasonCode || warning.ConfiguredSource == "" || warning.Action == "" {
					t.Fatalf("incomplete warning: %#v", warning)
				}
			}
			if !reflect.DeepEqual(signals, tt.wantSignals) {
				t.Fatalf("signals = %v, want %v", signals, tt.wantSignals)
			}
			if len(snapshot.BoardIssues) != 0 || len(snapshot.Pipeline) != 0 {
				t.Fatal("diagnostics added board or pipeline issues")
			}
			state.Authorization = selector.Selector{AuthorIn: []string{"allowed"}}
			if got := state.Snapshot(now).LaneSignalWarnings; len(got) != 0 {
				t.Fatalf("unauthorized diagnostics: %#v", got)
			}
		})
	}
}

type laneSignalRefreshFixture struct{ issues []connector.Issue }

func (laneSignalRefreshFixture) CombinedRefreshEnabled() bool { return true }
func (f laneSignalRefreshFixture) FetchRefreshIssues(context.Context, []string, []string, connector.IssueFilterHint) connector.RefreshIssueResult {
	return connector.RefreshIssueResult{LaneSignalCandidates: f.issues}
}

func TestHubSchedulingRetainsOutOfLaneDiagnostics(t *testing.T) {
	t.Parallel()
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{TrackerKind: workflowconfig.TrackerGitHub, TrackerStatusSource: workflowconfig.GitHubStatusSourceProjectV2,
				LaneSignalStates: []string{"Todo", "Backlog"}, SchedulingRepository: "acme/widgets"})
			scheduling := &hubSchedulingSource{issue: connector.Issue{ID: "hub", State: "Todo"}}
			if !active {
				scheduling.issue = connector.Issue{}
			}
			tracker := &laneSignalHubConnector{diagnostic: connector.Issue{ID: "triage", State: "Triage", Labels: []string{"detent:todo"}}}
			o, err := New(cfg, Dependencies{Connector: tracker, Scheduling: scheduling})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(cfg)
			fetched, ok := o.fetchTickIssues(t.Context(), &state, time.Now(), githubBudgetReserveDecision{})
			if !ok {
				t.Fatal("fetch failed")
			}
			for _, issue := range fetched.candidates {
				if issue.ID == "triage" {
					t.Fatal("diagnostic became a Hub candidate")
				}
			}
			if scheduling.fetches != 1 || tracker.candidateReads.Load() != 0 {
				t.Fatal("Hub candidate ownership changed")
			}
			if got := state.Snapshot(time.Now()).LaneSignalWarnings; len(got) != 1 || got[0].IssueID != "triage" {
				t.Fatalf("warnings = %#v, want out-of-lane diagnostic", got)
			}
		})
	}
}

type laneSignalHubConnector struct {
	hubSchedulingConnector
	diagnostic connector.Issue
}

func (c *laneSignalHubConnector) FetchRefreshIssues(_ context.Context, candidates, _ []string, _ connector.IssueFilterHint) connector.RefreshIssueResult {
	if len(candidates) > 0 {
		c.candidateReads.Add(1)
	}
	return connector.RefreshIssueResult{LaneSignalCandidates: []connector.Issue{c.diagnostic}}
}
