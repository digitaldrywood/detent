package operatortool

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestExplainItemSummary(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	in := explain.FromNativeEvidence(tracker.NativeRuntimeEvidence{
		Issue:            tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "project", Number: 1}, State: "In Progress", Title: "Example"},
		ObservedAt:       at,
		LatestTransition: &tracker.CollaborationEvent{RecordedAt: at, Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress", Reason: "claimed", ReasonDetail: "Worker started"}},
		Scheduling:       tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "Already running", At: at},
		Selection:        "current",
		Attempt:          &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt", RunnerID: "runner", Identity: &tracker.NativeExecutionIdentity{Role: "code"}}, Status: "running", StartedAt: at, RuntimeFreshness: "available"},
		CurrentVersion:   &tracker.NativeChangeVersionEvidence{Validation: &gate.CommandResult{Output: strings.Repeat("validation-output", 1000)}},
		Capacity:         []tracker.NativeRuntimeCapacity{{}},
		Unavailable:      []string{"coverage_notes"},
	})
	in.FailureSignature = &explain.FailureSignature{Signature: "failure", Count: 2}
	in.RequiredGate = explain.Gate{State: explain.GatePending, SourceState: explain.SourceAvailable, Reason: "Validation pending"}
	for _, test := range []struct {
		name, include string
		attempt       bool
		invalid       bool
	}{
		{"default", "", true, false},
		{"summary", `,"include":["summary"]`, true, false},
		{"missing attempt", `,"include":["summary"]`, false, false},
		{"unsupported include", `,"include":["capacity"]`, true, true},
		{"multiple includes", `,"include":["summary","summary"]`, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := in
			if !test.attempt {
				model.Attempt, model.LatestTransition, model.FailureSignature = nil, nil, nil
			}
			executor := NewExecutor(Dependencies{Explainer: staticExplainer{result: model}})
			result, err := executor.Execute(t.Context(), Call{Name: ExplainItem, Arguments: json.RawMessage(`{"project_id":"project","reference":"wi_example"` + test.include + `}`)})
			if test.invalid {
				if !errors.Is(err, ErrInvalidArguments) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.include == "" {
				var got explain.IssueExplanation
				if err := json.Unmarshal(result.Content, &got); err != nil || !reflect.DeepEqual(got, model) {
					t.Fatalf("default changed: %s error=%v", result.Content, err)
				}
				return
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(result.Content, &got); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"identity", "current_lane", "eligibility", "required_gate", "schema", "found", "observed_at"} {
				if got[field] == nil {
					t.Fatalf("missing %s", field)
				}
			}
			for _, field := range []string{"capacity", "native_runtime", "current_version", "validation-output", "coverage_notes"} {
				if strings.Contains(string(result.Content), field) {
					t.Fatalf("summary contains %s", field)
				}
			}
			var summary explanationSummary
			if err := json.Unmarshal(result.Content, &summary); err != nil {
				t.Fatal(err)
			}
			if summary.Identity.ProjectID != "project" || summary.CurrentLane.Name != "In Progress" || summary.Eligibility.Current.Outcome != "skipped" || summary.Eligibility.Current.Reason != "Already running" || summary.RequiredGate.State != explain.GatePending {
				t.Fatalf("lost triage fields: %s", result.Content)
			}
			if test.attempt {
				if len(got) != 10 || summary.Attempt == nil || summary.Attempt.RunnerID != "runner" || summary.Attempt.Role != "code" || summary.Attempt.Status != "running" || !summary.Attempt.StartedAt.Equal(at) || summary.FailureSignature.Count != 2 || summary.LatestTransition.Reason != "claimed" || summary.LatestTransition.ReasonDetail != "Worker started" {
					t.Fatalf("lost runtime summary: %s", result.Content)
				}
			} else if len(got) != 7 || got["attempt"] != nil || got["latest_transition"] != nil || got["failure_signature"] != nil {
				t.Fatalf("invented unavailable evidence: %s", result.Content)
			}
		})
	}
}
