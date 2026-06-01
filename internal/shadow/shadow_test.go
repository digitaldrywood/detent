package shadow

import (
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
					AssignedToWorker: boolPtr(true),
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

func boolPtr(value bool) *bool {
	return &value
}
