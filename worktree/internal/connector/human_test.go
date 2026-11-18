package connector

import (
	"strings"
	"testing"
)

const humanBody = "```detent-human\nschema: 1\nkey: account-test\naction: Enable the test account\nowner: Account administrator\ncompletion_criteria: Test account can authenticate\napproval_constraint: Publishing requires separate approval\n"

func TestHumanTaskContract(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		body    string
		labels  []string
		closed  bool
		owned   bool
		ready   bool
		invalid bool
	}{
		{name: "ordinary software"},
		{name: "quoted example", body: "````markdown\n" + humanBody + "```\n````"},
		{name: "after example", body: "```text\nExample\n```\n" + humanBody + "```", owned: true},
		{name: "marker", body: humanBody + "```", owned: true},
		{name: "human label", labels: []string{" Human-Owned "}, owned: true},
		{name: "closed alone", body: humanBody + "```", closed: true, owned: true},
		{name: "evidence while open", body: humanBody + "completion_evidence: Admin verified authentication\n```", owned: true},
		{name: "completed", body: humanBody + "completion_evidence: Admin verified authentication\n```", closed: true, owned: true, ready: true},
		{name: "schema", body: strings.Replace(humanBody, "schema: 1", "schema: 2", 1) + "```", owned: true, invalid: true},
		{name: "missing criteria", body: "```detent-human\nschema: 1\n```", owned: true, invalid: true},
		{name: "unterminated", body: humanBody, owned: true, invalid: true},
		{name: "malformed fence", body: "```detent-human invalid\n```", owned: true, invalid: true},
		{name: "duplicate", body: humanBody + "```\n" + humanBody + "```", owned: true, invalid: true},
		{name: "unknown field", body: humanBody + "approval: granted\n```", owned: true, invalid: true},
		{name: "multiple documents", body: humanBody + "---\nschema: 1\n```", owned: true, invalid: true},
		{name: "tilde marker", body: strings.ReplaceAll(humanBody+"```", "```", "~~~"), owned: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := Issue{Description: tt.body, Labels: tt.labels, Closed: tt.closed, State: "Done"}
			if got := HumanOwned(issue); got != tt.owned {
				t.Fatalf("HumanOwned = %v", got)
			}
			if got := HumanPrerequisiteReady(issue); got != tt.ready {
				t.Fatalf("HumanPrerequisiteReady = %v", got)
			}
			_, _, err := ParseHumanTask(tt.body)
			if (err != nil) != tt.invalid {
				t.Fatalf("parse error = %v", err)
			}
			if got := NonExecutableReason(issue); (got == "human_owned") != tt.owned {
				t.Fatalf("exclusion = %q", got)
			}
		})
	}
}

func TestTrackingEpicExclusion(t *testing.T) {
	t.Parallel()
	for _, issue := range []Issue{{Title: "Epic: launch"}, {Labels: []string{"Epic"}}} {
		if got := NonExecutableReason(issue); got != "tracking_epic" {
			t.Fatalf("exclusion = %q", got)
		}
	}
}
