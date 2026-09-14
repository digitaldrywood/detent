package orchestrator

import (
	"reflect"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
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
