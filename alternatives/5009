package shadow

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestRunComputesGoObservationAndReportsDiffs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	priority := 1
	input := Input{
		Date: "2026-05-31",
		Now:  now.Format(time.RFC3339),
		Scenario: &Scenario{
			Config: DispatchConfig{
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
			},
			Candidates: []Issue{
				{
					ID:               "issue-go",
					Identifier:       "digitaldrywood/detent#46",
					Title:            "Go shadow issue",
					State:            "Todo",
					Priority:         &priority,
					CreatedAt:        now.Add(-time.Hour).Format(time.RFC3339),
					AssignedToWorker: new(true),
				},
			},
			Tokens: TokenAccounting{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
				Sessions:     1,
				ByModel: []ModelTokenAccounting{
					{Model: "gpt-5", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Sessions: 1},
				},
			},
		},
		Elixir: Observation{
			Dispatch: DispatchObservation{
				DispatchOrder: []string{"issue-elixir"},
				Claimed:       []string{"issue-elixir"},
			},
			Tokens: TokenAccounting{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  16,
				Sessions:     1,
				ByModel: []ModelTokenAccounting{
					{Model: "gpt-5", InputTokens: 10, OutputTokens: 5, TotalTokens: 16, Sessions: 1},
				},
			},
		},
	}

	report, err := Run(input)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !report.Diff.HasDifferences {
		t.Fatal("HasDifferences = false, want true")
	}
	if !slices.Equal(report.Go.Dispatch.DispatchOrder, []string{"issue-go"}) {
		t.Fatalf("Go dispatch order = %#v, want issue-go", report.Go.Dispatch.DispatchOrder)
	}
	if !diffFieldsContain(report.Diff.Dispatch, "dispatch_order") {
		t.Fatalf("Dispatch diffs = %#v, want dispatch_order", report.Diff.Dispatch)
	}
	if !tokenDiffsContain(report.Diff.Tokens, "total_tokens") {
		t.Fatalf("Token diffs = %#v, want total_tokens", report.Diff.Tokens)
	}
}

func TestRunNormalizesTokenModelOrder(t *testing.T) {
	t.Parallel()

	input := Input{
		Date: "2026-05-31",
		Now:  "2026-05-31T12:00:00Z",
		Go: &Observation{
			Dispatch: DispatchObservation{DispatchOrder: []string{"issue-1"}},
			Tokens: TokenAccounting{
				InputTokens:  12,
				OutputTokens: 8,
				TotalTokens:  20,
				Sessions:     2,
				ByModel: []ModelTokenAccounting{
					{Model: "gpt-b", InputTokens: 2, OutputTokens: 3, TotalTokens: 5, Sessions: 1},
					{Model: "gpt-a", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Sessions: 1},
				},
			},
		},
		Elixir: Observation{
			Dispatch: DispatchObservation{DispatchOrder: []string{"issue-1"}},
			Tokens: TokenAccounting{
				InputTokens:  12,
				OutputTokens: 8,
				TotalTokens:  20,
				Sessions:     2,
				ByModel: []ModelTokenAccounting{
					{Model: "gpt-a", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Sessions: 1},
					{Model: "gpt-b", InputTokens: 2, OutputTokens: 3, TotalTokens: 5, Sessions: 1},
				},
			},
		},
	}

	report, err := Run(input)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if report.Diff.HasDifferences {
		t.Fatalf("Diff = %#v, want no differences", report.Diff)
	}
}

func TestRunDiffsDispatchDetailsWhenBothReportsIncludeThem(t *testing.T) {
	t.Parallel()

	input := Input{
		Date: "2026-05-31",
		Go: &Observation{
			Dispatch: DispatchObservation{
				DispatchOrder: []string{"issue-1"},
				Dispatches: []DispatchDetail{
					{IssueID: "issue-1", Attempt: 2, Retry: true, WorkerHost: "worker-a"},
				},
			},
		},
		Elixir: Observation{
			Dispatch: DispatchObservation{
				DispatchOrder: []string{"issue-1"},
				Dispatches: []DispatchDetail{
					{IssueID: "issue-1", Attempt: 1, Retry: true, WorkerHost: "worker-b"},
				},
			},
		},
	}

	report, err := Run(input)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !report.Diff.HasDifferences {
		t.Fatal("HasDifferences = false, want true")
	}
	if !diffFieldsContain(report.Diff.Dispatch, "dispatch_details") {
		t.Fatalf("Dispatch diffs = %#v, want dispatch_details", report.Diff.Dispatch)
	}
}

func TestScenarioJSONOptionalStructs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scenario    Scenario
		wantPresent []string
		wantMissing []string
	}{
		{
			name: "zero optional structs omitted",
			scenario: Scenario{
				Config: DispatchConfig{MaxConcurrentAgents: 1},
			},
			wantMissing: []string{"initial_state", "tokens"},
		},
		{
			name: "populated optional structs emitted",
			scenario: Scenario{
				Config: DispatchConfig{MaxConcurrentAgents: 1},
				InitialState: InitialState{
					Running: []Running{{Issue: Issue{ID: "issue-running"}}},
				},
				Tokens: TokenAccounting{TotalTokens: 12},
			},
			wantPresent: []string{"initial_state", "tokens"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.scenario)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}

			for _, key := range tt.wantPresent {
				if _, ok := got[key]; !ok {
					t.Fatalf("scenario JSON missing %q: %s", key, string(data))
				}
			}
			for _, key := range tt.wantMissing {
				if _, ok := got[key]; ok {
					t.Fatalf("scenario JSON includes %q: %s", key, string(data))
				}
			}
		})
	}
}

func diffFieldsContain(diffs []DispatchDiff, field string) bool {
	for _, diff := range diffs {
		if diff.Field == field {
			return true
		}
	}
	return false
}

func tokenDiffsContain(diffs []TokenDiff, field string) bool {
	for _, diff := range diffs {
		if diff.Field == field {
			return true
		}
	}
	return false
}
