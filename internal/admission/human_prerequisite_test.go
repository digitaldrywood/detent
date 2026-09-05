package admission

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

func TestAutomaticAdmissionExcludesHumanTasksAndEpics(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		labels     []string
	}{
		{name: "human marker", body: "```detent-human\nschema: 1\n```"},
		{name: "human label", labels: []string{"human-owned"}},
		{name: "epic", labels: []string{"epic"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			issue := admissionIssueFixture("human", "owner/repo#10", 1, now)
			issue.Description = tt.body
			issue.Labels = tt.labels
			tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
			settings.Config.AutoAdmit = true
			manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })
			result, err := manager.RunOnce(t.Context())
			if err != nil || len(result.Proposals) != 0 {
				t.Fatalf("RunOnce = %+v, %v", result, err)
			}
			current, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(current) != 1 || current[0].State != "Backlog" {
				t.Fatalf("human/epic left Backlog: %+v, %v", current, err)
			}
			if admissionSourceEligible(issue, settings.Config) || autoAdmitCandidateEligible(issue, settings.Config) || eligibleCandidate(issue, settings.Config, settings.TerminalStates) {
				t.Fatal("manual or automatic admission bypassed exclusion")
			}
		})
	}
}

func TestHumanAdmissionDependencyRequiresEvidence(t *testing.T) {
	t.Parallel()
	for _, evidence := range []string{"", "Administrator verified authentication"} {
		body := fmt.Sprintf("```detent-human\nschema: 1\nkey: account\naction: Enable test account\nowner: Administrator\ncompletion_criteria: Authentication works\napproval_constraint: Publishing remains separately approved\ncompletion_evidence: %q\n```", evidence)
		tracker := memory.New(memory.Config{Issues: []connector.Issue{{ID: "human", Identifier: "owner/repo#10", Description: body, Closed: true, State: "Done"}}, Stateful: true})
		settings := Settings{Issues: tracker, TerminalStates: []string{"Done"}}
		got := resolveAdmissionDependencies(t.Context(), settings, connector.Issue{Identifier: "owner/repo#20", Description: "Depends on: owner/repo#10"}, time.Now())
		if got == nil || got.Ready != (evidence != "") {
			t.Fatalf("evidence=%q readiness=%+v", evidence, got)
		}
	}
}
